package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/notify"
	"wickfleet/internal/reconcile"
	"wickfleet/internal/signals"
)

// fakeBot answers the freqtrade reads the hub makes for user name / password pw.
func fakeBot(t *testing.T, name string, profit float64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != name || p != "pw-"+name {
			http.Error(w, "auth", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/profit":
			fmt.Fprintf(w, `{"profit_all_coin": %g, "trade_count": 2, "closed_trade_count": 1}`, profit)
		case "/api/v1/balance":
			fmt.Fprintf(w, `{"total": %g, "starting_capital": 1000, "stake": "USDT"}`, 1000+profit)
		case "/api/v1/trades":
			fmt.Fprint(w, `{"trades": [{"fee_open_cost": 1, "fee_close_cost": 1}], "total_trades": 1}`)
		case "/api/v1/status":
			fmt.Fprint(w, `[{"fee_open_cost": 1, "is_open": true}]`)
		case "/api/v1/show_config":
			fmt.Fprint(w, `{"timeframe": "1d"}`)
		case "/api/v1/pair_candles":
			fmt.Fprint(w, `{"columns": ["date", "close", "hi20"], "data": [["2026-10-20", 100, 105]]}`)
		}
	}))
}

func TestHubEndToEnd(t *testing.T) {
	reg, sec := t.TempDir(), t.TempDir()
	ma, other := fakeBot(t, "ma", 10), fakeBot(t, "lu", -4)
	defer ma.Close()
	defer other.Close()
	urls := map[string]string{"ma": ma.URL, "lu": other.URL}
	for name, owner := range map[string]string{"ma": "alice", "lu": "bob"} {
		body := fmt.Sprintf("name: %s\nowner: %s\nstrategy: S\nstake_currency: USDT\npairs: [BTC/USDT]\nwatch: [{name: breakout, left: close, op: \">\", right: hi20}]\n", name, owner)
		os.WriteFile(filepath.Join(reg, name+".yaml"), []byte(body), 0o600)
		os.WriteFile(filepath.Join(sec, name+".api.password"), []byte("pw-"+name+"\n"), 0o600)
	}
	// One server stands in for every bot: route by the bot name in the pattern.
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		req, _ := http.NewRequest(r.Method, urls[name]+"/"+rest+"?"+r.URL.RawQuery, nil)
		req.Header = r.Header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		var buf [64 << 10]byte
		n, _ := resp.Body.Read(buf[:])
		w.Write(buf[:n])
	}))
	defer router.Close()

	h := &hub{cfg: config{RegistryDir: reg, SecretsDir: sec, BotURL: router.URL + "/%s", CacheFor: time.Minute}}
	if err := h.reload(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.routes(notify.Discard{}))
	defer srv.Close()

	get := func(path, user, groups string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("X-Auth-Request-User", user+"@idm.example.org")
		req.Header.Set("X-Auth-Request-Groups", groups)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf [64 << 10]byte
		n, _ := resp.Body.Read(buf[:])
		return resp, buf[:n]
	}

	var rep struct {
		Bots []struct {
			Name  string  `json:"name"`
			Fees  float64 `json:"fees"`
			Error string  `json:"error"`
		} `json:"bots"`
		Totals []struct {
			Bots      int     `json:"bots"`
			ProfitNet float64 `json:"profit_net"`
		} `json:"totals"`
	}
	_, body := get("/hub/api/fleet", "alice", "trade_admins@idm.example.org")
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(rep.Bots) != 2 || rep.Bots[0].Error != "" || rep.Bots[0].Fees != 3 || rep.Totals[0].ProfitNet != 6 {
		t.Fatalf("admin view %s", body)
	}
	_, body = get("/hub/api/fleet", "bob", "trade_viewers@idm.example.org")
	json.Unmarshal(body, &rep)
	if len(rep.Bots) != 1 || rep.Bots[0].Name != "lu" || rep.Totals[0].ProfitNet != -4 {
		t.Fatalf("viewer must see only own bots: %s", body)
	}
	if resp, _ := get("/hub/api/fleet", "x", ""); resp.StatusCode != 403 {
		t.Fatalf("no role got %d", resp.StatusCode)
	}
	if _, body := get("/metrics", "", ""); !strings.Contains(string(body), `wickfleet_bot_fees{bot="ma",owner="alice",dry="true",stake="USDT"} 3`) {
		t.Fatalf("metrics %s", body)
	}
	if resp, _ := get("/healthz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}

	var waits []struct {
		Bot  string  `json:"bot"`
		Pair string  `json:"pair"`
		Met  bool    `json:"met"`
		Gap  float64 `json:"gap"`
	}
	_, body = get("/hub/api/watch", "alice", "trade_admins@idm.example.org")
	if err := json.Unmarshal(body, &waits); err != nil || len(waits) != 2 || waits[0].Met || waits[0].Gap != 0.05 {
		t.Fatalf("admin watch %v: %s", err, body)
	}
	_, body = get("/hub/api/watch", "bob", "trade_viewers@idm.example.org")
	json.Unmarshal(body, &waits)
	if len(waits) != 1 || waits[0].Bot != "lu" {
		t.Fatalf("viewer must see only own watch rows: %s", body)
	}
	if resp, _ := get("/hub/api/watch", "x", ""); resp.StatusCode != 403 {
		t.Fatalf("no role got %d on watch", resp.StatusCode)
	}
	if _, body := get("/metrics", "", ""); !strings.Contains(string(body), `wickfleet_watch_gap_ratio{bot="ma",owner="alice",pair="BTC/USDT",rule="breakout"} 0.05`) {
		t.Fatalf("watch metrics %s", body)
	}
}

func TestSecretRejectsTraversal(t *testing.T) {
	h := &hub{cfg: config{SecretsDir: t.TempDir()}}
	if _, err := h.secret("../etc/passwd"); err == nil {
		t.Fatal("traversal must fail")
	}
}

func TestHealthzBeforeLoad(t *testing.T) {
	h := &hub{cfg: config{RegistryDir: "/nonexistent"}}
	w := httptest.NewRecorder()
	h.routes(notify.Discard{}).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer ok.Close()
	if got := healthcheck(strings.TrimPrefix(ok.URL, "http://")); got != 0 {
		t.Fatalf("healthy server: %d", got)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", 503)
	}))
	defer bad.Close()
	if got := healthcheck(strings.TrimPrefix(bad.URL, "http://")); got != 1 {
		t.Fatalf("503 server: %d", got)
	}
	if got := healthcheck("127.0.0.1:1"); got != 1 {
		t.Fatalf("closed port: %d", got)
	}
}

type barSrc []signals.Bar

func (b barSrc) Daily(context.Context, string) ([]signals.Bar, error) { return b, nil }

type sink struct {
	msgs []string
	fail bool
}

func (s *sink) Send(_ context.Context, m string) error {
	if s.fail {
		return fmt.Errorf("down")
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func TestCheckSignalsOncePerMonth(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	var bars barSrc
	for m := 0; m < 14; m++ {
		bars = append(bars, signals.Bar{Day: time.Date(2025, time.Month(1+m), 10, 21, 0, 0, 0, time.UTC), Close: float64(10 + m)})
	}
	h := &hub{cfg: config{Signals: []string{"XIU.TO"}, StateDir: t.TempDir()}}
	nov := time.Date(2025, 11, 3, 9, 0, 0, 0, loc)

	down := &sink{fail: true}
	if sent, err := h.checkSignals(context.Background(), bars, down, nov, loc); sent || err == nil {
		t.Fatal("a failed send must not count as sent")
	}
	s := &sink{}
	for i := 0; i < 3; i++ {
		if _, err := h.checkSignals(context.Background(), bars, s, nov, loc); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.msgs) != 1 || !strings.Contains(s.msgs[0], "2025-10 close") {
		t.Fatalf("want exactly one October summary, got %q", s.msgs)
	}
	if _, err := h.checkSignals(context.Background(), bars, s, nov.AddDate(0, 1, 0), loc); err != nil || len(s.msgs) != 2 {
		t.Fatalf("December run must send the November summary: %v %d", err, len(s.msgs))
	}
}

func TestWatchlistSource(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bots"), 0o755)
	h := &hub{cfg: config{RegistryDir: dir, Signals: []string{"ENV.TO"}}}
	names := func() string { return strings.Join(h.Watch().Names(), ",") }
	if err := h.reload(); err != nil || names() != "ENV.TO" {
		t.Fatalf("no file must fall back to env: %v %q", err, names())
	}
	os.WriteFile(filepath.Join(dir, "watchlist.yaml"), []byte("symbols:\n  - symbol: XIC.TO\n  - symbol: BTC-USD\n"), 0o644)
	if err := h.reload(); err != nil || names() != "XIC.TO,BTC-USD" {
		t.Fatalf("file must win: %v %q", err, names())
	}
	os.WriteFile(filepath.Join(dir, "watchlist.yaml"), []byte("symbols:\n  - symbol: ../x\n"), 0o644)
	if err := h.reload(); err != nil || names() != "XIC.TO,BTC-USD" {
		t.Fatalf("broken file must keep the last good list and the bots: %v %q", err, names())
	}
	os.WriteFile(filepath.Join(dir, "watchlist.yaml"), []byte("symbols: []\n"), 0o644)
	if err := h.reload(); err != nil || names() != "" {
		t.Fatalf("an empty file is an empty list, not the env: %q", names())
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" XIU.TO, ,XEQT.TO "); len(got) != 2 || got[1] != "XEQT.TO" {
		t.Fatalf("%q", got)
	}
}

func TestKeyGuardsAuthzAndAPI(t *testing.T) {
	reg, sec := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(reg, "ma.yaml"), []byte("name: ma\nowner: alice\nstrategy: S\n"), 0o600)
	os.WriteFile(filepath.Join(sec, "ma.api.password"), []byte("pw"), 0o600)
	h := &hub{cfg: config{RegistryDir: reg, SecretsDir: sec, AuthzKey: "proxy-key", MetricsAddr: ":0"}}
	h.reload()
	mux := h.routes(notify.Discard{})
	for _, path := range []string{"/authz", "/hub/api/me", "/hub/api/fleet", "/hub/api/signals"} {
		for _, withKey := range []bool{false, true} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("X-Auth-Request-User", "alice@idm.example.org")
			r.Header.Set("X-Auth-Request-Groups", "trade_admins@idm.example.org")
			r.Header.Set("X-Forwarded-Method", "GET")
			r.Header.Set("X-Forwarded-Uri", "/bots/ma/api/v1/status")
			if withKey {
				r.Header.Set("X-Wickfleet-Authz-Key", "proxy-key")
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if !withKey && w.Code != 401 {
				t.Errorf("%s without key: %d", path, w.Code)
			}
			if withKey && w.Code == 401 {
				t.Errorf("%s with key refused", path)
			}
			if !withKey && w.Header().Get("Authorization") != "" {
				t.Errorf("%s leaked a bot key without the proxy key", path)
			}
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 404 {
		t.Errorf("main listener must not serve /metrics when a metrics listener is set: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.metricsRoutes().ServeHTTP(w, httptest.NewRequest("GET", "/authz", nil))
	if w.Code != 404 {
		t.Errorf("metrics listener must not serve /authz: %d", w.Code)
	}
}

func TestParseImages(t *testing.T) {
	m := parseImages("freqtrade=ft:1@sha256:aa, freqtrade_freqai=ft:2,bad,=x")
	if len(m) != 2 || m["freqtrade"] != "ft:1@sha256:aa" || m["freqtrade_freqai"] != "ft:2" {
		t.Fatalf("%v", m)
	}
}

func TestReconcileEndpointAdminOnly(t *testing.T) {
	h := &hub{cfg: config{RegistryDir: t.TempDir()}}
	mux := h.routes(notify.Discard{})
	for groups, want := range map[string]int{"trade_operators@idm.example.org": 403, "trade_admins@idm.example.org": 200} {
		r := httptest.NewRequest("GET", "/hub/api/reconcile", nil)
		r.Header.Set("X-Auth-Request-User", "alice@idm.example.org")
		r.Header.Set("X-Auth-Request-Groups", groups)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d want %d", groups, w.Code, want)
		}
	}
}

func TestSecretFromReconciler(t *testing.T) {
	h := &hub{cfg: config{}, rec: &reconcile.Reconciler{Cfg: reconcile.Config{StateDir: t.TempDir()}}}
	a, err := h.secret("ma")
	b, _ := h.secret("ma")
	if err != nil || len(a) != 64 || a != b {
		t.Fatalf("%v %q %q", err, a, b)
	}
}

type countEngine struct{ managed int }

func (c *countEngine) Managed(context.Context) ([]docker.Container, error) {
	c.managed++
	return []docker.Container{{ID: "c1", Labels: map[string]string{"wickfleet.bot": "trend", "wickfleet.managed": "hub"}}}, nil
}
func (c *countEngine) Create(context.Context, string, docker.Spec) (string, error) { return "x", nil }
func (c *countEngine) Start(context.Context, string) error                         { return nil }
func (c *countEngine) Remove(context.Context, string) error {
	panic("must not remove before a registry loaded")
}
func (c *countEngine) Pull(context.Context, string) error { return nil }

func TestNoReconcileBeforeRegistryLoads(t *testing.T) {
	e := &countEngine{}
	h := &hub{cfg: config{RegistryDir: filepath.Join(t.TempDir(), "missing")}, rec: &reconcile.Reconciler{Docker: e}}
	if err := h.reload(); err == nil {
		t.Fatal("missing registry must fail")
	}
	h.reconcileOnce(context.Background())
	if e.managed != 0 {
		t.Fatal("reconcile ran without a registry")
	}
}

func TestDrafterConfig(t *testing.T) {
	h := &hub{cfg: config{}}
	if len(h.drafter().Providers) != 0 {
		t.Fatal("no config, no providers")
	}
	key := filepath.Join(t.TempDir(), "k")
	os.WriteFile(key, []byte("sk-test\n"), 0o600)
	h.cfg = config{DraftOllama: "http://ai-1:11434", DraftModel: "qwen3:4b", DraftAnthropicKey: key, DraftClaudeModel: "claude-opus-5-5"}
	ps := h.drafter().Providers
	if len(ps) != 2 || ps[0].Name() != "ollama/qwen3:4b" || ps[1].Name() != "anthropic/claude-opus-5-5" {
		t.Fatalf("order %v", ps)
	}
	h.cfg.DraftAnthropicKey = filepath.Join(t.TempDir(), "missing")
	if ps := h.drafter().Providers; len(ps) != 1 {
		t.Fatalf("a missing key file drops Anthropic only: %v", ps)
	}
}
