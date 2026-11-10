// Package botapi lets the UI list, create, change and delete bots. A change
// is a commit to the registry repo; the reconciler does the rest.
package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"wickfleet/internal/authz"
	"wickfleet/internal/gitstore"
	"wickfleet/internal/httpx"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

// CSRFHeader must be on every write. A custom header forces a CORS
// preflight, so another site cannot send one with the user's cookie.
const CSRFHeader = httpx.CSRFHeader

// Writer commits registry changes and reads the current head.
type Writer interface {
	Change(ctx context.Context, rel string, data []byte, msg, author string) error
	ChangeAll(ctx context.Context, files map[string][]byte, msg, author string) error
	Read(ctx context.Context, rel string) ([]byte, error)
}

// API serves /hub/api/bots and /hub/api/strategies.
type API struct {
	Bots             func() []registry.Bot
	RegistryDir      string // read-only checkout: defaults.yaml, strategies/
	Images           []string
	Store            Writer
	Watchlist        func() registry.Watchlist // the list signals run on
	Replays          Replays                   // nil = replay off
	ReplaySince      string                    // first day of the candle store, YYYY-MM-DD
	ReplaySinceBy    map[string]string         // per exchange, lower case; overrides ReplaySince
	Drafts           *Drafts                   // nil = no sentence-to-spec drafting
	AdminCreatesOnly bool                      // only admins create bots on this instance
	ReservedAuthor   string                    // strategies by this author change only through it
	Live             bool                      // WICKFLEET_LIVE_TRADING: a bot may set dry_run: false
}

// Input is what the UI may set. Name comes from the path; owner from the
// caller unless an admin sets it.
type Input struct {
	Owner         string   `json:"owner,omitempty" yaml:"owner"`
	Strategy      string   `json:"strategy" yaml:"strategy"`
	StrategyFile  string   `json:"strategy_file,omitempty" yaml:"strategy_file,omitempty"`
	Exchange      string   `json:"exchange,omitempty" yaml:"exchange,omitempty"`
	Pairs         []string `json:"pairs,omitempty" yaml:"pairs,omitempty"`
	StakeCurrency string   `json:"stake_currency,omitempty" yaml:"stake_currency,omitempty"`
	Wallet        float64  `json:"wallet,omitempty" yaml:"wallet,omitempty"`
	MaxOpenTrades int      `json:"max_open_trades,omitempty" yaml:"max_open_trades,omitempty"`
	Fee           *float64 `json:"fee,omitempty" yaml:"fee,omitempty"`
	Fiat          *string  `json:"fiat,omitempty" yaml:"fiat,omitempty"`
	Image         string   `json:"image,omitempty" yaml:"image,omitempty"`
	MemLimit      string   `json:"mem_limit,omitempty" yaml:"mem_limit,omitempty"`
	DryRun        *bool    `json:"dry_run,omitempty" yaml:"dry_run,omitempty"`
}

// View is one bot in a listing.
type View struct {
	registry.Bot
	URL      string `json:"url"`
	Editable bool   `json:"editable"`
}

func visible(id authz.Identity, b registry.Bot) bool {
	return id.Role == authz.Admin || (id.Role >= authz.Viewer && id.User == b.Owner)
}

func editable(id authz.Identity, b registry.Bot) bool {
	return id.Role == authz.Admin || (id.Role >= authz.Operator && id.User == b.Owner)
}

var jsonOut, fail = httpx.JSON, httpx.Fail

// Mux is where routes go; *http.ServeMux is one, a test recorder another.
type Mux = httpx.Mux

// Routes registers the handlers on mux (paths under /hub/api/).
func (a API) Routes(mux Mux) {
	mux.HandleFunc("GET /hub/api/bots", a.list)
	mux.HandleFunc("GET /hub/api/strategies", a.strategies)
	mux.HandleFunc("PUT /hub/api/bots/{name}", a.put)
	mux.HandleFunc("DELETE /hub/api/bots/{name}", a.del)
	a.WatchRoutes(mux)
	a.ReplayRoutes(mux)
	a.SpecRoutes(mux)
}

func (a API) list(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	out := []View{}
	for _, b := range a.Bots() {
		if visible(id, b) {
			out = append(out, View{Bot: b, URL: "/bots/" + b.Name, Editable: editable(id, b)})
		}
	}
	jsonOut(w, http.StatusOK, map[string]any{"bots": out, "role": id.Role.String(), "user": id.User, "images": a.Images})
}

func (a API) strategies(w http.ResponseWriter, r *http.Request) {
	if authz.FromHeaders(r.Header).Role == authz.None {
		fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	files, _ := filepath.Glob(filepath.Join(a.RegistryDir, "strategies", "*.py"))
	names := []string{}
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	jsonOut(w, http.StatusOK, map[string]any{"strategies": names})
}

func (a API) writeGuard(w http.ResponseWriter, r *http.Request) (authz.Identity, string, bool) {
	if r.Header.Get(CSRFHeader) != "1" {
		fail(w, http.StatusForbidden, "missing "+CSRFHeader)
		return authz.Identity{}, "", false
	}
	id := authz.FromHeaders(r.Header)
	if id.Role < authz.Operator {
		fail(w, http.StatusForbidden, "operators and admins only")
		return id, "", false
	}
	name := r.PathValue("name")
	if !registry.ValidName(name) {
		fail(w, http.StatusBadRequest, "bad bot name")
		return id, "", false
	}
	if a.Store == nil {
		fail(w, http.StatusServiceUnavailable, "registry writes are not configured")
		return id, "", false
	}
	return id, name, true
}

func (a API) put(w http.ResponseWriter, r *http.Request) {
	id, name, ok := a.writeGuard(w, r)
	if !ok {
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "JSON only")
		return
	}
	var in Input
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	existing, exists := registry.Find(a.Bots(), name)
	// The head decides: a managed bot may land there before the checkout syncs.
	var base map[string]any
	if raw, err := a.Store.Read(r.Context(), "bots/"+name+".yaml"); err == nil {
		_ = yaml.Unmarshal(raw, &base)
	}
	switch {
	case existing.Managed != "" || (base["arena"] != nil && base["arena"] != ""):
		fail(w, http.StatusForbidden, "managed bots change only through their controller")
		return
	case !exists && a.AdminCreatesOnly && id.Role != authz.Admin:
		fail(w, http.StatusForbidden, "only admins create bots on this instance")
		return
	}
	if exists && !editable(id, existing) {
		fail(w, http.StatusForbidden, "not your bot")
		return
	}
	switch {
	case id.Role != authz.Admin:
		in.Owner = id.User // operators always own what they make
	case in.Owner == "" && exists:
		in.Owner = existing.Owner
	case in.Owner == "":
		in.Owner = id.User
	}
	if in.Image != "" && !contains(a.Images, in.Image) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("image %q is not allowed", in.Image))
		return
	}
	if !exists {
		base = nil // an edit keeps what the UI cannot express; a create starts clean
	}
	data, resolved, err := a.render(name, in, base)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// A new bot starts only after a replay of exactly these settings.
	if !exists && a.Replays != nil {
		if _, ok := a.Replays.Passed(id.User, replay.SettingsHash(resolved), ReplayWindow); !ok {
			jsonOut(w, http.StatusPreconditionFailed, map[string]string{
				"error":    "replay these settings first (last 7 days, no lookahead bias found), then save",
				"settings": replay.SettingsHash(resolved),
			})
			return
		}
	}
	// Real money only behind a passing robustness verdict of exactly these settings.
	if !resolved.Dry() {
		if !a.liveReady(w, id.User, resolved) {
			return
		}
	}
	verb := "creates"
	if exists {
		verb = "changes"
	}
	err = a.Store.Change(r.Context(), "bots/"+name+".yaml", data,
		fmt.Sprintf("feat: %s %s bot %s", id.User, verb, name), author(id))
	if errors.Is(err, gitstore.ErrNoChange) {
		jsonOut(w, http.StatusOK, map[string]string{"result": "unchanged"})
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "commit failed")
		return
	}
	code := http.StatusAccepted
	jsonOut(w, code, map[string]string{"result": verb, "note": "applied after the next sync, about a minute"})
}

func (a API) del(w http.ResponseWriter, r *http.Request) {
	id, name, ok := a.writeGuard(w, r)
	if !ok {
		return
	}
	b, exists := registry.Find(a.Bots(), name)
	if !exists {
		fail(w, http.StatusNotFound, "no such bot")
		return
	}
	if b.Managed != "" {
		fail(w, http.StatusForbidden, "managed bots change only through their controller")
		return
	}
	if !editable(id, b) {
		fail(w, http.StatusForbidden, "not your bot")
		return
	}
	err := a.Store.Change(r.Context(), "bots/"+name+".yaml", nil, fmt.Sprintf("feat: %s removes bot %s", id.User, name), author(id))
	if err != nil && !errors.Is(err, gitstore.ErrNoChange) {
		fail(w, http.StatusBadGateway, "commit failed")
		return
	}
	jsonOut(w, http.StatusAccepted, map[string]string{"result": "removed", "note": "the container goes after the next sync; its data stays"})
}

func author(id authz.Identity) string {
	return fmt.Sprintf("%s <%s@wickfleet.invalid>", id.User, id.User)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// render writes the bot file and checks it with the same loader the
// reconciler uses, against the real defaults and strategies.
func (a API) render(name string, in Input, base map[string]any) ([]byte, registry.Bot, error) {
	type file struct {
		Name  string `yaml:"name"`
		Input `yaml:",inline"`
	}
	raw, err := yaml.Marshal(file{Name: name, Input: in})
	if err != nil {
		return nil, registry.Bot{}, err
	}
	merged := map[string]any{}
	for k, v := range base {
		merged[k] = v
	}
	var own map[string]any
	if err := yaml.Unmarshal(raw, &own); err != nil {
		return nil, registry.Bot{}, err
	}
	for k, v := range own {
		merged[k] = v
	}
	merged["name"] = name
	data, err := yaml.Marshal(merged)
	if err != nil {
		return nil, registry.Bot{}, err
	}
	tmp, err := os.MkdirTemp("", "wickfleet-validate-")
	if err != nil {
		return nil, registry.Bot{}, err
	}
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "bots"), 0o700); err != nil {
		return nil, registry.Bot{}, err
	}
	if d, err := os.ReadFile(filepath.Join(a.RegistryDir, "defaults.yaml")); err == nil {
		if err := os.WriteFile(filepath.Join(tmp, "defaults.yaml"), d, 0o600); err != nil {
			return nil, registry.Bot{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "bots", name+".yaml"), data, 0o600); err != nil {
		return nil, registry.Bot{}, err
	}
	bots, err := registry.LoadWith(tmp, registry.Policy{AllowLive: a.Live})
	if err != nil {
		return nil, registry.Bot{}, err
	}
	b := bots[0]
	if b.StrategyFile != "" {
		if _, err := os.Stat(filepath.Join(a.RegistryDir, "strategies", b.StrategyFile)); err != nil {
			return nil, registry.Bot{}, fmt.Errorf("strategy file %s is not in the registry", b.StrategyFile)
		}
		// A spec with a model only runs on a FreqAI bot.
		if s, ok, _ := a.readSpec(strings.TrimSuffix(b.StrategyFile, ".py")); ok && s.Model != nil {
			if _, fq := b.ConfigExtra["freqai"]; b.FreqaiModel == "" || !fq {
				return nil, registry.Bot{}, fmt.Errorf("strategy %s uses a FreqAI model; the bot needs freqaimodel and a freqai config", s.Name)
			}
		}
	}
	if !contains(a.Images, b.Image) {
		return nil, registry.Bot{}, fmt.Errorf("image %q is not allowed", b.Image)
	}
	return data, b, nil
}

// liveReady checks a live bot: the instance allows it and a recent replay of
// these settings passed the bias checks and the robustness verdict.
func (a API) liveReady(w http.ResponseWriter, owner string, b registry.Bot) bool {
	if !a.Live {
		fail(w, http.StatusForbidden, "live trading is not allowed on this instance")
		return false
	}
	if a.Replays == nil {
		fail(w, http.StatusPreconditionFailed, "live trading needs replay to check the strategy first")
		return false
	}
	job, ok := a.Replays.Passed(owner, replay.SettingsHash(b), ReplayWindow)
	if !ok || job.Robust == nil || !job.Robust.Pass {
		jsonOut(w, http.StatusPreconditionFailed, map[string]string{
			"error":    "live trading needs a replay of these settings that passes the robustness verdict",
			"settings": replay.SettingsHash(b),
		})
		return false
	}
	return true
}
