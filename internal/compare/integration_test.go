package compare

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

// TestIntegration compares four bots over one window through the real replay
// runner: freqtrade in Docker behind a socket proxy, Binance daily candles from
// the internet. Opt in like the replay integration test, with
// WICKFLEET_IT_DOCKER=http://127.0.0.1:2375 and WICKFLEET_IT_DIR=<dir Docker can bind>.
func TestIntegration(t *testing.T) {
	base, dir := os.Getenv("WICKFLEET_IT_DOCKER"), os.Getenv("WICKFLEET_IT_DIR")
	if base == "" || dir == "" {
		t.Skip("WICKFLEET_IT_DOCKER and WICKFLEET_IT_DIR not set")
	}
	image := os.Getenv("WICKFLEET_IT_IMAGE")
	if image == "" {
		image = "freqtradeorg/freqtrade:2026.9"
	}
	strat := filepath.Join(dir, "strategies")
	os.MkdirAll(strat, 0o755)
	for _, f := range []string{"EmaCross.py", "RsiReversion.py"} {
		src, err := os.ReadFile("../../examples/registry/strategies/" + f)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(strat, f), src, 0o644)
	}
	r := &replay.Runner{Docker: docker.Client{Base: base}, Log: t.Logf, Cfg: replay.Config{
		Images:     map[string]string{"freqtrade": image},
		Strategies: strat, CandleDir: filepath.Join(dir, "candles"), JobDir: filepath.Join(dir, "jobs"),
		Network: "bridge", BotUser: os.Getenv("WICKFLEET_IT_USER"),
		Since:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Memory: 1536 << 20, Timeout: 10 * time.Minute, MaxQueued: 5,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s := &Service{Replays: r, Dir: filepath.Join(dir, "comparisons"), Log: t.Logf}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bot := func(name, strategy string, pairs ...string) registry.Bot {
		return registry.Bot{Name: name, Owner: "it", Strategy: strategy, StrategyFile: strategy + ".py",
			Exchange: "binance", Pairs: pairs, StakeCurrency: "USDT", Wallet: 1000, MaxOpenTrades: 3, Image: "freqtrade"}
	}
	bots := []registry.Bot{
		bot("ema-majors", "EmaCross", "BTC/USDT", "ETH/USDT", "SOL/USDT"),
		bot("rsi-majors", "RsiReversion", "BTC/USDT", "ETH/USDT", "SOL/USDT"),
		bot("ema-btc", "EmaCross", "BTC/USDT"),
		bot("rsi-eth", "RsiReversion", "ETH/USDT"),
	}
	began := time.Now()
	c, err := s.Submit("it", bots, "2025-06-01", "2026-06-01")
	if err != nil {
		t.Fatal(err)
	}
	var got Comparison
	for range 3600 {
		got, _ = s.Get(c.ID)
		if got.State != Running {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("4-bot comparison took %s", time.Since(began).Round(time.Second))
	if got.State != Done {
		t.Fatalf("%+v", got)
	}
	for _, row := range got.Rows {
		if row.State != replay.Done || row.Metrics == nil || len(row.Equity) != 366 {
			t.Fatalf("row %+v", row)
		}
		m := row.Metrics
		t.Logf("%s: profit %.2f%% dd %.2f%% sharpe %v trades %d windows %d/%d verdict %s",
			row.Bot, m.ProfitPct, m.MaxDrawdownPct, deref(m.Sharpe), m.Trades, m.WindowsPassed, m.Windows, m.Verdict)
		// Sharpe and Sortino may be absent: freqtrade's -100 marks a zero denominator.
		if m.Trades > 0 && (m.ProfitFactor == nil || m.WinRatePct == nil || m.AvgDurationS == nil) {
			t.Fatalf("freqtrade's ratios missing on %s: %+v", row.Bot, m)
		}
		if last := *row.Equity[len(row.Equity)-1].Equity; last < *m.FinalBalance-0.01 || last > *m.FinalBalance+0.01 {
			t.Fatalf("%s: equity ends at %v, final balance %v", row.Bot, last, *m.FinalBalance)
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
