package replay

import (
	"strings"
	"testing"
	"time"
)

var (
	wfFrom = time.Date(2025, 10, 19, 0, 0, 0, 0, time.UTC)
	wfTo   = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
)

// tradesEvery closes one trade per day from wfFrom with the given profit
// for each 30-day window counted from the replay end, oldest first.
func tradesEvery(perWindow ...float64) []Trade {
	var out []Trade
	n := len(perWindow)
	for i, p := range perWindow {
		ws := wfTo.Add(-time.Duration(n-i) * 30 * 24 * time.Hour)
		for d := 0; d < 3; d++ {
			out = append(out, Trade{Close: ws.Add(time.Duration(d*24+12) * time.Hour).Format("2006-01-02 15:04:05-07:00"),
				ProfitNet: p / 3})
		}
	}
	return out
}

func sumOf(trades []Trade, start, market float64) Summary {
	s := Summary{StartBalance: start, MarketPct: market}
	for _, t := range trades {
		s.ProfitNet += t.ProfitNet
	}
	s.ProfitPct = s.ProfitNet / start * 100
	return s
}

func TestAssessPassesASteadyStrategy(t *testing.T) {
	tr := tradesEvery(20, -10, 30, 15, -5, 25, 10, 20, -8, 12, 18, 5)
	r := Assess(sumOf(tr, 1000, 10), tr, wfFrom, wfTo, Rules{}, "job1")
	if !r.Pass || len(r.Windows) != 12 || r.Profitable != 9 {
		t.Fatalf("%+v", r)
	}
	if r.Windows[0].Trades != 3 || r.Windows[0].ProfitPct != 2 || r.Windows[11].To != "2026-10-19" {
		t.Fatalf("windows %+v", r.Windows)
	}
	// The second window's base is the equity after the first: 1000 + 20.
	if r.Windows[1].ProfitPct != round2(-10.0/1020*100) {
		t.Fatalf("window 2 %+v", r.Windows[1])
	}
	if r.VsHoldPct != round2(sumOf(tr, 1000, 10).ProfitPct-10) {
		t.Fatalf("vs hold %v", r.VsHoldPct)
	}
	if r.MonteCarlo == nil || !r.MonteCarlo.Judged || r.MonteCarlo.Runs != 10000 {
		t.Fatalf("mc %+v", r.MonteCarlo)
	}
}

func TestAssessReasons(t *testing.T) {
	cases := map[string]struct {
		trades []Trade
		from   time.Time
		want   string
	}{
		"short":      {tradesEvery(10, 10, 10), wfTo.Add(-100 * 24 * time.Hour), "covers 3 full 30-day windows; at least 6"},
		"losing":     {tradesEvery(-5, -5, -5, -5, -5, 1, 1, 1, 1, 1, 1, 1), wfFrom, "loses money"},
		"few wins":   {tradesEvery(100, -5, -5, -5, -5, -5, -5, -5, 5, 5, 5, 5), wfFrom, "profitable in 5 of 12 windows; at least 6"},
		"worst":      {tradesEvery(80, 40, 40, -250, 40, 40, 40, 40, 40, 40, 40, 40), wfFrom, "worst window"},
		"one window": {tradesEvery(10), wfTo.Add(-31 * 24 * time.Hour), "at least 6 needed"},
	}
	for name, c := range cases {
		r := Assess(sumOf(c.trades, 1000, 0), c.trades, c.from, wfTo, Rules{}, name)
		if r.Pass || !strings.Contains(strings.Join(r.Reasons, "; "), c.want) {
			t.Errorf("%s: pass=%v reasons=%v", name, r.Pass, r.Reasons)
		}
	}
}

func TestMonteCarloRuinAndDeterminism(t *testing.T) {
	// Big swings on a small account: many resamples sink below half.
	var tr []Trade
	for i := 0; i < 40; i++ {
		p := 150.0
		if i%2 == 0 {
			p = -140
		}
		tr = append(tr, Trade{Close: wfTo.Add(-time.Duration(i) * 24 * time.Hour).Format("2006-01-02 15:04:05-07:00"), ProfitNet: p})
	}
	a := Assess(sumOf(tr, 1000, 0), tr, wfFrom, wfTo, Rules{}, "seed")
	b := Assess(sumOf(tr, 1000, 0), tr, wfFrom, wfTo, Rules{}, "seed")
	if *a.MonteCarlo != *b.MonteCarlo {
		t.Fatal("same seed must give the same Monte Carlo")
	}
	if a.MonteCarlo.RuinPct <= 5 || !strings.Contains(strings.Join(a.Reasons, "; "), "resamples reach a 50% drawdown") {
		t.Fatalf("ruin not judged: %+v %v", a.MonteCarlo, a.Reasons)
	}
	if a.MonteCarlo.P95DrawdownPct < a.MonteCarlo.MedianDrawdownPct {
		t.Fatalf("p95 drawdown below median: %+v", a.MonteCarlo)
	}
}

func TestMonteCarloNotJudgedOnFewTrades(t *testing.T) {
	tr := []Trade{{Close: "2026-10-01 00:00:00+00:00", ProfitNet: -600}, {Close: "2026-10-02 00:00:00+00:00", ProfitNet: 650}}
	r := Assess(sumOf(tr, 1000, 0), tr, wfFrom, wfTo, Rules{}, "few")
	if r.MonteCarlo == nil || r.MonteCarlo.Judged || strings.Contains(strings.Join(r.Reasons, "; "), "resamples") {
		t.Fatalf("%+v %v", r.MonteCarlo, r.Reasons)
	}
}

// The real export: trades close as "2024-08-29 00:00:00+00:00".
func TestAssessReadsRealResult(t *testing.T) {
	sum, trades, err := ParseResult("testdata/emacross-2023-10.zip", "EmaCross")
	if err != nil {
		t.Fatal(err)
	}
	r := Assess(sum, trades, time.Date(2023, 10, 18, 0, 0, 0, 0, time.UTC), time.Date(2024, 10, 18, 0, 0, 0, 0, time.UTC), Rules{}, "real")
	got := 0
	for _, w := range r.Windows {
		got += w.Trades
	}
	if got != len(trades) || got == 0 {
		t.Fatalf("windows hold %d of %d trades: %+v", got, len(trades), r.Windows)
	}
}
