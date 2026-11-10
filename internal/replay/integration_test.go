package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wickfleet/internal/docker"
	"wickfleet/internal/registry"
)

// TestIntegration runs a real replay: freqtrade 2026.9 through a Docker
// socket proxy, Binance daily candles from the internet. Opt in with
// WICKFLEET_IT_DOCKER=http://127.0.0.1:2375 and WICKFLEET_IT_DIR=<dir Docker can bind>.
func TestIntegration(t *testing.T) {
	base, dir := os.Getenv("WICKFLEET_IT_DOCKER"), os.Getenv("WICKFLEET_IT_DIR")
	if base == "" || dir == "" {
		t.Skip("WICKFLEET_IT_DOCKER and WICKFLEET_IT_DIR not set")
	}
	strat := filepath.Join(dir, "strategies")
	os.MkdirAll(strat, 0o755)
	src, err := os.ReadFile("../../examples/registry/strategies/EmaCross.py")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(strat, "EmaCross.py"), src, 0o644)
	r := &Runner{Docker: docker.Client{Base: base}, Log: t.Logf, Cfg: Config{
		Images:     map[string]string{"freqtrade": "freqtradeorg/freqtrade:2026.9"},
		Strategies: strat, CandleDir: filepath.Join(dir, "candles"), JobDir: filepath.Join(dir, "jobs"),
		Network: "bridge", BotUser: os.Getenv("WICKFLEET_IT_USER"),
		Since:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Memory: 1536 << 20, Timeout: 10 * time.Minute, MaxQueued: 2,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	b := registry.Bot{Name: "trend", Owner: "it", Strategy: "EmaCross", StrategyFile: "EmaCross.py",
		Exchange: "binance", Pairs: []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}, StakeCurrency: "USDT",
		Wallet: 1000, MaxOpenTrades: 3, Image: "freqtrade"}
	fee := 0.008
	b.Fee = &fee
	j, err := r.Submit("it", b, "2025-10-18", "2026-10-18")
	if err != nil {
		t.Fatal(err)
	}
	var got Job
	for i := 0; i < 600; i++ {
		got, _ = r.Get(j.ID)
		if got.State == Done || got.State == Failed {
			break
		}
		time.Sleep(time.Second)
	}
	if got.State != Done || got.Summary == nil {
		t.Fatalf("%+v", got)
	}
	t.Logf("summary %+v", *got.Summary)
	if got.Summary.Trades == 0 || got.Summary.Fees <= 0 || got.Summary.Stake != "USDT" {
		t.Fatalf("implausible %+v", *got.Summary)
	}
	// Both analyses ran inside the same containers setup and gave a verdict.
	if got.Bias == nil || got.Bias.Lookahead == "" || got.Bias.Recursive == "" {
		t.Fatalf("no bias verdict: %+v", got.Bias)
	}
	t.Logf("bias %+v", *got.Bias)
	if got.Bias.Lookahead == "biased" || got.Bias.Recursive != "clean" {
		t.Fatalf("EmaCross must not look biased: %+v", *got.Bias)
	}
	if cs, _ := (docker.Client{Base: base}).Labeled(ctx, "replay"); len(cs) != 0 {
		t.Fatalf("job containers left behind: %+v", cs)
	}
}
