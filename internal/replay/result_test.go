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
