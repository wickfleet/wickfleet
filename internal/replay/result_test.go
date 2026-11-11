package replay

import (
	"math"
	"testing"
	"time"
)

// testdata/emacross-2023-10.zip is a real freqtrade 2026.9 result: EmaCross,
// BTC/ETH/SOL USDT on Binance 1d, fee 0.008, 2023-10-18 to 2024-10-18.
func TestParseResult(t *testing.T) {
	s, trades, err := ParseResult("testdata/emacross-2023-10.zip", "EmaCross")
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-4 }
	if s.Trades != 11 || len(trades) != 11 || s.Stake != "USDT" || s.StartBalance != 1000 {
		t.Fatalf("%+v", s)
	}
	if !near(s.ProfitNet, 82.85888) || !near(s.FinalBalance, 1082.85888) || !near(s.Fees, 70.161445) {
		t.Fatalf("money %+v", s)
	}
	// Gross is what the price moves paid before fees: sum of amount * (close - open).
	if !near(s.ProfitGross, 153.020324) {
		t.Fatalf("gross %v", s.ProfitGross)
	}
	var fees float64
	for _, tr := range trades {
		fees += tr.Fee
		if tr.Pair == "" || tr.Open == "" || tr.ExitReason == "" || tr.Fee <= 0 {
			t.Fatalf("trade %+v", tr)
		}
	}
	if !near(fees, s.Fees) {
		t.Fatalf("trade fees %v != summary %v", fees, s.Fees)
	}
	// freqtrade's own ratios ride along unchanged, absent ones stay nil.
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"sharpe": {s.Sharpe, 0.058172}, "sortino": {s.Sortino, 0.278908}, "calmar": {s.Calmar, 2.248058},
		"profit_factor": {s.ProfitFactor, 1.280345}, "winrate": {s.WinRatePct, 27.272727}, "holding_avg_s": {s.HoldingAvgS, 2646960},
	} {
		if c.got == nil || !near(*c.got, c.want) {
			t.Errorf("%s: %v, want %v", name, c.got, c.want)
		}
	}
	if s2, _, _ := summarize("X", ftStrategy{TotalTrades: 0}); s2.Sharpe != nil || s2.WinRatePct != nil || s2.HoldingAvgS != nil {
		t.Fatalf("absent ratios must stay nil: %+v", s2)
	}
	// freqtrade writes -100 when a ratio's denominator is zero: no figure, not a terrible one.
	undef := -100.0
	if s3, _, _ := summarize("X", ftStrategy{Sharpe: &undef, Sortino: &undef, Calmar: &undef, ProfitFactor: &undef}); s3.Sharpe != nil || s3.Sortino != nil || s3.Calmar != nil || s3.ProfitFactor == nil {
		t.Fatalf("-100 sentinel: %+v", s3)
	}
	if _, _, err := ParseResult("testdata/emacross-2023-10.zip", "Other"); err == nil {
		t.Fatal("a strategy not in the result must fail")
	}
	if _, _, err := ParseResult("testdata/missing.zip", "EmaCross"); err == nil {
		t.Fatal("a missing file must fail")
	}
}

func TestParseTradeTime(t *testing.T) {
	want := time.Date(2025, 10, 20, 13, 0, 0, 0, time.UTC)
	for _, s := range []string{"2025-10-20 13:00:00+00:00", "2025-10-20 15:00:00+02:00", "2025-10-20T13:00:00Z", " 2025-10-20 13:00:00 "} {
		if got, ok := ParseTradeTime(s); !ok || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("%q: %v %v", s, got, ok)
		}
	}
	if _, ok := ParseTradeTime("yesterday"); ok {
		t.Error("parsed a non-time")
	}
}
