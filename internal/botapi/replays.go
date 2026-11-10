package botapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"wickfleet/internal/authz"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

// Replays is what the API needs from the replay runner.
type Replays interface {
	Submit(owner string, b registry.Bot, from, to string) (*replay.Job, error)
	List() []replay.Job
	Get(id string) (replay.Job, bool)
	Trades(id string) ([]replay.Trade, error)
	Passed(owner, settings string, within time.Duration) (replay.Job, bool)
}

// ReplayWindow is how old a replay may be and still let its bot start.
const ReplayWindow = 7 * 24 * time.Hour

// ReplayInput names an existing bot, or carries a draft the way PUT does.
type ReplayInput struct {
	Bot   string `json:"bot,omitempty"`
	Draft *Input `json:"draft,omitempty"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// ReplayRoutes registers /hub/api/replays.
func (a API) ReplayRoutes(mux Mux) {
	mux.HandleFunc("GET /hub/api/replays", a.replayList)
	mux.HandleFunc("GET /hub/api/replays/{id}", a.replayGet)
	mux.HandleFunc("POST /hub/api/replays", a.replayPost)
}

// Admins see every job, store fetches included; others their own replays.
func replayVisible(id authz.Identity, j replay.Job) bool {
	return id.Role == authz.Admin || (id.Role >= authz.Viewer && j.Kind == "replay" && j.Owner == id.User)
}

func (a API) replayList(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	out := []replay.Job{}
	if a.Replays != nil {
		for _, j := range a.Replays.List() {
			if replayVisible(id, j) {
				out = append(out, j)
			}
		}
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"enabled": a.Replays != nil, "since": a.ReplaySince, "since_by_exchange": a.ReplaySinceBy,
		"can_run": id.Role >= authz.Operator, "jobs": out,
	})
}

func (a API) replayGet(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if a.Replays == nil {
		fail(w, http.StatusNotFound, "replay is off on this instance")
		return
	}
	j, ok := a.Replays.Get(r.PathValue("id"))
	if !ok || !replayVisible(id, j) {
		fail(w, http.StatusNotFound, "no such replay")
		return
	}
	out := map[string]any{"job": j}
	if j.State == replay.Done && j.Kind == "replay" {
		if trades, err := a.Replays.Trades(j.ID); err == nil {
			out["trades"] = trades
		}
	}
	jsonOut(w, http.StatusOK, out)
}

func (a API) replayPost(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) != "1" {
		fail(w, http.StatusForbidden, "missing "+CSRFHeader)
		return
	}
	id := authz.FromHeaders(r.Header)
	if id.Role < authz.Operator {
		fail(w, http.StatusForbidden, "operators and admins only")
		return
	}
	if a.Replays == nil {
		fail(w, http.StatusServiceUnavailable, "replay is off on this instance")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "JSON only")
		return
	}
	var in ReplayInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	var b registry.Bot
	switch {
	case in.Bot != "" && in.Draft == nil:
		var ok bool
		b, ok = registry.Find(a.Bots(), in.Bot)
		if !ok || (id.Role != authz.Admin && b.Owner != id.User) {
			fail(w, http.StatusNotFound, "no such bot")
			return
		}
	case in.Bot == "" && in.Draft != nil:
		d := *in.Draft
		d.Owner = id.User
		var err error
		if _, b, err = a.render("draft", d, nil); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	default:
		fail(w, http.StatusBadRequest, "give either bot or draft")
		return
	}
	j, err := a.Replays.Submit(id.User, b, in.From, in.To)
	if errors.Is(err, replay.ErrBusy) {
		fail(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOut(w, http.StatusAccepted, map[string]string{"id": j.ID, "state": j.State})
}
