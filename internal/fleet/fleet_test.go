package fleet

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"wickfleet/internal/freqtrade"
	"wickfleet/internal/registry"
)

type fake struct {
	p   freqtrade.Profit
	b   freqtrade.Balance
	t   []freqtrade.Trade
	err error
}

func (f fake) Profit(context.Context) (freqtrade.Profit, error)   { return f.p, f.err }
func (f fake) Balance(context.Context) (freqtrade.Balance, error) { return f.b, nil }
func (f fake) Trades(context.Context) ([]freqtrade.Trade, error)  { return f.t, nil }

func fp(v float64) *float64 { return &v }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCollect(t *testing.T) {
	live := false
	bots := []registry.Bot{
		{Name: "ma", Owner: "alice", StakeCurrency: "USDT"},
		{Name: "sample", Owner: "alice", StakeCurrency: "USDT", Wallet: 1000},
		{Name: "real", Owner: "alice", StakeCurrency: "USDT", DryRun: &live},
		{Name: "down", Owner: "alice", StakeCurrency: "USDT"},
	}
	srcs := map[string]Source{
		"ma": fake{
			p: freqtrade.Profit{ProfitAllCoin: 12, TradeCount: 4, ClosedTradeCount: 3},
			b: freqtrade.Balance{Total: 1012, StartingCapital: 1000, Stake: "USDT"},
			t: []freqtrade.Trade{{FeeOpenCost: fp(0.5), FeeCloseCost: fp(0.5)}, {FeeOpenCost: fp(1)}},
		},
		"sample": fake{
			p: freqtrade.Profit{ProfitAllCoin: -2, TradeCount: 1},
			b: freqtrade.Balance{Total: 998, StartingCapital: 990, Stake: "USDT"},
		},
		"real": fake{
			p: freqtrade.Profit{ProfitAllCoin: 5, TradeCount: 2},
			b: freqtrade.Balance{Total: 105, StartingCapital: 100, Stake: "USDT"},
		},
		"down": fake{err: errors.New("connection refused")},
	}
	r := Collect(context.Background(), bots, func(b registry.Bot) Source { return srcs[b.Name] })

	ma := r.Bots[0]
	if !near(ma.Fees, 2) || !near(ma.ProfitGross, 14) || !near(ma.ReturnPct, 1.2) || !near(ma.FeePerTrade, 0.5) {
		t.Fatalf("ma %+v", ma)
	}
	if r.Bots[3].Error == "" {
		t.Fatal("down bot must carry its error")
	}
	if len(r.Totals) != 2 {
		t.Fatalf("totals %+v", r.Totals)
	}
	liveT, dryT := r.Totals[0], r.Totals[1]
	if liveT.Dry || liveT.Bots != 1 || !near(liveT.ProfitNet, 5) {
		t.Fatalf("live totals mixed: %+v", liveT)
	}
	if !dryT.Dry || dryT.Bots != 2 || !near(dryT.Investment, 2000) || !near(dryT.ProfitNet, 10) || !near(dryT.ReturnPct, 0.5) {
		t.Fatalf("dry totals %+v", dryT)
	}

	var buf bytes.Buffer
	WriteMetrics(&buf, r)
	out := buf.String()
	for _, want := range []string{
		`wickfleet_bot_up{bot="down",owner="alice",dry="true",stake="USDT"} 0`,
		`wickfleet_bot_fees{bot="ma",owner="alice",dry="true",stake="USDT"} 2`,
		`wickfleet_bot_value{bot="real",owner="alice",dry="false",stake="USDT"} 105`,
		"# TYPE wickfleet_bot_profit_net gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
