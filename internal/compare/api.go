package compare

import (
	"errors"
	"net/http"
	"sort"

	"wickfleet/internal/authz"
	"wickfleet/internal/httpx"
	"wickfleet/internal/registry"
)

// API serves /hub/api/compare.
type API struct {
	Service *Service // nil: comparisons are off
	Bots    func() []registry.Bot
	Demo    bool // read-only demo: earlier comparisons only
}

// Input picks the bots by name, or every bot the caller may replay.
type Input struct {
	Bots []string `json:"bots,omitempty"`
	All  bool     `json:"all,omitempty"`
	From string   `json:"from"`
	To   string   `json:"to"`
}

// Routes registers the comparison endpoints.
func (a API) Routes(mux httpx.Mux) {
	mux.HandleFunc("GET /hub/api/compare", a.list)
	mux.HandleFunc("GET /hub/api/compare/{id}", a.get)
	mux.HandleFunc("POST /hub/api/compare", a.post)
}

// visible: admins see every comparison, others one only when every bot in it is theirs.
func visible(id authz.Identity, c Comparison) bool {
	if id.Role == authz.Admin {
		return true
	}
	if id.Role < authz.Viewer || len(c.Rows) == 0 {
		return false
	}
	for _, r := range c.Rows {
		if r.Owner != id.User {
			return false
		}
	}
	return true
}

// mayReplay is the replay rule: operators their own bots, admins any.
func mayReplay(id authz.Identity, b registry.Bot) bool {
	return id.Role == authz.Admin || (id.Role >= authz.Operator && b.Owner == id.User)
}

func (a API) enabled() bool { return a.Service != nil && a.Service.Replays != nil }

func (a API) list(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		httpx.Fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	out := []Comparison{}
	maxBots := 0
	if a.Service != nil {
		for _, c := range a.Service.List() {
			if visible(id, c) {
				out = append(out, view(id, c.Brief()))
			}
		}
		maxBots = a.Service.MaxBots
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"enabled": a.enabled(), "demo": a.Demo, "max_bots": maxBots,
		"can_run": a.enabled() && !a.Demo && id.Role >= authz.Operator, "comparisons": out,
	})
}

func (a API) get(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		httpx.Fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	if a.Service == nil {
		httpx.Fail(w, http.StatusNotFound, "no such comparison")
		return
	}
	c, ok := a.Service.Get(r.PathValue("id"))
	if !ok || !visible(id, c) {
		httpx.Fail(w, http.StatusNotFound, "no such comparison")
		return
	}
	httpx.JSON(w, http.StatusOK, view(id, c))
}

// view hides money from viewers, the demo included.
func view(id authz.Identity, c Comparison) Comparison {
	if id.Role < authz.Operator {
		return c.Redact()
	}
	return c
}

func (a API) post(w http.ResponseWriter, r *http.Request) {
	var in Input
	id, ok := httpx.Guard(w, r, authz.Operator, "operators and admins only", &in)
	if !ok {
		return
	}
	if a.Demo {
		httpx.Fail(w, http.StatusForbidden, "read-only demo")
		return
	}
	if !a.enabled() {
		httpx.Fail(w, http.StatusServiceUnavailable, "replay is off on this instance")
		return
	}
	var bots []registry.Bot
	switch {
	case in.All && len(in.Bots) == 0:
		// Managed bots belong to their controller. "all" means the caller's fleet.
		for _, b := range a.Bots() {
			if b.Managed == "" && mayReplay(id, b) {
				bots = append(bots, b)
			}
		}
		sort.Slice(bots, func(i, j int) bool { return bots[i].Name < bots[j].Name })
	case !in.All && len(in.Bots) > 0:
		seen := map[string]bool{}
		for _, name := range in.Bots {
			if seen[name] {
				continue
			}
			seen[name] = true
			b, found := registry.Find(a.Bots(), name)
			if !found || !mayReplay(id, b) {
				httpx.Fail(w, http.StatusNotFound, "no such bot: "+name)
				return
			}
			bots = append(bots, b)
		}
	default:
		httpx.Fail(w, http.StatusBadRequest, "give either bots or all")
		return
	}
	c, err := a.Service.Submit(id.User, bots, in.From, in.To)
	if errors.Is(err, ErrBusy) {
		httpx.Fail(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, err.Error())
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]string{"id": c.ID, "state": c.State})
}
