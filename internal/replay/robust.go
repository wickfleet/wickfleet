package replay

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// Rules are the robustness pass rules. Zero fields take the defaults.
type Rules struct {
	WindowDays      int     // length of one walk-forward window
	MinWindows      int     // full windows a replay must cover
	MinProfitable   float64 // share of windows with a profit
	WorstWindowPct  float64 // no window may lose more than this (negative)
	Runs            int     // Monte Carlo resamples
	MinTradesMC     int     // fewer trades: Monte Carlo is shown, not judged
	RuinDrawdownPct float64 // a drawdown this deep counts as ruin
	MaxRuinPct      float64 // share of resamples allowed to reach ruin
}

func (r Rules) withDefaults() Rules {
	def := Rules{WindowDays: 30, MinWindows: 6, MinProfitable: 0.5, WorstWindowPct: -15,
		Runs: 10000, MinTradesMC: 20, RuinDrawdownPct: 50, MaxRuinPct: 5}
	if r.WindowDays <= 0 {
		r.WindowDays = def.WindowDays
	}
	if r.MinWindows <= 0 {
		r.MinWindows = def.MinWindows
	}
	if r.MinProfitable <= 0 {
		r.MinProfitable = def.MinProfitable
	}
	if r.WorstWindowPct >= 0 {
		r.WorstWindowPct = def.WorstWindowPct
	}
	if r.Runs <= 0 {
		r.Runs = def.Runs
	}
	if r.MinTradesMC <= 0 {
		r.MinTradesMC = def.MinTradesMC
	}
	if r.RuinDrawdownPct <= 0 {
		r.RuinDrawdownPct = def.RuinDrawdownPct
	}
	if r.MaxRuinPct <= 0 {
		r.MaxRuinPct = def.MaxRuinPct
	}
	return r
}

// Window is one walk-forward slice; trades count where they close.
type Window struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Trades    int     `json:"trades"`
	ProfitPct float64 `json:"profit_pct"`
}

// MonteCarlo resamples the replay's trades to show what order and luck can do.
type MonteCarlo struct {
	Runs              int     `json:"runs"`
	MedianReturnPct   float64 `json:"median_return_pct"`
	P5ReturnPct       float64 `json:"p5_return_pct"`
	MedianDrawdownPct float64 `json:"median_drawdown_pct"`
	P95DrawdownPct    float64 `json:"p95_drawdown_pct"`
	RuinPct           float64 `json:"ruin_pct"`
	Judged            bool    `json:"judged"`
}

// Robustness is the walk-forward and Monte Carlo verdict of one replay.
// It gates live trading; a dry-run bot starts with it shown as a warning.
type Robustness struct {
	Pass       bool        `json:"pass"`
	Reasons    []string    `json:"reasons,omitempty"`
	Windows    []Window    `json:"windows"`
	Profitable int         `json:"profitable"`
	WorstPct   float64     `json:"worst_pct"`
	VsHoldPct  float64     `json:"vs_hold_pct"` // replay profit minus buy-and-hold, percentage points
	MonteCarlo *MonteCarlo `json:"monte_carlo,omitempty"`
}

// Assess splits [from, to) into full windows that end at to, older first.
// seed makes the Monte Carlo reproducible per replay.
func Assess(sum Summary, trades []Trade, from, to time.Time, rules Rules, seed string) *Robustness {
	r := rules.withDefaults()
	out := &Robustness{VsHoldPct: round2(sum.ProfitPct - sum.MarketPct)}
	span := time.Duration(r.WindowDays) * 24 * time.Hour
	n := int(to.Sub(from) / span)

	closed := make([]time.Time, len(trades))
	for i, t := range trades {
		closed[i], _ = ParseTradeTime(t.Close)
	}
	for i := 0; i < n; i++ {
		ws, we := to.Add(-time.Duration(n-i)*span), to.Add(-time.Duration(n-i-1)*span)
		equity, profit, count := sum.StartBalance, 0.0, 0
		for k, t := range trades {
			switch {
			case closed[k].Before(ws):
				equity += t.ProfitNet
			case closed[k].Before(we):
				profit += t.ProfitNet
				count++
			}
		}
		pct := 0.0
		if equity > 0 {
			pct = profit / equity * 100
		}
		w := Window{From: ws.Format("2006-01-02"), To: we.Format("2006-01-02"), Trades: count, ProfitPct: round2(pct)}
		out.Windows = append(out.Windows, w)
		if w.ProfitPct > 0 {
			out.Profitable++
		}
		if i == 0 || w.ProfitPct < out.WorstPct {
			out.WorstPct = w.ProfitPct
		}
	}

	if n < r.MinWindows {
		out.Reasons = append(out.Reasons, fmt.Sprintf("the replay covers %d full %d-day windows; at least %d needed",
			n, r.WindowDays, r.MinWindows))
	}
	if sum.ProfitNet <= 0 {
		out.Reasons = append(out.Reasons, fmt.Sprintf("loses money over the replay (%.2f%%)", sum.ProfitPct))
	}
	if need := int(math.Ceil(r.MinProfitable * float64(n))); n > 0 && out.Profitable < need {
		out.Reasons = append(out.Reasons, fmt.Sprintf("profitable in %d of %d windows; at least %d needed",
			out.Profitable, n, need))
	}
	if n > 0 && out.WorstPct < r.WorstWindowPct {
		out.Reasons = append(out.Reasons, fmt.Sprintf("worst window %.2f%% (limit %.0f%%)", out.WorstPct, r.WorstWindowPct))
	}
	if mc := monteCarlo(sum.StartBalance, trades, r, seed); mc != nil {
		out.MonteCarlo = mc
		if mc.Judged && mc.RuinPct > r.MaxRuinPct {
			out.Reasons = append(out.Reasons, fmt.Sprintf("%.1f%% of %d resamples reach a %.0f%% drawdown (limit %.0f%%)",
				mc.RuinPct, mc.Runs, r.RuinDrawdownPct, r.MaxRuinPct))
		}
	}
	out.Pass = len(out.Reasons) == 0
	return out
}

// monteCarlo draws len(trades) trades with replacement per run and tracks
// the account: final return and deepest drawdown from the running peak.
func monteCarlo(start float64, trades []Trade, r Rules, seed string) *MonteCarlo {
	if len(trades) == 0 || start <= 0 {
		return nil
	}
	h := fnv.New64a()
	h.Write([]byte(seed))
	rng := rand.New(rand.NewPCG(h.Sum64(), uint64(len(trades))))
	rets := make([]float64, r.Runs)
	dds := make([]float64, r.Runs)
	ruin := 0
	for run := 0; run < r.Runs; run++ {
		equity, peak, dd := start, start, 0.0
		for range trades {
			equity += trades[rng.IntN(len(trades))].ProfitNet
			if equity > peak {
				peak = equity
			}
			if d := (peak - equity) / peak * 100; d > dd {
				dd = d
			}
			if equity <= 0 {
				dd = 100
				break
			}
		}
		rets[run], dds[run] = (equity-start)/start*100, dd
		if dd >= r.RuinDrawdownPct {
			ruin++
		}
	}
	sort.Float64s(rets)
	sort.Float64s(dds)
	return &MonteCarlo{
		Runs:              r.Runs,
		MedianReturnPct:   round2(quantile(rets, 0.5)),
		P5ReturnPct:       round2(quantile(rets, 0.05)),
		MedianDrawdownPct: round2(quantile(dds, 0.5)),
		P95DrawdownPct:    round2(quantile(dds, 0.95)),
		RuinPct:           round2(float64(ruin) / float64(r.Runs) * 100),
		Judged:            len(trades) >= r.MinTradesMC,
	}
}

func quantile(sorted []float64, q float64) float64 {
	return sorted[int(math.Round(q*float64(len(sorted)-1)))]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
