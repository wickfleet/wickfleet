package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"wickfleet/internal/docker"
	"wickfleet/internal/guard"
	"wickfleet/internal/registry"
)

type fakeEngine struct {
	ctrs    map[string]docker.Container // by id
	specs   map[string]docker.Spec
	n       int
	noImage map[string]bool
	pulled  []string
	started []string
}

func newFake() *fakeEngine {
	return &fakeEngine{ctrs: map[string]docker.Container{}, specs: map[string]docker.Spec{}, noImage: map[string]bool{}}
}

func (f *fakeEngine) Managed(context.Context) ([]docker.Container, error) {
	var out []docker.Container
	for _, c := range f.ctrs {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeEngine) Create(_ context.Context, name string, s docker.Spec) (string, error) {
	if f.noImage[s.Image] {
		return "", &docker.StatusError{Code: 404, Body: "No such image"}
	}
	f.n++
	id := fmt.Sprintf("c%d", f.n)
	f.ctrs[id] = docker.Container{ID: id, Names: []string{"/" + name}, Labels: s.Labels, State: "created"}
	f.specs[id] = s
	return id, nil
}
func (f *fakeEngine) Start(_ context.Context, id string) error {
	c := f.ctrs[id]
	c.State = "running"
	f.ctrs[id] = c
	f.started = append(f.started, id)
	return nil
}
func (f *fakeEngine) Remove(_ context.Context, id string) error { delete(f.ctrs, id); return nil }
func (f *fakeEngine) Pull(_ context.Context, ref string) error {
	f.pulled = append(f.pulled, ref)
	delete(f.noImage, ref)
	return nil
}

func setup(t *testing.T) (*Reconciler, *fakeEngine, string) {
	t.Helper()
	root := t.TempDir()
	strat := filepath.Join(root, "repo", "strategies")
	os.MkdirAll(strat, 0o755)
	os.WriteFile(filepath.Join(strat, "EmaCross.py"), []byte("class EmaCross: pass\n"), 0o644)
	f := newFake()
	r := &Reconciler{Docker: f, Cfg: Config{
		Images:     map[string]string{"freqtrade": "ft:1@sha256:aa", "freqtrade_freqai": "ft:1_freqai@sha256:bb"},
		Strategies: strat, DataDir: filepath.Join(root, "bots"), StateDir: filepath.Join(root, "state"),
		Network: "wickfleet_edge", Host: "trade.example.org", UIBot: "trend",
		WebhookURL: "http://hub:8090/hub/webhook/", BotUser: "1000:1000",
	}}
	return r, f, root
}

func bot(name string) registry.Bot {
	return registry.Bot{Name: name, Owner: "alice", Strategy: "EmaCross", StrategyFile: "EmaCross.py",
		Exchange: "kraken", Pairs: []string{"BTC/USDT"}, StakeCurrency: "USDT", Wallet: 1000, MaxOpenTrades: 3,
		Image: "freqtrade", MemLimit: "768m"}
}

func TestRunLifecycle(t *testing.T) {
	r, f, root := setup(t)
	ctx := context.Background()

	res := r.Run(ctx, []registry.Bot{bot("trend"), bot("other")})
	if len(res.Errors) != 0 || len(res.Created) != 2 {
		t.Fatalf("first run %+v", res)
	}
	var trendSpec docker.Spec
	for id, s := range f.specs {
		if s.Labels[labelBot] == "trend" {
			trendSpec = s
			if f.ctrs[id].State != "running" {
				t.Fatal("created container must be started")
			}
		}
	}
	hc := trendSpec.HostConfig
	if trendSpec.Image != "ft:1@sha256:aa" || hc.Privileged || hc.Memory != 768<<20 || hc.NetworkMode != "wickfleet_edge" || trendSpec.User != "1000:1000" {
		t.Fatalf("spec %+v", trendSpec)
	}
	if len(hc.Binds) != 3 || !strings.HasSuffix(hc.Binds[1], "config.json:/freqtrade/user_data/config.json:ro") || hc.CapDrop[0] != "ALL" {
		t.Fatalf("binds %v", hc.Binds)
	}
	if trendSpec.Labels["traefik.http.routers.ui.service"] != "bot-trend" {
		t.Fatal("UI bot must carry the ui router")
	}
	if !strings.Contains(trendSpec.Labels["traefik.http.routers.bot-trend.middlewares"], "hubauth@docker") {
		t.Fatal("bot route must go through hubauth")
	}
	if a := trendSpec.NetworkingConfig.EndpointsConfig["wickfleet_edge"].Aliases; len(a) != 1 || a[0] != "bot-trend" {
		t.Fatalf("alias %v", a)
	}

	cfg, err := os.ReadFile(filepath.Join(root, "state", "bots", "trend", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(cfg, &m)
	if m["dry_run"] != true || m["webhook"].(map[string]any)["url"] != "http://hub:8090/hub/webhook/trend" {
		t.Fatalf("config %s", cfg)
	}
	if st, _ := os.Stat(filepath.Join(root, "state", "bots", "trend.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("secrets mode %v", st.Mode())
	}
	sec1, _ := r.Secrets("trend")

	res = r.Run(ctx, []registry.Bot{bot("trend"), bot("other")})
	if len(res.Unchanged) != 2 || len(res.Created)+len(res.Replaced) != 0 {
		t.Fatalf("second run must change nothing: %+v", res)
	}
	if sec2, _ := r.Secrets("trend"); sec2 != sec1 {
		t.Fatal("secrets must be stable across runs")
	}

	os.WriteFile(filepath.Join(r.Cfg.Strategies, "EmaCross.py"), []byte("class EmaCross: x = 1\n"), 0o644)
	res = r.Run(ctx, []registry.Bot{bot("trend")})
	if len(res.Replaced) != 2 && !(len(res.Replaced) == 1 && len(res.Removed) == 1) {
		t.Fatalf("strategy change must replace, dropped bot must go: %+v", res)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "other" || len(f.ctrs) != 1 {
		t.Fatalf("removed %+v, %d left", res, len(f.ctrs))
	}
}

func TestRejectsUnknownImageKey(t *testing.T) {
	r, f, _ := setup(t)
	b := bot("evil")
	b.Image = "alpine"
	res := r.Run(context.Background(), []registry.Bot{b})
	if !strings.Contains(res.Errors["evil"], "not allowed") || len(f.ctrs) != 0 {
		t.Fatalf("%+v", res)
	}
}

// The bot runs as BotUser, not as the hub; the hub's umask must not lock it out.
func TestDataDirGroupWritableUnderUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	r, _, root := setup(t)
	// "stale" was made by an older hub before this fix.
	os.MkdirAll(filepath.Join(root, "bots", "stale"), 0o750)
	if res := r.Run(context.Background(), []registry.Bot{bot("fresh"), bot("stale")}); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	for _, n := range []string{"fresh", "stale"} {
		st, err := os.Stat(filepath.Join(root, "bots", n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o070 != 0o070 {
			t.Fatalf("%s data dir mode %v, group needs rwx", n, st.Mode().Perm())
		}
	}
}

func TestPullsMissingImageAndStartsStopped(t *testing.T) {
	r, f, _ := setup(t)
	f.noImage["ft:1@sha256:aa"] = true
	res := r.Run(context.Background(), []registry.Bot{bot("trend")})
	if len(res.Created) != 1 || len(f.pulled) != 1 {
		t.Fatalf("%+v pulled=%v", res, f.pulled)
	}
	for id, c := range f.ctrs {
		c.State = "exited"
		f.ctrs[id] = c
	}
	n := len(f.started)
	res = r.Run(context.Background(), []registry.Bot{bot("trend")})
	if len(res.Unchanged) != 1 || len(f.started) != n+1 {
		t.Fatalf("exited container must be started again: %+v", res)
	}
}

func TestMissingStrategyKeepsOldContainer(t *testing.T) {
	r, f, _ := setup(t)
	r.Run(context.Background(), []registry.Bot{bot("trend")})
	b := bot("trend")
	b.StrategyFile = "Gone.py"
	res := r.Run(context.Background(), []registry.Bot{b})
	if res.Errors["trend"] == "" || len(f.ctrs) != 1 || len(res.Removed) != 0 {
		t.Fatalf("a broken bot must keep running: %+v", res)
	}
}

func TestMemBytes(t *testing.T) {
	for in, want := range map[string]int64{"768m": 768 << 20, "2g": 2 << 30, "": 0} {
		if got := memBytes(in); got != want {
			t.Errorf("%s: %d", in, got)
		}
	}
}

func TestRemovedBotDirsAreArchived(t *testing.T) {
	r, _, root := setup(t)
	ctx := context.Background()
	if res := r.Run(ctx, []registry.Bot{bot("trend"), bot("gone")}); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	os.MkdirAll(filepath.Join(root, "bots", "orphan"), 0o750)
	os.WriteFile(filepath.Join(root, "bots", "gone", "trades.sqlite"), []byte("x"), 0o640)

	res := r.Run(ctx, []registry.Bot{bot("trend")})
	if len(res.Errors) != 0 || len(res.Removed) != 1 {
		t.Fatalf("%+v", res)
	}
	for _, d := range []string{"bots", "state/bots"} {
		if _, err := os.Stat(filepath.Join(root, d, "trend")); err != nil {
			t.Fatalf("%s/trend must stay: %v", d, err)
		}
		if _, err := os.Stat(filepath.Join(root, d, "gone")); !os.IsNotExist(err) {
			t.Fatalf("%s/gone must be archived", d)
		}
	}
	moved, _ := filepath.Glob(filepath.Join(root, "bots", ".removed", "gone-*", "trades.sqlite"))
	orphan, _ := filepath.Glob(filepath.Join(root, "bots", ".removed", "orphan-*"))
	secret, _ := filepath.Glob(filepath.Join(root, "state", "bots", ".removed", "gone-*.json"))
	if len(moved) != 1 || len(orphan) != 1 || len(secret) != 1 {
		t.Fatalf("archive: %v %v %v", moved, orphan, secret)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "bots", "trend.json")); err != nil {
		t.Fatal("a wanted bot's secrets must stay")
	}

	if res := r.Run(ctx, nil); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "bots", "trend")); err != nil {
		t.Fatal("an empty registry must not archive anything")
	}
}

func TestArchiveFailureIsReported(t *testing.T) {
	r, _, root := setup(t)
	ctx := context.Background()
	if res := r.Run(ctx, []registry.Bot{bot("trend"), bot("gone")}); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	// A file where .removed/ should go makes MkdirAll fail.
	os.WriteFile(filepath.Join(root, "bots", ".removed"), []byte("x"), 0o640)
	var logged []string
	r.Log = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }

	res := r.Run(ctx, []registry.Bot{bot("trend")})
	if !strings.HasPrefix(res.Errors["gone"], "archive: ") {
		t.Fatalf("failed archive must be an error: %+v", res.Errors)
	}
	if _, err := os.Stat(filepath.Join(root, "bots", "gone")); err != nil {
		t.Fatal("a failed archive must leave the dir in place")
	}
	for _, l := range logged {
		if strings.Contains(l, "archived gone to "+filepath.Join(root, "bots")) {
			t.Fatalf("must not log a move that failed: %q", l)
		}
	}
}

// Every container the reconciler asks for must pass the docker guard as
// production configures it, or the guard would stop the fleet.
func TestSpecsPassTheDockerGuard(t *testing.T) {
	r, f, _ := setup(t)
	if res := r.Run(context.Background(), []registry.Bot{bot("trend"), bot("other")}); len(res.Errors) != 0 {
		t.Fatalf("%+v", res)
	}
	var images []string
	for _, ref := range r.Cfg.Images {
		images = append(images, ref)
	}
	p := guard.Policy{Images: images, BindPrefixes: []string{r.Cfg.DataDir, r.Cfg.StateDir}, Networks: []string{r.Cfg.Network},
		Users: []string{r.Cfg.BotUser}, NamePrefix: "wickfleet-", ManagedLabel: docker.ManagedLabel}
	for id, s := range f.specs {
		raw, _ := json.Marshal(s)
		if err := p.CheckCreate(strings.TrimPrefix(f.ctrs[id].Names[0], "/"), raw); err != nil {
			t.Errorf("%s: %v", f.ctrs[id].Names[0], err)
		}
	}
	if len(f.specs) != 2 {
		t.Fatalf("checked %d specs", len(f.specs))
	}
}
