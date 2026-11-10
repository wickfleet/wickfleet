package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/guard"
	"wickfleet/internal/registry"
)

// fakeEngine plays freqtrade: a "bt" container drops the fixture result into
// the job's out/; exit codes and holds are set per step name.
type fakeEngine struct {
	mu      sync.Mutex
	specs   map[string]docker.Spec
	names   map[string]string
	removed []string
	exit    map[string]int    // step suffix -> exit code
	logs    map[string]string // step suffix -> logs
	hold    map[string]chan struct{}
	order   []string
	n       int
	orphans []docker.Container
	ahead   string // lookahead fixture the "la" step writes; "" = clean
	noImage map[string]bool
	pulled  []string
}

func newFake() *fakeEngine {
	return &fakeEngine{specs: map[string]docker.Spec{}, names: map[string]string{}, exit: map[string]int{},
		logs: map[string]string{}, hold: map[string]chan struct{}{}}
}

func (f *fakeEngine) step(id string) string { n := f.names[id]; return n[strings.LastIndex(n, "-")+1:] }

func (f *fakeEngine) Create(_ context.Context, name string, s docker.Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noImage[s.Image] {
		return "", &docker.StatusError{Code: 404, Body: "No such image: " + s.Image}
	}
	f.n++
	id := fmt.Sprintf("c%d", f.n)
	f.specs[id], f.names[id] = s, name
	return id, nil
}

func (f *fakeEngine) Pull(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	delete(f.noImage, ref)
	return nil
}

func (f *fakeEngine) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, f.names[id])
	host := strings.SplitN(f.specs[id].HostConfig.Binds[0], ":", 2)[0]
	if f.step(id) == "bt" && f.exit["bt"] == 0 {
		raw, _ := os.ReadFile("testdata/emacross-2023-10.zip")
		os.WriteFile(filepath.Join(host, "out", "backtest-result-2026-10-19_00-06-24.zip"), raw, 0o644)
	}
	if f.step(id) == "la" && f.exit["la"] == 0 {
		fx := f.ahead
		if fx == "" {
			fx = "lookahead-clean.csv"
		}
		raw, _ := os.ReadFile("testdata/bias/" + fx)
		os.WriteFile(filepath.Join(host, "out", "lookahead.csv"), raw, 0o644)
	}
	return nil
}

func (f *fakeEngine) Inspect(_ context.Context, id string) (docker.State, error) {
	f.mu.Lock()
	h := f.hold[f.specs[id].HostConfig.Binds[1]] // keyed by the candle bind, i.e. the exchange
	code := f.exit[f.step(id)]
	f.mu.Unlock()
	if h != nil {
		select {
		case <-h:
		default:
			return docker.State{Status: "running", Running: true}, nil
		}
	}
	return docker.State{Status: "exited", ExitCode: code}, nil
}

func (f *fakeEngine) Logs(_ context.Context, id string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.logs[f.step(id)]; ok {
		return l, nil
	}
	if f.step(id) == "ra" {
		raw, _ := os.ReadFile("testdata/bias/recursive-clean.log")
		return string(raw), nil
	}
	return "", nil
}

func (f *fakeEngine) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeEngine) Labeled(_ context.Context, v string) ([]docker.Container, error) {
	if v != "replay" {
		return nil, errors.New("wrong label")
	}
	return f.orphans, nil
}

var today = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*Runner, *fakeEngine, string) {
	t.Helper()
	root := t.TempDir()
	strat := filepath.Join(root, "strategies")
	os.MkdirAll(strat, 0o755)
	os.WriteFile(filepath.Join(strat, "EmaCross.py"), []byte("class EmaCross:\n    timeframe = \"1d\"\n"), 0o644)
	f := newFake()
	r := &Runner{Docker: f, Now: func() time.Time { return today }, Cfg: Config{
		Images:     map[string]string{"freqtrade": "ft:1@sha256:aa"},
		Strategies: strat, CandleDir: filepath.Join(root, "candles"), JobDir: filepath.Join(root, "jobs"),
		Network: "wickfleet_edge", BotUser: "1000:1000", Since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Memory: 1536 << 20, Timeout: 10 * time.Second, MaxQueued: 2, KeepJobs: 50,
	}}
	// Registered after TempDir, so it runs first: no job writes into a removed dir.
	t.Cleanup(r.Wait)
	// The store already holds history from Since; tests of the prepend step remove this.
	os.MkdirAll(filepath.Join(root, "candles", "binance"), 0o755)
	os.WriteFile(filepath.Join(root, "candles", "binance", ".wickfleet-since-BTC_USDT-1d"), []byte("2025-01-01\n"), 0o644)
	return r, f, root
}

func trendBot() registry.Bot {
	return registry.Bot{Name: "trend", Owner: "alice", Strategy: "EmaCross", StrategyFile: "EmaCross.py",
		Exchange: "binance", Pairs: []string{"BTC/USDT"}, StakeCurrency: "USDT", Wallet: 1000, MaxOpenTrades: 3, Image: "freqtrade"}
}

func wait(t *testing.T, r *Runner, id string) Job {
	t.Helper()
	for i := 0; i < 200; i++ {
		if j, _ := r.Get(id); j.State == Done || j.State == Failed {
			return j
		}
		time.Sleep(25 * time.Millisecond)
	}
	j, _ := r.Get(id)
	t.Fatalf("job never finished: %+v", j)
	return j
}

func TestReplayRuns(t *testing.T) {
	r, f, root := setup(t)
	f.orphans = []docker.Container{{ID: "old1"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	got := wait(t, r, j.ID)
	if got.State != Done || got.Summary == nil || got.Summary.Trades != 11 || got.Summary.Fees <= 0 {
		t.Fatalf("%+v", got)
	}
	trades, err := r.Trades(j.ID)
	if err != nil || len(trades) != 11 {
		t.Fatalf("trades %v %v", trades, err)
	}
	if len(f.order) != 4 || !strings.HasSuffix(f.order[0], "-dl") || !strings.HasSuffix(f.order[1], "-bt") ||
		!strings.HasSuffix(f.order[2], "-la") || !strings.HasSuffix(f.order[3], "-ra") {
		t.Fatalf("steps %v", f.order)
	}
	if got.Bias == nil || got.Bias.Lookahead != "clean" || got.Bias.Recursive != "clean" || got.Bias.Signals != 20 {
		t.Fatalf("bias %+v", got.Bias)
	}
	for id, s := range f.specs {
		want := []string{filepath.Join(root, "jobs", j.ID) + ":/job", filepath.Join(root, "candles", "binance") + ":/candles/binance"}
		if strings.Join(s.HostConfig.Binds, ",") != strings.Join(want, ",") || s.User != "1000:1000" ||
			s.Labels[docker.ManagedLabel] != "replay" || s.HostConfig.RestartPolicy.Name != "no" ||
			s.HostConfig.Memory != 1536<<20 || s.HostConfig.CapDrop[0] != "ALL" || s.Image != "ft:1@sha256:aa" {
			t.Fatalf("%s spec %+v", id, s)
		}
	}
	// Containers go after each step; the orphan from before the start went too.
	if strings.Join(f.removed, ",") != "old1,c1,c2,c3,c4" {
		t.Fatalf("removed %v", f.removed)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", j.ID, "strategies", "EmaCross.py")); err != nil {
		t.Fatal("strategy not copied into the job")
	}
}

// The control from the real run: a strategy peeking one candle ahead.
func TestBiasedReplayDoesNotPass(t *testing.T) {
	r, f, _ := setup(t)
	f.ahead = "lookahead-biased.csv"
	v, _ := os.ReadFile("testdata/bias/recursive-variance.log")
	f.logs["ra"] = string(v)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	j, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	got := wait(t, r, j.ID)
	if got.State != Done || got.Bias.Lookahead != "biased" || got.Bias.Indicators != "sma200,enter_long" ||
		got.Bias.Recursive != "variance" || !strings.Contains(got.Bias.Note, "ema50") {
		t.Fatalf("%+v %+v", got, got.Bias)
	}
	if _, ok := r.Passed("alice", SettingsHash(trendBot()), 7*24*time.Hour); ok {
		t.Fatal("a biased replay must not let the bot start")
	}
}

// A broken analysis is inconclusive, and inconclusive still passes the gate.
func TestBrokenAnalysisIsInconclusive(t *testing.T) {
	r, f, _ := setup(t)
	f.exit["la"] = 1
	f.logs["la"] = "2026 - freqtrade - ERROR - something broke\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	j, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	got := wait(t, r, j.ID)
	if got.State != Done || got.Bias.Lookahead != "inconclusive" || !strings.Contains(got.Bias.Note, "something broke") {
		t.Fatalf("%+v", got.Bias)
	}
	if _, ok := r.Passed("alice", SettingsHash(trendBot()), 7*24*time.Hour); !ok {
		t.Fatal("inconclusive warns, it does not block")
	}
}

func TestFreqaiSkipsBiasChecks(t *testing.T) {
	r, f, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	b := trendBot()
	b.FreqaiModel = "LightGBMRegressor"
	j, _ := r.Submit("alice", b, "2025-10-18", "2026-10-18")
	got := wait(t, r, j.ID)
	if got.Bias == nil || got.Bias.Lookahead != "skipped" || len(f.order) != 2 {
		t.Fatalf("%+v %v", got.Bias, f.order)
	}
}

func TestPassed(t *testing.T) {
	r, _, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	b := trendBot()
	j, _ := r.Submit("alice", b, "2025-10-18", "2026-10-18")
	wait(t, r, j.ID)
	saved := b
	saved.Name, saved.Owner = "trend-btc", "alice"
	if _, ok := r.Passed("alice", SettingsHash(saved), 7*24*time.Hour); !ok {
		t.Fatal("same settings under another name must pass")
	}
	if _, ok := r.Passed("bob", SettingsHash(saved), 7*24*time.Hour); ok {
		t.Fatal("another user's replay must not count")
	}
	changed := saved
	changed.Pairs = []string{"ETH/USDT"}
	if _, ok := r.Passed("alice", SettingsHash(changed), 7*24*time.Hour); ok {
		t.Fatal("changed settings must not pass")
	}
	r.Now = func() time.Time { return today.Add(8 * 24 * time.Hour) }
	if _, ok := r.Passed("alice", SettingsHash(saved), 7*24*time.Hour); ok {
		t.Fatal("an old replay must not pass")
	}
}

func TestReplayFailureCarriesFreqtradeError(t *testing.T) {
	r, f, _ := setup(t)
	f.exit["bt"] = 2
	f.logs["bt"] = "2026 - freqtrade - INFO - loading\n2026 - freqtrade - ERROR - No data found. Terminating.\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	j, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	got := wait(t, r, j.ID)
	if got.State != Failed || got.Error != "backtest: exit 2: No data found. Terminating." {
		t.Fatalf("%+v", got)
	}
}

// freqtrade exits 0 on a fatal config error; the log is the only signal.
func TestExitZeroWithErrorFails(t *testing.T) {
	r, f, _ := setup(t)
	f.logs["dl"] = "2026 - freqtrade - ERROR - Configuration error: 'x' is too short\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	j, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	got := wait(t, r, j.ID)
	if got.State != Failed || got.Error != "download: Configuration error: 'x' is too short" {
		t.Fatalf("%+v", got)
	}
	if len(f.order) != 1 {
		t.Fatalf("backtest must not run after a failed download: %v", f.order)
	}
}

func TestSubmitRejects(t *testing.T) {
	r, _, _ := setup(t)
	r.init(context.Background())
	noFile := trendBot()
	noFile.StrategyFile = ""
	badImg := trendBot()
	badImg.Image = "alpine"
	for name, c := range map[string]struct {
		b        registry.Bot
		from, to string
	}{
		"reversed":     {trendBot(), "2026-01-01", "2025-06-01"},
		"before store": {trendBot(), "2024-06-01", "2025-06-01"},
		"future":       {trendBot(), "2026-01-01", "2026-12-01"},
		"bad date":     {trendBot(), "2026-1-1", "2026-02-01"},
		"no file":      {noFile, "2025-06-01", "2026-01-01"},
		"image":        {badImg, "2025-06-01", "2026-01-01"},
	} {
		if _, err := r.Submit("alice", c.b, c.from, c.to); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// No lanes run, so the queue (2) fills.
	for i := 0; i < 2; i++ {
		if _, err := r.Submit("alice", trendBot(), "2025-06-01", "2026-01-01"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Submit("alice", trendBot(), "2025-06-01", "2026-01-01"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third must be busy, got %v", err)
	}
}

func TestRestartFailsUnfinishedJobs(t *testing.T) {
	r, _, root := setup(t)
	dir := filepath.Join(root, "jobs", "r20261018-000000-aaaaaa")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "job.json"), []byte(`{"id":"r20261018-000000-aaaaaa","kind":"replay","state":"testing","note":"waiting for a free slot"}`), 0o644)
	r.init(context.Background())
	j, ok := r.Get("r20261018-000000-aaaaaa")
	if !ok || j.State != Failed || !strings.Contains(j.Error, "restarted") || j.Note != "" {
		t.Fatalf("%+v", j)
	}
}

func TestFetchTimeout(t *testing.T) {
	r, f, root := setup(t)
	r.Cfg.Timeout, r.Cfg.FetchTimeout = 50*time.Millisecond, 10*time.Second
	hold := make(chan struct{})
	f.hold[filepath.Join(root, "candles", "binance")+":/candles/binance"] = hold
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	fj, _ := r.Fetch("binance", "freqtrade", []string{"BTC/USDT"}, []string{"1d"})
	time.Sleep(300 * time.Millisecond)
	if j, _ := r.Get(fj.ID); j.State != Downloading {
		t.Fatalf("a fetch must outlive the replay timeout: %+v", j)
	}
	close(hold)
	if got := wait(t, r, fj.ID); got.State != Done {
		t.Fatalf("%+v", got)
	}
	f.hold[filepath.Join(root, "candles", "binance")+":/candles/binance"] = make(chan struct{})
	rj, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	if got := wait(t, r, rj.ID); got.State != Failed || !strings.Contains(got.Error, "gave up after 50ms") {
		t.Fatalf("a replay keeps its own timeout: %+v", got)
	}
}

// A store fetch on an exchange holds it; a replay there waits, one elsewhere does not.
func TestExchangeLock(t *testing.T) {
	r, f, root := setup(t)
	hold := make(chan struct{})
	f.hold[filepath.Join(root, "candles", "binance")+":/candles/binance"] = hold
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	fj, _ := r.Fetch("binance", "freqtrade", []string{"BTC/USDT"}, []string{"1d"})
	time.Sleep(100 * time.Millisecond)
	rj, _ := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	other := trendBot()
	other.Exchange = "ndax"
	oj, _ := r.Submit("alice", other, "2025-10-18", "2026-10-18")
	if got := wait(t, r, oj.ID); got.State != Done {
		t.Fatalf("ndax replay must not wait for binance: %+v", got)
	}
	if j, _ := r.Get(rj.ID); j.State != Queued || !strings.Contains(j.Note, "binance candle store") {
		t.Fatalf("binance replay must wait: %+v", j)
	}
	close(hold)
	if got := wait(t, r, fj.ID); got.State != Done {
		t.Fatalf("fetch %+v", got)
	}
	if got := wait(t, r, rj.ID); got.State != Done {
		t.Fatalf("replay after fetch %+v", got)
	}
}

// The weekly prune drops a digest-only image no container uses; a replay pulls it.
func TestReplayPullsMissingImage(t *testing.T) {
	r, f, _ := setup(t)
	f.noImage = map[string]bool{"ft:1@sha256:aa": true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, r, j.ID); got.State != Done {
		t.Fatalf("%+v", got)
	}
	if len(f.pulled) != 1 || f.pulled[0] != "ft:1@sha256:aa" {
		t.Fatalf("pulled %v", f.pulled)
	}
}

// Every replay step container must pass the docker guard as production
// configures it, or the guard would stop replays.
func TestStepsPassTheDockerGuard(t *testing.T) {
	r, f, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, r, j.ID); got.State != Done {
		t.Fatalf("%+v", got)
	}
	p := guard.Policy{Images: []string{r.Cfg.Images["freqtrade"]}, BindPrefixes: []string{r.Cfg.JobDir, r.Cfg.CandleDir},
		Networks: []string{r.Cfg.Network}, Users: []string{r.Cfg.BotUser}, NamePrefix: "wickfleet-", ManagedLabel: docker.ManagedLabel}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, s := range f.specs {
		raw, _ := json.Marshal(s)
		if err := p.CheckCreate(f.names[id], raw); err != nil {
			t.Errorf("%s: %v", f.names[id], err)
		}
	}
	if len(f.specs) != 4 {
		t.Fatalf("checked %d step specs", len(f.specs))
	}
}

// History before the store's start is fetched once with --prepend; the
// marker then says how far back the store goes.
func TestHistoryIsPrependedOncePerEarlierStart(t *testing.T) {
	r, f, root := setup(t)
	marker := filepath.Join(root, "candles", "binance", ".wickfleet-since-BTC_USDT-1d")
	os.Remove(marker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	steps := func() []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for _, n := range f.order {
			out = append(out, n[strings.LastIndex(n, "-")+1:])
		}
		f.order = nil
		return out
	}
	run := func() {
		t.Helper()
		j, err := r.Submit("alice", trendBot(), "2025-10-18", "2026-10-18")
		if err != nil {
			t.Fatal(err)
		}
		if got := wait(t, r, j.ID); got.State != Done {
			t.Fatalf("%+v", got)
		}
	}
	run()
	if got := strings.Join(steps(), ","); !strings.HasPrefix(got, "dlp,dl,bt") {
		t.Fatalf("unmarked store must prepend first: %s", got)
	}
	if raw, _ := os.ReadFile(marker); strings.TrimSpace(string(raw)) != "2025-01-01" {
		t.Fatalf("marker %q", raw)
	}
	cmd := ""
	for _, s := range f.specs {
		if strings.Contains(strings.Join(s.Cmd, " "), "--prepend") {
			cmd = strings.Join(s.Cmd, " ")
		}
	}
	if !strings.Contains(cmd, "--timerange 20250101-") {
		t.Fatalf("prepend command %q", cmd)
	}
	run()
	if got := strings.Join(steps(), ","); strings.Contains(got, "dlp") {
		t.Fatalf("a marked store must not prepend again: %s", got)
	}
	// A pair the store has never been filled for prepends, even on a marked store.
	eth := trendBot()
	eth.Pairs = []string{"BTC/USDT", "ETH/USDT"}
	j, err := r.Submit("alice", eth, "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, r, j.ID)
	if got := strings.Join(steps(), ","); !strings.HasPrefix(got, "dlp,") {
		t.Fatalf("a new pair must prepend: %s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "candles", "binance", ".wickfleet-since-ETH_USDT-1d")); err != nil {
		t.Fatal("the new pair's marker is missing")
	}
	// Moving binance back to 2020 fills in once more; kraken keeps its own start.
	r.Cfg.SinceBy = map[string]time.Time{"binance": time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "kraken": time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	run()
	if got := strings.Join(steps(), ","); !strings.HasPrefix(got, "dlp,") {
		t.Fatalf("an earlier start must prepend: %s", got)
	}
	k := trendBot()
	k.Exchange = "kraken"
	if _, err := r.Submit("alice", k, "2023-01-01", "2026-10-18"); err == nil || !strings.Contains(err.Error(), "kraken candle store starts 2025-01-01") {
		t.Fatalf("kraken before its start: %v", err)
	}
	if _, err := r.Submit("alice", trendBot(), "2021-01-01", "2026-10-18"); err != nil {
		t.Fatalf("binance from 2021: %v", err)
	}
}

func TestParseSince(t *testing.T) {
	def, by, err := ParseSince("2020-01-01, Kraken=2025-01-01")
	if err != nil || def.Format("2006-01-02") != "2020-01-01" || by["kraken"].Format("2006-01-02") != "2025-01-01" {
		t.Fatalf("%v %v %v", def, by, err)
	}
	if d, _, err := ParseSince("2025-01-01"); err != nil || d.Year() != 2025 {
		t.Fatalf("plain date: %v %v", d, err)
	}
	for _, bad := range []string{"", "kraken=2025-01-01", "2020-13-01", "2020-01-01,kraken=x"} {
		if _, _, err := ParseSince(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	c := Config{Since: def, SinceBy: by}
	if c.SinceFor("KRAKEN").Year() != 2025 || c.SinceFor("binance").Year() != 2020 {
		t.Fatal("SinceFor")
	}
}

func TestSubmitSourceUsesGivenCode(t *testing.T) {
	r, _, root := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b := trendBot()
	b.Strategy, b.StrategyFile = "Kid", "Kid.py" // not in the registry
	if _, err := r.Submit("alice", b, "2025-10-18", "2026-10-18"); err == nil {
		t.Fatal("a bot without its file in the registry was queued")
	}
	src := []byte("class Kid:\n    timeframe = \"1d\"\n")
	j, err := r.SubmitSource("alice", b, src, "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, r, j.ID)
	got, err := os.ReadFile(filepath.Join(root, "jobs", j.ID, "strategies", "Kid.py"))
	if err != nil || string(got) != string(src) {
		t.Fatalf("job strategy %q %v", got, err)
	}
}
