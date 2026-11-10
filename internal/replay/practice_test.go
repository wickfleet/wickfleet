package replay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/guard"
)

var practiceSrc = []byte("class EmaCross:\n    timeframe = \"15m\"\n")

// Guard 7: with the practice lane full and held, a birth gate and a forward
// record are still accepted and finish.
func TestPracticeNeverBlocksABirth(t *testing.T) {
	r, f, root := setup(t)
	r.Cfg.PracticeMem = 768 << 20
	hold := make(chan struct{})
	f.hold[filepath.Join(root, "candles", "practice", "binance")+":/candles/binance"] = hold
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var practices []string
	for {
		j, err := r.SubmitPractice("learn", trendBot(), practiceSrc, "2026-07-20", "2026-10-18")
		if errors.Is(err, ErrBusy) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		practices = append(practices, j.ID)
		if len(practices) == 1 {
			time.Sleep(100 * time.Millisecond) // the first one takes the slot; the rest queue
		}
	}
	if len(practices) != 1+r.Cfg.MaxQueued {
		t.Fatalf("practice lane took %d before busy, want %d", len(practices), 1+r.Cfg.MaxQueued)
	}
	gate, err := r.SubmitSource("ctl", trendBot(), []byte("class EmaCross:\n    timeframe = \"1d\"\n"), "2025-10-18", "2026-10-17")
	if err != nil {
		t.Fatalf("a birth gate behind a full practice lane: %v", err)
	}
	forward, err := r.Submit("ctl", trendBot(), "2026-09-01", "2026-10-17")
	if err != nil {
		t.Fatalf("a forward record behind a full practice lane: %v", err)
	}
	for _, id := range []string{gate.ID, forward.ID} {
		if got := wait(t, r, id); got.State != Done {
			t.Fatalf("%+v", got)
		}
	}
	for _, id := range practices {
		if j, _ := r.Get(id); j.State == Done || j.State == Failed {
			t.Fatalf("practice %s finished while held: %+v", id, j)
		}
	}
	close(hold)
	for _, id := range practices {
		if got := wait(t, r, id); got.State != Done || got.Summary == nil || got.Summary.Trades != 11 {
			t.Fatalf("%+v", got)
		}
	}
	if trades, err := r.Trades(practices[0]); err != nil || len(trades) != 11 {
		t.Fatalf("practice trades %v %v", trades, err)
	}
	// Practice containers: own store, JSON candles, lowest CPU weight, own memory cap, no bias steps.
	f.mu.Lock()
	defer f.mu.Unlock()
	steps := map[string]int{}
	for id, s := range f.specs {
		name := f.names[id]
		job := strings.TrimPrefix(name, "wickfleet-replay-")
		job = job[:strings.LastIndex(job, "-")]
		if !strings.Contains(strings.Join(practices, ","), job) {
			if s.HostConfig.CPUShares != 0 || s.HostConfig.Binds[1] != filepath.Join(root, "candles", "binance")+":/candles/binance" {
				t.Errorf("replay step %s: %+v", name, s.HostConfig)
			}
			continue
		}
		steps[f.step(id)]++
		if s.HostConfig.CPUShares != 2 || s.HostConfig.Memory != 768<<20 ||
			s.HostConfig.Binds[1] != filepath.Join(root, "candles", "practice", "binance")+":/candles/binance" {
			t.Errorf("practice step %s: %+v", name, s.HostConfig)
		}
	}
	if steps["dl"] != len(practices) || steps["bt"] != len(practices) || len(steps) != 2 {
		t.Fatalf("practice steps %v", steps)
	}
	cfg, _ := os.ReadFile(filepath.Join(root, "jobs", practices[0], "config.json"))
	if !strings.Contains(string(cfg), `"dataformat_ohlcv": "json"`) {
		t.Fatalf("practice config keeps feather: %s", cfg)
	}
}

// The practice download starts 30 days before the window; it never prepends.
func TestPracticeFetch(t *testing.T) {
	r, f, root := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.FetchPractice("binance", "freqtrade", []string{"BTC/USDT"}, []string{"15m"}, "2026-07-20")
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, r, j.ID); got.State != Done {
		t.Fatalf("%+v", got)
	}
	if r.PracticePending("practice-fetch") {
		t.Fatal("pending after done")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) != 1 || !strings.HasSuffix(f.order[0], "-dl") {
		t.Fatalf("steps %v", f.order)
	}
	for _, s := range f.specs {
		cmd := strings.Join(s.Cmd, " ")
		if !strings.Contains(cmd, "--timerange 20260620-") || strings.Contains(cmd, "--prepend") ||
			s.HostConfig.Binds[1] != filepath.Join(root, "candles", "practice", "binance")+":/candles/binance" {
			t.Fatalf("fetch %q %v", cmd, s.HostConfig.Binds)
		}
	}
}

func TestSubmitPracticeRejects(t *testing.T) {
	r, _, _ := setup(t)
	r.init(context.Background())
	badImg := trendBot()
	badImg.Image = "alpine"
	for name, c := range map[string]struct {
		from, to string
		src      []byte
	}{
		"reversed": {"2026-10-18", "2026-07-20", practiceSrc},
		"future":   {"2026-07-20", "2026-12-01", practiceSrc},
		"bad date": {"2026-7-20", "2026-10-18", practiceSrc},
		"no code":  {"2026-07-20", "2026-10-18", nil},
		"no tf":    {"2026-07-20", "2026-10-18", []byte("class X:\n    pass\n")},
	} {
		if _, err := r.SubmitPractice("learn", trendBot(), c.src, c.from, c.to); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := r.SubmitPractice("learn", badImg, practiceSrc, "2026-07-20", "2026-10-18"); err == nil {
		t.Error("image accepted")
	}
}

// Practice step containers pass the docker guard as production configures it.
func TestPracticeStepsPassTheDockerGuard(t *testing.T) {
	r, f, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.SubmitPractice("learn", trendBot(), practiceSrc, "2026-07-20", "2026-10-18")
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
	if len(f.specs) != 2 {
		t.Fatalf("checked %d step specs", len(f.specs))
	}
}

// Finished practice runs are pruned among themselves, never against replays.
func TestPrunePerKind(t *testing.T) {
	r, _, _ := setup(t)
	r.Cfg.KeepJobs = 2
	r.init(context.Background())
	add := func(kind string, i int) string {
		id := "r20261018-00000" + string(rune('0'+i)) + "-" + kind
		r.jobs[id] = &Job{ID: id, Kind: kind, State: Done}
		return id
	}
	forward := add("replay", 0)
	for i := 1; i <= 5; i++ {
		add("practice", i)
	}
	r.prune()
	if _, ok := r.Get(forward); !ok {
		t.Fatal("practice runs pruned a forward record")
	}
	n := 0
	for _, j := range r.List() {
		if j.Kind == "practice" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("kept %d practice runs, want 2", n)
	}
}

func TestPracticeCandles(t *testing.T) {
	r, _, root := setup(t)
	dir := filepath.Join(root, "candles", "practice", "binance")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "BTC_USDT-15m.json"), []byte(`[[1760000000000,100.5,101,99,100.75,12.5],[1760000900000,100.75,102,100,101.5,8]]`), 0o644)
	cs, err := r.PracticeCandles("binance", "BTC/USDT", "15m")
	if err != nil || len(cs) != 2 || cs[0].Time != 1760000000 || cs[1].Close != 101.5 || cs[0].Volume != 12.5 {
		t.Fatalf("%+v %v", cs, err)
	}
	raw, _ := json.Marshal(cs[0])
	if string(raw) != `[1760000000,100.5,101,99,100.75,12.5]` {
		t.Fatalf("candle json %s", raw)
	}
	if _, err := r.PracticeCandles("binance", "ETH/USDT", "15m"); err == nil {
		t.Fatal("a missing file read as candles")
	}
	for _, bad := range []string{`{}`, `[[1,2,3]]`, `[[1,2,3,4,5,"x"]]`} {
		if _, err := parseCandles([]byte(bad)); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}
