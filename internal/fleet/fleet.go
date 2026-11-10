// Package fleet collects per-bot money figures and sums them. Dry and live
// bots are summed apart and never mixed.
package fleet

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"wickfleet/internal/freqtrade"
	"wickfleet/internal/registry"
)

// BotStats is one bot's figures in its stake currency.
type BotStats struct {
	Name        string  `json:"name"`
	Owner       string  `json:"owner"`
	Dry         bool    `json:"dry"`
	Stake       string  `json:"stake"`
	Investment  float64 `json:"investment"`
	Value       float64 `json:"value"`
	ProfitNet   float64 `json:"profit_net"`
	Fees        float64 `json:"fees"`
	ProfitGross float64 `json:"profit_gross"`
	ReturnPct   float64 `json:"return_pct"`
	Trades      int     `json:"trades"`
	Closed      int     `json:"closed"`
	FeePerTrade float64 `json:"fee_per_trade"`
	Error       string  `json:"error,omitempty"`
}

// Totals sums bots of one kind (dry or live) and one stake currency.
type Totals struct {
	Dry         bool    `json:"dry"`
	Stake       string  `json:"stake"`
	Bots        int     `json:"bots"`
	Investment  float64 `json:"investment"`
	Value       float64 `json:"value"`
	ProfitNet   float64 `json:"profit_net"`
	Fees        float64 `json:"fees"`
	ProfitGross float64 `json:"profit_gross"`
	ReturnPct   float64 `json:"return_pct"`
	Trades      int     `json:"trades"`
}

// Report is what /hub/api/fleet returns.
type Report struct {
	At     time.Time  `json:"at"`
	Bots   []BotStats `json:"bots"`
	Totals []Totals   `json:"totals"`
}

// Source reads one bot.
type Source interface {
	Profit(context.Context) (freqtrade.Profit, error)
	Balance(context.Context) (freqtrade.Balance, error)
	Trades(context.Context) ([]freqtrade.Trade, error)
}

func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}

// Stats reads one bot. A failing bot gets Error set, never a guessed number.
func Stats(ctx context.Context, b registry.Bot, src Source) BotStats {
	s := BotStats{Name: b.Name, Owner: b.Owner, Dry: b.Dry(), Stake: b.StakeCurrency}
	p, err := src.Profit(ctx)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	bal, err := src.Balance(ctx)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	trades, err := src.Trades(ctx)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	if bal.Stake != "" {
		s.Stake = bal.Stake
	}
	// freqtrade's starting_capital is wallet x tradable_balance_ratio while
	// total counts the whole wallet, so the registry wallet is the investment.
	s.Investment = bal.StartingCapital
	if b.Wallet > 0 {
		s.Investment = b.Wallet
	}
	s.Value = bal.Total
	s.ProfitNet = p.ProfitAllCoin
	for _, t := range trades {
		s.Fees += t.Fees()
	}
	s.ProfitGross = s.ProfitNet + s.Fees
	s.ReturnPct = pct(s.ProfitNet, s.Investment)
	s.Trades = p.TradeCount
	s.Closed = p.ClosedTradeCount
	if s.Trades > 0 {
		s.FeePerTrade = s.Fees / float64(s.Trades)
	}
	return s
}

// Sum groups by dry/live and stake currency. Bots with an error are left out.
func Sum(bots []BotStats) []Totals {
	type key struct {
		dry   bool
		stake string
	}
	m := map[key]*Totals{}
	for _, b := range bots {
		if b.Error != "" {
			continue
		}
		k := key{b.Dry, b.Stake}
		t := m[k]
		if t == nil {
			t = &Totals{Dry: b.Dry, Stake: b.Stake}
			m[k] = t
		}
		t.Bots++
		t.Investment += b.Investment
		t.Value += b.Value
		t.ProfitNet += b.ProfitNet
		t.Fees += b.Fees
		t.ProfitGross += b.ProfitGross
		t.Trades += b.Trades
	}
	out := make([]Totals, 0, len(m))
	for _, t := range m {
		t.ReturnPct = pct(t.ProfitNet, t.Investment)
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dry != out[j].Dry {
			return !out[i].Dry
		}
		return out[i].Stake < out[j].Stake
	})
	return out
}

// Collect reads all bots in parallel.
func Collect(ctx context.Context, bots []registry.Bot, src func(registry.Bot) Source) Report {
	stats := make([]BotStats, len(bots))
	var wg sync.WaitGroup
	for i, b := range bots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats[i] = Stats(ctx, b, src(b))
		}()
	}
	wg.Wait()
	return Report{At: time.Now().UTC(), Bots: stats, Totals: Sum(stats)}
}

// WriteMetrics renders the report in Prometheus text format.
func WriteMetrics(w io.Writer, r Report) {
	gauges := []struct {
		name, help string
		val        func(BotStats) float64
	}{
		{"wickfleet_bot_up", "1 when the hub could read the bot.", func(b BotStats) float64 {
			if b.Error == "" {
				return 1
			}
			return 0
		}},
		{"wickfleet_bot_investment", "Starting capital in stake currency.", func(b BotStats) float64 { return b.Investment }},
		{"wickfleet_bot_value", "Current wallet value in stake currency.", func(b BotStats) float64 { return b.Value }},
		{"wickfleet_bot_profit_net", "Profit after fees, open trades included.", func(b BotStats) float64 { return b.ProfitNet }},
		{"wickfleet_bot_fees", "Fees paid, open and close legs.", func(b BotStats) float64 { return b.Fees }},
		{"wickfleet_bot_trades", "Trades, open and closed.", func(b BotStats) float64 { return float64(b.Trades) }},
		{"wickfleet_bot_fee_per_trade", "Fees divided by trades.", func(b BotStats) float64 { return b.FeePerTrade }},
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, b := range r.Bots {
			fmt.Fprintf(w, "%s{bot=%q,owner=%q,dry=\"%t\",stake=%q} %g\n", g.name, b.Name, b.Owner, b.Dry, b.Stake, g.val(b))
		}
	}
}
