package replay

import (
	"os"
	"strings"
	"testing"
)

// Fixtures are real freqtrade outputs on Binance 1d: EmaCross and a copy whose
// SMA 200 peeks 5 candles ahead (2026.10, 5 pairs, 2023-07-01..2026-11-01), and an
// EMA50 strategy with only 5 startup candles (2026.9, BTC/ETH/SOL, 2025-07-01..2026-10-18).
func TestParseLookahead(t *testing.T) {
	for file, want := range map[string]string{
		"lookahead-clean.csv":  "clean",
		"lookahead-biased.csv": "biased",
		"lookahead-toofew.csv": "inconclusive",
	} {
		got, signals, ind, err := ParseLookahead("testdata/bias/" + file)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", file, got, err)
		}
		if want == "biased" && (ind != "sma200,enter_long" || signals != 20) {
			t.Fatalf("%s: %d %q", file, signals, ind)
		}
	}
	if _, _, _, err := ParseLookahead("testdata/bias/missing.csv"); err == nil {
		t.Fatal("a missing export must be an error, not a verdict")
	}
}

func TestParseRecursive(t *testing.T) {
	clean, _ := os.ReadFile("testdata/bias/recursive-clean.log")
	if s, n := ParseRecursive(string(clean)); s != "clean" {
		t.Fatalf("%s %s", s, n)
	}
	v, _ := os.ReadFile("testdata/bias/recursive-variance.log")
	s, n := ParseRecursive(string(v))
	if s != "variance" || !strings.Contains(n, "ema50 nan%") {
		t.Fatalf("%s %q", s, n)
	}
	if s, _ := ParseRecursive("freqtrade crashed"); s != "variance" {
		t.Fatal("no table and no all-clear must not read as clean")
	}
	// A small drift at the strategy's startup is within the limit.
	small := "┃ Indicators ┃ 30 (from strategy) ┃ 199 ┃\n│ ema │ 0.050% │ 0.000% │\n"
	if s, _ := ParseRecursive(small); s != "clean" {
		t.Fatal("0.05% is within the limit")
	}
}

func TestBiasBlocks(t *testing.T) {
	var none *Bias
	if none.Blocks() || (&Bias{Lookahead: "inconclusive"}).Blocks() || !(&Bias{Lookahead: "biased"}).Blocks() {
		t.Fatal("only a biased lookahead blocks")
	}
}
