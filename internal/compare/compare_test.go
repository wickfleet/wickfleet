package compare

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

// fakeReplays plays the runner: each Get moves a job one state on
// (queued, testing, done) and records what was still queued at each Submit.
type fakeReplays struct {
	mu      sync.Mutex
	jobs    map[string]*replay.Job
	order   []string
	busy    int               // ErrBusy answers left
	fail    map[string]string // bot -> replay error
	refuse  map[string]string // bot -> Submit error
	queued  []int             // jobs still queued when Submit was called
	trades  []replay.Trade
	summary replay.Summary
}

func (f *fakeReplays) Submit(owner string, b registry.Bot, from, to string) (*replay.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy > 0 {
		f.busy--
		return nil, replay.ErrBusy
	}
	if msg := f.refuse[b.Name]; msg != "" {
		return nil, errors.New(msg)
	}
	q := 0
	for _, j := range f.jobs {
		if j.State == replay.Queued {
			q++
		}
	}
	f.queued = append(f.queued, q)
	j := &replay.Job{ID: fmt.Sprintf("r%d", len(f.order)), Kind: "replay", Owner: owner, Bot: b.Name, From: from, To: to, State: replay.Queued}
	if f.jobs == nil {
		f.jobs = map[string]*replay.Job{}
	}
	f.jobs[j.ID] = j
	f.order = append(f.order, b.Name)
	cp := *j
	return &cp, nil
}

func (f *fakeReplays) Get(id string) (replay.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return replay.Job{}, false
	}
	switch j.State {
	case replay.Queued:
		j.State = replay.Testing
	case replay.Testing:
		if msg := f.fail[j.Bot]; msg != "" {
			j.State, j.Error = replay.Failed, msg
		} else {
			sum := f.summary
			j.State, j.Summary = replay.Done, &sum
			j.Robust = &replay.Robustness{Pass: true, Windows: make([]replay.Window, 6), Profitable: 4,
				MonteCarlo: &replay.MonteCarlo{P95DrawdownPct: 18.5, RuinPct: 0.4, Judged: true}}
			j.Bias = &replay.Bias{Lookahead: "clean", Recursive: "clean"}
		}
	}
	return *j, true
}

func (f *fakeReplays) Trades(string) ([]replay.Trade, error) { return f.trades, nil }

func bots(names ...string) []registry.Bot {
	var out []registry.Bot
	for _, n := range names {
		out = append(out, registry.Bot{Name: n, Owner: "alice", Exchange: "binance", Strategy: "S"})
	}
	return out
}

func service(t *testing.T, r Replays) *Service {
	t.Helper()
	s := &Service{Replays: r, Dir: t.TempDir(), Poll: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Wait() })
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func wait(t *testing.T, s *Service, id string) Comparison {
	t.Helper()
	for range 5000 {
		if c, _ := s.Get(id); c.State != Running {
			return c
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("comparison never finished")
	return Comparison{}
}

func TestRunQueuesOneAtATime(t *testing.T) {
	fr := &fakeReplays{busy: 2, summary: replay.Summary{StartBalance: 1000, Trades: 2, Wins: 1, ProfitPct: 1.5},
		trades: []replay.Trade{{Close: "2026-01-02 10:00:00+00:00", ProfitNet: 20}, {Close: "2026-01-04 10:00:00+00:00", ProfitNet: -5}}}
	s := service(t, fr)
	c, err := s.Submit("alice", bots("a", "b", "c", "d"), "2026-01-01", "2026-01-05")
	if err != nil {
		t.Fatal(err)
	}
	got := wait(t, s, c.ID)
	if got.State != Done || strings.Join(fr.order, "") != "abcd" {
		t.Fatalf("state %s, order %v", got.State, fr.order)
	}
	// Every Submit found nothing of ours still queued: one queue place at most.
	for i, q := range fr.queued {
		if q != 0 {
			t.Fatalf("submit %d saw %d queued jobs, want 0", i, q)
		}
	}
	for _, r := range got.Rows {
		if r.State != replay.Done || r.Metrics == nil || len(r.Equity) != 5 || r.Equity[0].Date != "2026-01-01" || r.Equity[4].Date != "2026-01-05" {
			t.Fatalf("row %+v", r)
		}
		if *r.Equity[4].Equity != 1015 || r.Metrics.WindowsPassed != 4 || r.Metrics.Verdict != "pass" {
			t.Fatalf("row %s: %+v %+v", r.Bot, r.Equity[4], r.Metrics)
		}
	}
	// The file on disk is the comparison.
	if _, err := os.Stat(filepath.Join(s.Dir, c.ID+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReplayIsReportedNotZero(t *testing.T) {
	fr := &fakeReplays{fail: map[string]string{"b": "backtest: exit 2"}, refuse: map[string]string{"c": "bot c has no strategy_file"},
		summary: replay.Summary{StartBalance: 1000, Trades: 1}}
	s := service(t, fr)
	c, err := s.Submit("alice", bots("a", "b", "c"), "2026-01-01", "2026-01-05")
	if err != nil {
		t.Fatal(err)
	}
	got := wait(t, s, c.ID)
	if got.State != Done || got.Rows[0].Metrics == nil {
		t.Fatalf("%+v", got)
	}
	for _, r := range got.Rows[1:] {
		if r.State != replay.Failed || r.Error == "" || r.Metrics != nil || r.Equity != nil {
			t.Fatalf("a failed bot must carry its error and no figures: %+v", r)
		}
	}
}

func TestSubmitChecks(t *testing.T) {
	s := service(t, &fakeReplays{})
	s.MaxBots = 3
	for _, c := range []struct {
		bots     []registry.Bot
		from, to string
		want     string
	}{
		{bots("a"), "2026-01-01", "2026-02-01", "two or more"},
		{bots("a", "b", "c", "d"), "2026-01-01", "2026-02-01", "at most 3"},
		{bots("a", "b"), "2026-02-01", "2026-01-01", "before"},
		{bots("a", "b"), "2026-01-01", "2027-01-01", "future"},
		{bots("a", "b"), "yesterday", "2026-01-01", "from"},
	} {
		if _, err := s.Submit("alice", c.bots, c.from, c.to); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v %s %s: %v, want %q", len(c.bots), c.from, c.to, err, c.want)
		}
	}
	if _, err := (&Service{}).Submit("alice", bots("a", "b"), "2026-01-01", "2026-01-02"); err == nil {
		t.Fatal("replay off must refuse")
	}
}

// blocking never lets a job leave the queue, so comparisons stay running.
type blocking struct{ fakeReplays }

func (b *blocking) Get(id string) (replay.Job, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return replay.Job{}, false
	}
	return *j, true
}

func TestMaxActive(t *testing.T) {
	s := service(t, &blocking{})
	s.MaxActive = 1
	if _, err := s.Submit("alice", bots("a", "b"), "2026-01-01", "2026-01-05"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit("bob", bots("c", "d"), "2026-01-01", "2026-01-05"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second comparison: %v, want ErrBusy", err)
	}
}

func TestRestartFailsRunning(t *testing.T) {
	dir := t.TempDir()
	raw := `{"id":"c1","owner":"alice","from":"2026-01-01","to":"2026-01-05","state":"running",
		"bots":[{"bot":"a","owner":"alice","state":"done"},{"bot":"b","owner":"alice","state":"testing","replay":"r1"}]}`
	os.WriteFile(filepath.Join(dir, "c1.json"), []byte(raw), 0o600)
	s := &Service{Dir: dir}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, ok := s.Get("c1")
	if !ok || c.State != Failed || !strings.Contains(c.Error, "restarted") || c.Rows[0].State != replay.Done || c.Rows[1].State != replay.Failed {
		t.Fatalf("%+v", c)
	}
}

// TestMeasureRealResult reads the freqtrade fixture the replay package
// parses: every figure comes from the result or its trades.
func TestMeasureRealResult(t *testing.T) {
	sum, trades, err := replay.ParseResult("../replay/testdata/emacross-2023-10.zip", "EmaCross")
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2023, 10, 18, 0, 0, 0, 0, time.UTC), time.Date(2024, 10, 18, 0, 0, 0, 0, time.UTC)
	j := replay.Job{Summary: &sum, Robust: replay.Assess(sum, trades, from, to, replay.Rules{}, "x"), Bias: &replay.Bias{Lookahead: "clean"}}
	m := Measure(j, trades)
	near := func(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-3 }
	if m.Trades != 11 || m.Stake != "USDT" || math.Abs(*m.ProfitAbs-82.85888) > 1e-4 || !near(m.Sharpe, 0.058172) ||
		!near(m.Sortino, 0.278908) || !near(m.ProfitFactor, 1.280345) || !near(m.WinRatePct, 27.272727) || !near(m.AvgDurationS, 2646960) {
		t.Fatalf("%+v", m)
	}
	if m.Windows != len(j.Robust.Windows) || m.Windows == 0 || m.MCRuinPct == nil || m.Verdict != map[bool]string{true: "pass", false: "fail"}[j.Robust.Pass] {
		t.Fatalf("robustness %+v", m)
	}
	eq := Equity(sum.StartBalance, trades, "2023-10-18", "2024-10-18")
	if len(eq) != 367 || math.Abs(*eq[len(eq)-1].Equity-sum.FinalBalance) > 1e-3 {
		t.Fatalf("equity ends at %v over %d days, want the final balance %v", eq[len(eq)-1], len(eq), sum.FinalBalance)
	}
}

func TestMeasureFallbacksAndVerdict(t *testing.T) {
	sum := replay.Summary{Trades: 4, Wins: 1}
	trades := []replay.Trade{{Open: "2026-01-01 00:00:00", Close: "2026-01-01 06:00:00"}, {Open: "2026-01-02 00:00:00", Close: "2026-01-02 02:00:00"}}
	m := Measure(replay.Job{Summary: &sum, Bias: &replay.Bias{Lookahead: "biased"}, Robust: &replay.Robustness{Pass: true}}, trades)
	if m.WinRatePct == nil || *m.WinRatePct != 25 || m.AvgDurationS == nil || *m.AvgDurationS != 4*3600 || m.Verdict != "biased" {
		t.Fatalf("%+v", m)
	}
	if m.Sharpe != nil || m.Sortino != nil || m.ProfitFactor != nil {
		t.Fatal("ratios the result does not carry must stay nil")
	}
	if Measure(replay.Job{}, nil).Verdict != "unknown" || Measure(replay.Job{}, nil).WinRatePct != nil {
		t.Fatal("no robustness: unknown, no trades: no win rate")
	}
}
