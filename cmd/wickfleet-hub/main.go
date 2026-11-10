// wickfleet-hub: ForwardAuth for the bots, fleet totals, metrics and
// notifications. Config comes from WICKFLEET_* env vars.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // the image has no zoneinfo; signals need America/Toronto

	"wickfleet/internal/atomicfile"
	"wickfleet/internal/authz"
	"wickfleet/internal/botapi"
	"wickfleet/internal/demo"
	"wickfleet/internal/docker"
	"wickfleet/internal/drafter"
	"wickfleet/internal/fleet"
	"wickfleet/internal/freqtrade"
	"wickfleet/internal/gitstore"
	"wickfleet/internal/login"
	"wickfleet/internal/notify"
	"wickfleet/internal/reconcile"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
	"wickfleet/internal/signals"
	"wickfleet/internal/watch"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type config struct {
	Listen       string
	Live         bool   // WICKFLEET_LIVE_TRADING=true: dry_run: false bots may trade, off by default
	KeysDir      string // WICKFLEET_EXCHANGE_KEYS_DIR: <bot>.json exchange keys, read only when Live
	RegistryDir  string
	SecretsDir   string
	BotURL       string // fmt pattern, %s is the bot name
	TelegramFile string
	TelegramChat string
	CacheFor     time.Duration
	Signals      []string // fallback watchlist when the registry has no watchlist.yaml
	SignalMode   signals.Mode
	StateDir     string
	AuthzKey     string // from WICKFLEET_AUTHZ_KEY_FILE; empty = no check
	Auth         string // WICKFLEET_AUTH: proxy (oauth2-proxy headers), builtin (internal/login) or demo (internal/demo)
	MetricsAddr  string // separate /metrics listener; empty = on the main one

	// Reconcile: the hub runs the bots itself when DockerURL is set.
	DockerURL string
	Images    map[string]string
	DataDir   string
	BotState  string
	Network   string
	Host      string
	UIBot     string
	BotUser   string

	// Registry writes (UI create/edit/delete) as commits with a write deploy key.
	WriteURL   string
	WriteKey   string
	KnownHosts string
	WorkDir    string

	// Replay: backtests over a shared candle store; off when CandleDir is empty.
	CandleDir   string
	ReplayDir   string
	ReplaySince string
	ReplayMemMB int64
	FreqaiMemMB int64
	PracticeMB  int64 // per practice run (Learn)

	// Strategy drafting from a sentence; providers in this order, each optional.
	DraftOllama       string
	DraftModel        string
	DraftAnthropicKey string // file
	DraftClaudeModel  string
}

// parseImages reads "key=ref,key=ref".
func parseImages(s string) map[string]string {
	out := map[string]string{}
	for _, p := range splitList(s) {
		if k, v, ok := strings.Cut(p, "="); ok && k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func loadConfig() config {
	return config{
		Listen:       env("WICKFLEET_LISTEN", ":8090"),
		Live:         os.Getenv("WICKFLEET_LIVE_TRADING") == "true",
		Auth:         env("WICKFLEET_AUTH", "proxy"),
		KeysDir:      os.Getenv("WICKFLEET_EXCHANGE_KEYS_DIR"),
		RegistryDir:  env("WICKFLEET_REGISTRY_DIR", "/registry"),
		SecretsDir:   env("WICKFLEET_SECRETS_DIR", "/secrets"),
		BotURL:       env("WICKFLEET_BOT_URL", "http://bot-%s:8080"),
		TelegramFile: os.Getenv("WICKFLEET_TELEGRAM_TOKEN_FILE"),
		TelegramChat: os.Getenv("WICKFLEET_TELEGRAM_CHAT_ID"),
		CacheFor:     30 * time.Second,
		Signals:      splitList(os.Getenv("WICKFLEET_SIGNALS")),
		SignalMode:   signals.Mode{Taxable: os.Getenv("WICKFLEET_SIGNAL_ACCOUNT") == "taxable", Parking: os.Getenv("WICKFLEET_SIGNAL_PARKING")},
		StateDir:     env("WICKFLEET_STATE_DIR", "/state"),
		MetricsAddr:  os.Getenv("WICKFLEET_METRICS_LISTEN"),
		DockerURL:    os.Getenv("WICKFLEET_DOCKER"),
		Images:       parseImages(os.Getenv("WICKFLEET_IMAGES")),
		DataDir:      env("WICKFLEET_DATA_DIR", "/var/lib/wickfleet/bots"),
		BotState:     env("WICKFLEET_BOT_STATE", "/var/lib/wickfleet/state"),
		Network:      env("WICKFLEET_NETWORK", "wickfleet_edge"),
		Host:         os.Getenv("WICKFLEET_HOST"),
		UIBot:        os.Getenv("WICKFLEET_UI_BOT"),
		BotUser:      env("WICKFLEET_BOT_USER", "1000:1000"),
		WriteURL:     os.Getenv("WICKFLEET_REGISTRY_WRITE_URL"),
		WriteKey:     os.Getenv("WICKFLEET_REGISTRY_WRITE_KEY"),
		KnownHosts:   os.Getenv("WICKFLEET_REGISTRY_KNOWN_HOSTS"),
		WorkDir:      env("WICKFLEET_REGISTRY_WORKDIR", "/var/lib/wickfleet/state/registry-rw"),
		CandleDir:    os.Getenv("WICKFLEET_CANDLE_DIR"),
		ReplayDir:    env("WICKFLEET_REPLAY_DIR", "/var/lib/wickfleet/state/replays"),
		ReplaySince:  env("WICKFLEET_REPLAY_SINCE", "2025-01-01"),
		ReplayMemMB:  envInt("WICKFLEET_REPLAY_MEM_MB", 1536),
		FreqaiMemMB:  envInt("WICKFLEET_REPLAY_FREQAI_MEM_MB", 3072),
		PracticeMB:   envInt("WICKFLEET_PRACTICE_MEM_MB", 1024),

		DraftOllama:       os.Getenv("WICKFLEET_DRAFT_OLLAMA"),
		DraftModel:        os.Getenv("WICKFLEET_DRAFT_MODEL"),
		DraftAnthropicKey: os.Getenv("WICKFLEET_DRAFT_ANTHROPIC_KEY_FILE"),
		DraftClaudeModel:  env("WICKFLEET_DRAFT_ANTHROPIC_MODEL", "claude-opus-5-5"),
	}
}

func envInt(k string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil && v > 0 {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// hub holds the last good registry and a short fleet cache.
type hub struct {
	cfg config

	mu       sync.RWMutex
	bots     []registry.Bot
	loaded   bool
	watch    registry.Watchlist
	watchSet bool // false: no watchlist.yaml, WICKFLEET_SIGNALS applies

	fmu     sync.Mutex
	report  fleet.Report
	reportT time.Time
	waiting []watch.Result
	waitT   time.Time

	smu  sync.RWMutex
	sigs []signals.Signal

	rec  *reconcile.Reconciler
	rmu  sync.RWMutex
	last reconcile.Result

	replays *replay.Runner
	login   *login.Service // nil in proxy mode

	dmu       sync.Mutex
	draftJobs *botapi.Drafts
	store     *gitstore.Store

	ext extension // nil: the plain bot manager
}

// extension is an optional part a private build adds in its own file: more
// routes, a front page and limits for non-admins. A public build has none.
type extension interface {
	routes(api botapi.Mux)
	front() http.Handler                // nil: no front page
	me(user string, out map[string]any) // extra /hub/api/me fields
	openPrefixes() []string             // nil: non-admins reach every route
	adminCreatesOnly() bool
	reservedAuthor() string
}

// newExtension starts the extension, or returns nil when its own config leaves it off.
var newExtension func(h *hub, sender notify.Sender) (extension, error)

// writer is the one working clone for registry writes; nil when writes are off.
func (h *hub) writer() *gitstore.Store {
	h.dmu.Lock()
	defer h.dmu.Unlock()
	if h.store == nil && h.cfg.WriteURL != "" {
		h.store = &gitstore.Store{URL: h.cfg.WriteURL, Dir: h.cfg.WorkDir, KeyFile: h.cfg.WriteKey, KnownHosts: h.cfg.KnownHosts}
	}
	return h.store
}

// reconcileOnce applies the registry; logs only when something moved.
func (h *hub) reconcileOnce(ctx context.Context) {
	h.mu.RLock()
	loaded := h.loaded
	h.mu.RUnlock()
	if h.rec == nil || !loaded {
		return
	}
	res := h.rec.Run(ctx, h.Bots())
	h.rmu.Lock()
	h.last = res
	h.rmu.Unlock()
	if len(res.Created)+len(res.Replaced)+len(res.Removed)+len(res.Errors) > 0 {
		log.Printf("reconcile: created=%v replaced=%v removed=%v errors=%v", res.Created, res.Replaced, res.Removed, res.Errors)
	}
}

// signalState is what was last reported, so each month goes out once.
type signalState struct {
	Month string          `json:"month"`
	Hold  map[string]bool `json:"hold"`
}

// checkSignals evaluates the monthly rule and sends one summary per new
// completed month. Returns whether it sent.
func (h *hub) checkSignals(ctx context.Context, src signals.Source, sender notify.Sender, now time.Time, loc *time.Location) (bool, error) {
	symbols := h.Watch().Names()
	sigs := signals.Evaluate(ctx, src, symbols, now, loc)
	h.smu.Lock()
	h.sigs = sigs
	h.smu.Unlock()
	if len(symbols) == 0 {
		return false, nil
	}
	month := ""
	for _, s := range sigs {
		if s.Error == "" {
			month = s.Month
			break
		}
	}
	if month == "" {
		return false, errors.New("no symbol produced a signal")
	}
	path := filepath.Join(h.cfg.StateDir, "signals.json")
	var st signalState
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Month >= month {
		return false, nil
	}
	if err := sender.Send(ctx, signals.Summary(sigs, st.Hold, h.cfg.SignalMode)); err != nil {
		return false, err
	}
	next := signalState{Month: month, Hold: map[string]bool{}}
	for _, s := range sigs {
		if s.Error == "" {
			next.Hold[s.Symbol] = s.Hold
		}
	}
	raw, _ := json.Marshal(next)
	return true, atomicfile.Write(path, raw, 0o600)
}

func (h *hub) reload() error {
	bots, err := registry.LoadWith(h.cfg.RegistryDir, registry.Policy{AllowLive: h.cfg.Live})
	if err != nil {
		return err
	}
	// A broken watchlist keeps the last good one; it must not hold up the bots.
	wl, ok, werr := registry.LoadWatchlist(h.cfg.RegistryDir)
	if werr != nil {
		log.Printf("watchlist, keeping the last good one: %v", werr)
	}
	h.mu.Lock()
	h.bots, h.loaded = bots, true
	if werr == nil {
		h.watch, h.watchSet = wl, ok
	}
	h.mu.Unlock()
	return nil
}

// Watch is the list signals run on: watchlist.yaml, else WICKFLEET_SIGNALS.
func (h *hub) Watch() registry.Watchlist {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.watchSet {
		return h.watch
	}
	var wl registry.Watchlist
	for _, s := range h.cfg.Signals {
		wl.Symbols = append(wl.Symbols, registry.Watch{Symbol: s})
	}
	return wl
}

// drafts keeps one job store for the hub's life; routes may be built twice in tests.
func (h *hub) drafts(d drafter.Drafter) *botapi.Drafts {
	h.dmu.Lock()
	defer h.dmu.Unlock()
	if h.draftJobs == nil {
		h.draftJobs = botapi.NewDrafts(d)
	}
	return h.draftJobs
}

// drafter builds the providers from config: Ollama first, Anthropic if a key file is set.
func (h *hub) drafter() drafter.Drafter {
	var d drafter.Drafter
	if h.cfg.DraftOllama != "" && h.cfg.DraftModel != "" {
		d.Providers = append(d.Providers, drafter.Ollama{Base: h.cfg.DraftOllama, Model: h.cfg.DraftModel})
	}
	if f := h.cfg.DraftAnthropicKey; f != "" {
		key, err := os.ReadFile(f)
		if err != nil || len(strings.TrimSpace(string(key))) == 0 {
			log.Printf("draft: anthropic key file %s unusable, skipping", f)
		} else {
			d.Providers = append(d.Providers, drafter.Anthropic{Key: strings.TrimSpace(string(key)), Model: h.cfg.DraftClaudeModel})
		}
	}
	return d
}

// warmStore queues one candle fetch per exchange the bots use, covering all
// their pairs and timeframes, so a replay finds its data already there.
func (h *hub) warmStore() {
	type want struct {
		image      string
		pairs, tfs map[string]bool
	}
	by := map[string]*want{}
	for _, b := range h.Bots() {
		if b.StrategyFile == "" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(h.cfg.RegistryDir, "strategies", b.StrategyFile))
		if err != nil {
			continue
		}
		tfs, err := replay.Timeframes(b, src)
		if err != nil {
			log.Printf("candle store: %v", err)
			continue
		}
		w, ok := by[b.Exchange]
		if !ok {
			w = &want{image: b.Image, pairs: map[string]bool{}, tfs: map[string]bool{}}
			by[b.Exchange] = w
		}
		if b.FreqaiModel == "" {
			w.image = b.Image // the plain image is enough to download
		}
		for _, p := range replay.Pairs(b) {
			w.pairs[p] = true
		}
		for _, tf := range tfs {
			w.tfs[tf] = true
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for ex, w := range by {
		if h.replays.FetchPending(ex) {
			continue
		}
		if _, err := h.replays.Fetch(ex, w.image, keys(w.pairs), keys(w.tfs)); err != nil {
			log.Printf("candle store %s: %v", ex, err)
		}
	}
}

func (h *hub) Bots() []registry.Bot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.bots
}

func (h *hub) secret(bot string) (string, error) {
	if !registry.ValidName(bot) {
		return "", errors.New("bad bot name")
	}
	if h.rec != nil {
		s, err := h.rec.Secrets(bot)
		return s.APIPassword, err
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.SecretsDir, bot+".api.password"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (h *hub) source(b registry.Bot) fleet.Source {
	pw, err := h.secret(b.Name)
	if err != nil {
		pw = ""
	}
	return freqtrade.Client{Base: fmt.Sprintf(h.cfg.BotURL, b.Name), User: b.Name, Pass: pw}
}

func (h *hub) fleet(ctx context.Context) fleet.Report {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	if time.Since(h.reportT) < h.cfg.CacheFor {
		return h.report
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h.report = fleet.Collect(ctx, h.Bots(), h.source)
	h.reportT = time.Now()
	return h.report
}

// watchTargets pairs each bot that has watch rules with its freqtrade client.
func (h *hub) watchTargets() []watch.Target {
	var ts []watch.Target
	for _, b := range h.Bots() {
		if len(b.Watch) == 0 {
			continue
		}
		pw, _ := h.secret(b.Name)
		ts = append(ts, watch.Target{Bot: b.Name, Owner: b.Owner, Pairs: b.Pairs, Rules: b.Watch, Config: b.ConfigExtra,
			Source: freqtrade.Client{Base: fmt.Sprintf(h.cfg.BotURL, b.Name), User: b.Name, Pass: pw}})
	}
	return ts
}

// watching is what each bot's entry rules say on the last closed candle, cached like the fleet report.
func (h *hub) watching(ctx context.Context) []watch.Result {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	if time.Since(h.waitT) < h.cfg.CacheFor {
		return h.waiting
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h.waiting = watch.Collect(ctx, h.watchTargets())
	h.waitT = time.Now()
	return h.waiting
}

// visibleWatch keeps the results an identity may see.
func visibleWatch(id authz.Identity, rs []watch.Result) []watch.Result {
	if id.Role == authz.Admin {
		return rs
	}
	var mine []watch.Result
	for _, r := range rs {
		if r.Owner == id.User {
			mine = append(mine, r)
		}
	}
	return mine
}

// visible trims a report to what the identity may see.
func visible(id authz.Identity, r fleet.Report) fleet.Report {
	if id.Role == authz.Admin {
		return r
	}
	var mine []fleet.BotStats
	for _, b := range r.Bots {
		if b.Owner == id.User {
			mine = append(mine, b)
		}
	}
	return fleet.Report{At: r.At, Bots: mine, Totals: fleet.Sum(mine)}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *hub) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fleet.WriteMetrics(w, h.fleet(r.Context()))
	watch.WriteMetrics(w, h.watching(r.Context()))
}

// metricsRoutes is the scrape-only listener: no identity, no keys.
func (h *hub) metricsRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", h.metrics)
	return mux
}

func (h *hub) routes(sender notify.Sender) http.Handler {
	mux := http.NewServeMux()
	key := h.cfg.AuthzKey
	var authzH http.Handler = authz.Handler{Bots: h.Bots, Secret: h.secret}
	if h.login != nil {
		h.login.Routes(mux)
		authzH = login.ToLogin(authzH)
	}
	mux.Handle("/authz", authz.RequireKey(key, authzH))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		ok := h.loaded
		h.mu.RUnlock()
		if !ok {
			http.Error(w, "registry not loaded", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	if h.cfg.MetricsAddr == "" {
		mux.HandleFunc("/metrics", h.metrics)
	}
	api := http.NewServeMux()
	var apiH http.Handler = api
	if h.ext != nil && h.ext.openPrefixes() != nil {
		apiH = authz.OpenPrefixes(api, h.ext.openPrefixes()...)
	}
	mux.Handle("/hub/api/", authz.RequireKey(key, apiH))
	h.apiRoutes(api)
	mux.Handle("/hub/webhook/", notify.Webhook{Bots: h.Bots, Sender: sender, Log: log.Printf})
	if h.ext != nil && h.ext.front() != nil {
		mux.Handle("/", h.ext.front()) // static files only; the data is /hub/api
	}
	if h.login != nil {
		return h.login.Wrap(mux)
	}
	if h.cfg.Auth == "demo" {
		return demo.Wrap(mux)
	}
	return mux
}

// apiRoutes registers every /hub/api/ route on api.
func (h *hub) apiRoutes(api botapi.Mux) {
	images := make([]string, 0, len(h.cfg.Images))
	for k := range h.cfg.Images {
		images = append(images, k)
	}
	sort.Strings(images)
	bapi := botapi.API{Bots: h.Bots, RegistryDir: h.cfg.RegistryDir, Images: images, Watchlist: h.Watch, Live: h.cfg.Live}
	if h.ext != nil {
		bapi.AdminCreatesOnly, bapi.ReservedAuthor = h.ext.adminCreatesOnly(), h.ext.reservedAuthor()
	}
	if h.replays != nil {
		bapi.Replays = h.replays
		if def, by, err := replay.ParseSince(h.cfg.ReplaySince); err == nil {
			bapi.ReplaySince, bapi.ReplaySinceBy = def.Format("2006-01-02"), map[string]string{}
			for ex, t := range by {
				bapi.ReplaySinceBy[ex] = t.Format("2006-01-02")
			}
		}
	}
	if d := h.drafter(); len(d.Providers) > 0 {
		bapi.Drafts = h.drafts(d)
	}
	if st := h.writer(); st != nil {
		bapi.Store = st
	}
	bapi.Routes(api)
	if h.ext != nil {
		h.ext.routes(api)
	}
	api.HandleFunc("/hub/api/me", func(w http.ResponseWriter, r *http.Request) {
		id := authz.FromHeaders(r.Header)
		out := map[string]any{"user": id.User, "role": id.Role.String(), "version": version}
		if h.ext != nil {
			h.ext.me(id.User, out)
		}
		writeJSON(w, out)
	})
	api.HandleFunc("/hub/api/fleet", func(w http.ResponseWriter, r *http.Request) {
		id := authz.FromHeaders(r.Header)
		if id.Role == authz.None {
			http.Error(w, "no wickfleet role", http.StatusForbidden)
			return
		}
		writeJSON(w, visible(id, h.fleet(r.Context())))
	})
	api.HandleFunc("/hub/api/watch", func(w http.ResponseWriter, r *http.Request) {
		id := authz.FromHeaders(r.Header)
		if id.Role == authz.None {
			http.Error(w, "no wickfleet role", http.StatusForbidden)
			return
		}
		writeJSON(w, visibleWatch(id, h.watching(r.Context())))
	})
	api.HandleFunc("/hub/api/reconcile", func(w http.ResponseWriter, r *http.Request) {
		if authz.FromHeaders(r.Header).Role != authz.Admin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		h.rmu.RLock()
		defer h.rmu.RUnlock()
		writeJSON(w, map[string]any{"enabled": h.rec != nil, "last": h.last})
	})
	api.HandleFunc("/hub/api/signals", func(w http.ResponseWriter, r *http.Request) {
		if authz.FromHeaders(r.Header).Role == authz.None {
			http.Error(w, "no wickfleet role", http.StatusForbidden)
			return
		}
		h.smu.RLock()
		defer h.smu.RUnlock()
		writeJSON(w, map[string]any{"rule": "monthly close above 10-month average", "signals": h.sigs})
	})
}

// healthcheck is the container healthcheck; the image has no curl.
func healthcheck(listen string) int {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	cfg := loadConfig()
	if cfg.Live {
		if cfg.KeysDir == "" {
			log.Fatal("WICKFLEET_LIVE_TRADING=true needs WICKFLEET_EXCHANGE_KEYS_DIR")
		}
		log.Printf("LIVE TRADING ON: dry_run: false bots trade real money, keys from %s", cfg.KeysDir)
	}
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(cfg.Listen))
	}
	if len(os.Args) > 1 && (os.Args[1] == "setup-token" || os.Args[1] == "user") {
		os.Exit(login.CLI(cfg.StateDir, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if s := os.Getenv("WICKFLEET_ROLE_GROUPS"); s != "" {
		g, err := authz.ParseGroups(s)
		if err != nil {
			log.Fatalf("WICKFLEET_ROLE_GROUPS: %v", err)
		}
		authz.Groups = g
	}
	if f := os.Getenv("WICKFLEET_AUTHZ_KEY_FILE"); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil || len(strings.TrimSpace(string(raw))) < 16 {
			log.Fatalf("authz key file %s unusable", f)
		}
		cfg.AuthzKey = strings.TrimSpace(string(raw))
	} else if cfg.Auth != "builtin" {
		log.Printf("no WICKFLEET_AUTHZ_KEY_FILE: identity headers are trusted from any caller")
	}
	h := &hub{cfg: cfg}
	switch cfg.Auth {
	case "proxy":
	case "builtin":
		if os.Getenv("WICKFLEET_ROLE_GROUPS") != "" {
			log.Printf("WICKFLEET_AUTH=builtin: WICKFLEET_ROLE_GROUPS is ignored, roles come from users.json")
		}
		authz.Groups = login.Groups
		h.login = &login.Service{Users: filepath.Join(cfg.StateDir, "users.json"),
			Setup: filepath.Join(cfg.StateDir, "setup-token"), TTL: 12 * time.Hour}
		log.Printf("built-in login, users in %s", h.login.Users)
	case "demo":
		if cfg.Live {
			log.Fatal("WICKFLEET_AUTH=demo: WICKFLEET_LIVE_TRADING must stay off")
		}
		authz.Groups = demo.Groups
		log.Printf("read-only demo: every visitor is the viewer %q, writes are refused", demo.User)
	default:
		log.Fatalf("WICKFLEET_AUTH %q: want proxy, builtin or demo", cfg.Auth)
	}
	if cfg.DockerURL != "" {
		webhook := ""
		if cfg.TelegramFile != "" && cfg.TelegramChat != "" {
			webhook = "http://hub:8090/hub/webhook/"
		}
		h.rec = &reconcile.Reconciler{Docker: docker.Client{Base: cfg.DockerURL}, Log: log.Printf, Cfg: reconcile.Config{
			Images: cfg.Images, Strategies: filepath.Join(cfg.RegistryDir, "strategies"),
			DataDir: cfg.DataDir, StateDir: cfg.BotState, Network: cfg.Network, Host: cfg.Host,
			UIBot: cfg.UIBot, WebhookURL: webhook, BotUser: cfg.BotUser,
			Live: cfg.Live, KeysDir: cfg.KeysDir,
		}}
	}
	if err := h.reload(); err != nil {
		log.Printf("registry: %v", err)
	}
	if cfg.DockerURL != "" && cfg.CandleDir != "" {
		since, sinceBy, err := replay.ParseSince(cfg.ReplaySince)
		if err != nil {
			log.Fatalf("WICKFLEET_REPLAY_SINCE: %v", err)
		}
		h.replays = &replay.Runner{Docker: docker.Client{Base: cfg.DockerURL}, Log: log.Printf, Cfg: replay.Config{
			Images: cfg.Images, Strategies: filepath.Join(cfg.RegistryDir, "strategies"),
			CandleDir: cfg.CandleDir, JobDir: cfg.ReplayDir, Network: cfg.Network, BotUser: cfg.BotUser,
			Since: since, SinceBy: sinceBy, Memory: cfg.ReplayMemMB << 20, FreqaiMem: cfg.FreqaiMemMB << 20,
			Timeout: 3 * time.Hour, FetchTimeout: 12 * time.Hour, MaxQueued: 5, KeepJobs: 200, PracticeMem: cfg.PracticeMB << 20,
		}}
		if err := h.replays.Start(context.Background()); err != nil {
			log.Fatalf("replay: %v", err)
		}
		go func() {
			time.Sleep(2 * time.Minute) // let git-sync deliver the registry first
			for {
				h.warmStore()
				time.Sleep(24 * time.Hour)
			}
		}()
	}
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			h.reconcileOnce(ctx)
			cancel()
			time.Sleep(30 * time.Second)
			if err := h.reload(); err != nil {
				log.Printf("registry reload, keeping the last good one: %v", err)
			}
		}
	}()

	var sender notify.Sender = notify.Discard{}
	if cfg.TelegramFile != "" && cfg.TelegramChat != "" {
		tok, err := os.ReadFile(cfg.TelegramFile)
		if err != nil {
			log.Fatalf("telegram token: %v", err)
		}
		sender = notify.Telegram{Token: strings.TrimSpace(string(tok)), ChatID: cfg.TelegramChat}
	} else {
		log.Printf("telegram not configured, notifications are dropped")
	}

	if newExtension != nil {
		e, err := newExtension(h, sender)
		if err != nil {
			log.Fatal(err)
		}
		h.ext = e
	}

	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		log.Fatalf("timezone: %v", err)
	}
	go func() {
		// Hourly, and right away when the watchlist changes.
		var last string
		var at time.Time
		for {
			names := strings.Join(h.Watch().Names(), ",")
			if names != last || time.Since(at) >= time.Hour {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				if sent, err := h.checkSignals(ctx, signals.Yahoo{}, sender, time.Now(), loc); err != nil {
					log.Printf("signals: %v", err)
				} else if sent {
					log.Printf("signals: month summary sent")
				}
				cancel()
				last, at = names, time.Now()
			}
			time.Sleep(time.Minute)
		}
	}()

	srv := &http.Server{Addr: cfg.Listen, Handler: h.routes(sender), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if cfg.MetricsAddr != "" {
		ms := &http.Server{Addr: cfg.MetricsAddr, Handler: h.metricsRoutes(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := ms.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	log.Printf("wickfleet-hub %s on %s, %d bots", version, cfg.Listen, len(h.Bots()))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
