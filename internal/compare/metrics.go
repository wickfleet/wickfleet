package compare

import (
	"math"
	"time"

	"wickfleet/internal/replay"
)

// Metrics is one bot's replay, side by side with the others. Every figure is
// read from the replay's result or derived from its trades. Nil means the
// result does not carry it.
type Metrics struct {
	Stake          string   `json:"stake"`
	StartBalance   *float64 `json:"start_balance,omitempty"` // money: nil for viewers
	FinalBalance   *float64 `json:"final_balance,omitempty"`
	ProfitPct      float64  `json:"profit_pct"`
	ProfitAbs      *float64 `json:"profit_abs,omitempty"`
	MarketPct      float64  `json:"market_pct"`
	MaxDrawdownPct float64  `json:"max_drawdown_pct"`
	Sharpe         *float64 `json:"sharpe,omitempty"`
	Sortino        *float64 `json:"sortino,omitempty"`
	Calmar         *float64 `json:"calmar,omitempty"`
	ProfitFactor   *float64 `json:"profit_factor,omitempty"`
	WinRatePct     *float64 `json:"win_rate_pct,omitempty"`
	Trades         int      `json:"trades"`
	AvgDurationS   *float64 `json:"avg_duration_s,omitempty"`
	Windows        int      `json:"windows"`
	WindowsPassed  int      `json:"windows_passed"`
	MCP95DDPct     *float64 `json:"mc_p95_drawdown_pct,omitempty"`
	MCRuinPct      *float64 `json:"mc_ruin_pct,omitempty"`
	MCJudged       bool     `json:"mc_judged"`
	Verdict        string   `json:"verdict"` // pass | fail | biased | unknown
	Reasons        []string `json:"reasons,omitempty"`
}

// Point is the account at the end of one day.
type Point struct {
	Date      string   `json:"date"`
	Equity    *float64 `json:"equity,omitempty"` // in the stake currency, nil for viewers
	ReturnPct float64  `json:"return_pct"`
}

func fp(v float64) *float64 { return &v }

// Measure reads a finished replay. Win rate and average duration fall back
// to the trades when freqtrade's own figures are missing.
func Measure(j replay.Job, trades []replay.Trade) Metrics {
	s := j.Summary
	if s == nil {
		s = &replay.Summary{}
	}
	m := Metrics{Stake: s.Stake, StartBalance: fp(s.StartBalance), FinalBalance: fp(s.FinalBalance),
		ProfitPct: s.ProfitPct, ProfitAbs: fp(s.ProfitNet), MarketPct: s.MarketPct, MaxDrawdownPct: s.MaxDrawdownPc,
		Sharpe: s.Sharpe, Sortino: s.Sortino, Calmar: s.Calmar, ProfitFactor: s.ProfitFactor,
		WinRatePct: s.WinRatePct, Trades: s.Trades, AvgDurationS: s.HoldingAvgS}
	if m.WinRatePct == nil && s.Trades > 0 {
		m.WinRatePct = fp(float64(s.Wins) / float64(s.Trades) * 100)
	}
	if m.AvgDurationS == nil {
		var sum float64
		n := 0
		for _, t := range trades {
			o, ok1 := replay.ParseTradeTime(t.Open)
			c, ok2 := replay.ParseTradeTime(t.Close)
			if ok1 && ok2 {
				sum += c.Sub(o).Seconds()
				n++
			}
		}
		if n > 0 {
			m.AvgDurationS = fp(sum / float64(n))
		}
	}
	if r := j.Robust; r != nil {
		m.Windows, m.WindowsPassed, m.Reasons = len(r.Windows), r.Profitable, r.Reasons
		if mc := r.MonteCarlo; mc != nil {
			m.MCP95DDPct, m.MCRuinPct, m.MCJudged = fp(mc.P95DrawdownPct), fp(mc.RuinPct), mc.Judged
		}
	}
	switch {
	case j.Bias.Blocks():
		m.Verdict = "biased"
	case j.Robust == nil:
		m.Verdict = "unknown"
	case j.Robust.Pass:
		m.Verdict = "pass"
	default:
		m.Verdict = "fail"
	}
	return m
}

// Equity is the account at the end of each day from from to to, both
// included, counting each trade on the day it closed. Every bot of one
// comparison gets the same dates, so the curves overlay.
func Equity(start float64, trades []replay.Trade, from, to string) []Point {
	f, err1 := time.Parse("2006-01-02", from)
	t, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || t.Before(f) {
		return nil
	}
	days := int(t.Sub(f)/(24*time.Hour)) + 1
	daily := make([]float64, days)
	for _, tr := range trades {
		c, ok := replay.ParseTradeTime(tr.Close)
		if !ok {
			continue
		}
		i := int(c.Truncate(24*time.Hour).Sub(f) / (24 * time.Hour))
		switch {
		case i < 0:
			i = 0
		case i >= days:
			i = days - 1
		}
		daily[i] += tr.ProfitNet
	}
	out := make([]Point, days)
	eq := start
	for i := range out {
		eq += daily[i]
		p := Point{Date: f.AddDate(0, 0, i).Format("2006-01-02"), Equity: fp(round(eq, 4))}
		if start > 0 {
			p.ReturnPct = round((eq-start)/start*100, 4)
		}
		out[i] = p
	}
	return out
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
