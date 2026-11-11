package replay

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Summary is one backtest's outcome, in the stake currency.
type Summary struct {
	Strategy      string  `json:"strategy"`
	Stake         string  `json:"stake"`
	Start         string  `json:"start"`
	End           string  `json:"end"`
	Trades        int     `json:"trades"`
	Wins          int     `json:"wins"`
	Losses        int     `json:"losses"`
	StartBalance  float64 `json:"start_balance"`
	FinalBalance  float64 `json:"final_balance"`
	ProfitNet     float64 `json:"profit_net"`
	ProfitPct     float64 `json:"profit_pct"`
	Fees          float64 `json:"fees"`
	ProfitGross   float64 `json:"profit_gross"`
	MaxDrawdown   float64 `json:"max_drawdown"`     // absolute
	MaxDrawdownPc float64 `json:"max_drawdown_pct"` // relative, percent
	MarketPct     float64 `json:"market_pct"`       // buy-and-hold of the pairs, averaged
	// freqtrade's own figures, nil when the result does not carry them.
	Sharpe       *float64 `json:"sharpe,omitempty"`
	Sortino      *float64 `json:"sortino,omitempty"`
	Calmar       *float64 `json:"calmar,omitempty"`
	ProfitFactor *float64 `json:"profit_factor,omitempty"`
	WinRatePct   *float64 `json:"win_rate_pct,omitempty"`
	HoldingAvgS  *float64 `json:"holding_avg_s,omitempty"` // average trade duration, seconds
}

// Trade is one closed (or force-closed) backtest trade.
type Trade struct {
	Pair       string  `json:"pair"`
	Open       string  `json:"open"`
	Close      string  `json:"close"`
	OpenRate   float64 `json:"open_rate"`
	CloseRate  float64 `json:"close_rate"`
	Stake      float64 `json:"stake"`
	ProfitNet  float64 `json:"profit_net"`
	ProfitPct  float64 `json:"profit_pct"`
	Fee        float64 `json:"fee"`
	ExitReason string  `json:"exit_reason"`
}

// ParseTradeTime reads a Trade's Open or Close as freqtrade writes it
// ("2025-10-20 13:00:00+00:00"), in UTC; ok is false for anything else.
func ParseTradeTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05-07:00", time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

type ftTrade struct {
	Pair        string  `json:"pair"`
	OpenDate    string  `json:"open_date"`
	CloseDate   string  `json:"close_date"`
	OpenRate    float64 `json:"open_rate"`
	CloseRate   float64 `json:"close_rate"`
	StakeAmount float64 `json:"stake_amount"`
	Amount      float64 `json:"amount"`
	ProfitAbs   float64 `json:"profit_abs"`
	ProfitRatio float64 `json:"profit_ratio"`
	FeeOpen     float64 `json:"fee_open"`
	FeeClose    float64 `json:"fee_close"`
	ExitReason  string  `json:"exit_reason"`
}

type ftStrategy struct {
	Trades              []ftTrade `json:"trades"`
	TotalTrades         int       `json:"total_trades"`
	Wins                int       `json:"wins"`
	Losses              int       `json:"losses"`
	StartingBalance     float64   `json:"starting_balance"`
	FinalBalance        float64   `json:"final_balance"`
	ProfitTotalAbs      float64   `json:"profit_total_abs"`
	ProfitTotal         float64   `json:"profit_total"`
	MaxDrawdownAbs      float64   `json:"max_drawdown_abs"`
	MaxRelativeDrawdown float64   `json:"max_relative_drawdown"`
	MarketChange        float64   `json:"market_change"`
	BacktestStart       string    `json:"backtest_start"`
	BacktestEnd         string    `json:"backtest_end"`
	StakeCurrency       string    `json:"stake_currency"`
	Sharpe              *float64  `json:"sharpe"`
	Sortino             *float64  `json:"sortino"`
	Calmar              *float64  `json:"calmar"`
	ProfitFactor        *float64  `json:"profit_factor"`
	Winrate             *float64  `json:"winrate"`
	HoldingAvgS         *float64  `json:"holding_avg_s"`
}

// ParseResult reads a freqtrade backtest-result-*.zip. Fees are what the
// trades paid: entry stake times fee_open plus exit value times fee_close.
func ParseResult(path, strategy string) (Summary, []Trade, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return Summary{}, nil, err
	}
	defer z.Close()
	for _, f := range z.File {
		n := filepath.Base(f.Name)
		if !strings.HasPrefix(n, "backtest-result-") || !strings.HasSuffix(n, ".json") || strings.HasSuffix(n, "_config.json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Summary{}, nil, err
		}
		var doc struct {
			Strategy map[string]ftStrategy `json:"strategy"`
		}
		err = json.NewDecoder(rc).Decode(&doc)
		rc.Close()
		if err != nil {
			return Summary{}, nil, fmt.Errorf("%s: %w", n, err)
		}
		s, ok := doc.Strategy[strategy]
		if !ok {
			return Summary{}, nil, fmt.Errorf("%s: no result for strategy %s", n, strategy)
		}
		return summarize(strategy, s)
	}
	return Summary{}, nil, errors.New("no backtest result in " + filepath.Base(path))
}

func summarize(name string, s ftStrategy) (Summary, []Trade, error) {
	out := Summary{
		Strategy: name, Stake: s.StakeCurrency, Start: s.BacktestStart, End: s.BacktestEnd,
		Trades: s.TotalTrades, Wins: s.Wins, Losses: s.Losses,
		StartBalance: s.StartingBalance, FinalBalance: s.FinalBalance,
		ProfitNet: s.ProfitTotalAbs, ProfitPct: s.ProfitTotal * 100,
		MaxDrawdown: s.MaxDrawdownAbs, MaxDrawdownPc: s.MaxRelativeDrawdown * 100,
		MarketPct: s.MarketChange * 100,
		Sharpe:    defined(s.Sharpe), Sortino: defined(s.Sortino), Calmar: defined(s.Calmar), ProfitFactor: s.ProfitFactor, HoldingAvgS: s.HoldingAvgS,
	}
	if s.Winrate != nil {
		w := *s.Winrate * 100
		out.WinRatePct = &w
	}
	trades := make([]Trade, 0, len(s.Trades))
	for _, t := range s.Trades {
		fee := t.StakeAmount*t.FeeOpen + t.Amount*t.CloseRate*t.FeeClose
		out.Fees += fee
		trades = append(trades, Trade{
			Pair: t.Pair, Open: t.OpenDate, Close: t.CloseDate, OpenRate: t.OpenRate, CloseRate: t.CloseRate,
			Stake: t.StakeAmount, ProfitNet: t.ProfitAbs, ProfitPct: t.ProfitRatio * 100, Fee: fee, ExitReason: t.ExitReason,
		})
	}
	out.ProfitGross = out.ProfitNet + out.Fees
	return out, trades, nil
}

// defined drops freqtrade's -100, which marks a ratio with a zero denominator.
func defined(v *float64) *float64 {
	if v == nil || *v == -100 {
		return nil
	}
	return v
}
