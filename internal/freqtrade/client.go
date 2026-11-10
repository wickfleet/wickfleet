// Package freqtrade is the small slice of the freqtrade REST API the hub reads.
package freqtrade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to one bot with its Basic credentials.
type Client struct {
	Base string // e.g. http://bot-ma:8080
	User string
	Pass string
	HTTP *http.Client
}

func (c Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.Base + "/api/v1/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Pass)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Profit is /api/v1/profit. Freqtrade's profit figures are after fees.
type Profit struct {
	ProfitClosedCoin float64 `json:"profit_closed_coin"`
	ProfitAllCoin    float64 `json:"profit_all_coin"`
	ClosedTradeCount int     `json:"closed_trade_count"`
	TradeCount       int     `json:"trade_count"`
	WinningTrades    int     `json:"winning_trades"`
	LosingTrades     int     `json:"losing_trades"`
}

// Balance is the part of /api/v1/balance the hub needs.
type Balance struct {
	Total           float64 `json:"total"`
	StartingCapital float64 `json:"starting_capital"`
	Stake           string  `json:"stake"`
}

// Trade is the fee part of one trade from /api/v1/trades or /api/v1/status.
type Trade struct {
	FeeOpenCost  *float64 `json:"fee_open_cost"`
	FeeCloseCost *float64 `json:"fee_close_cost"`
	IsOpen       bool     `json:"is_open"`
}

// Fees is the trade's fees paid so far, in stake currency.
func (t Trade) Fees() float64 {
	var f float64
	if t.FeeOpenCost != nil {
		f += *t.FeeOpenCost
	}
	if t.FeeCloseCost != nil {
		f += *t.FeeCloseCost
	}
	return f
}

func (c Client) Profit(ctx context.Context) (Profit, error) {
	var p Profit
	return p, c.get(ctx, "profit", nil, &p)
}

func (c Client) Balance(ctx context.Context) (Balance, error) {
	var b Balance
	return b, c.get(ctx, "balance", nil, &b)
}

// pageSize is freqtrade's own cap for /trades.
const pageSize = 500

// Trades returns every closed trade (paged) plus the open ones.
func (c Client) Trades(ctx context.Context) ([]Trade, error) {
	var all []Trade
	for offset := 0; ; offset += pageSize {
		var page struct {
			Trades      []Trade `json:"trades"`
			TotalTrades int     `json:"total_trades"`
		}
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := c.get(ctx, "trades", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Trades...)
		if len(page.Trades) < pageSize || offset+pageSize >= page.TotalTrades {
			break
		}
	}
	var open []Trade
	if err := c.get(ctx, "status", nil, &open); err != nil {
		return nil, err
	}
	return append(all, open...), nil
}

// Timeframe is the bot's candle timeframe from /api/v1/show_config.
func (c Client) Timeframe(ctx context.Context) (string, error) {
	var sc struct {
		Timeframe string `json:"timeframe"`
	}
	if err := c.get(ctx, "show_config", nil, &sc); err != nil {
		return "", err
	}
	if sc.Timeframe == "" {
		return "", fmt.Errorf("show_config: no timeframe")
	}
	return sc.Timeframe, nil
}

// LastCandle is the newest analyzed candle of pair from /api/v1/pair_candles,
// column name to value. Freqtrade analyzes closed candles only.
func (c Client) LastCandle(ctx context.Context, pair, timeframe string) (map[string]any, error) {
	var pc struct {
		Columns []string `json:"columns"`
		Data    [][]any  `json:"data"`
	}
	q := url.Values{"pair": {pair}, "timeframe": {timeframe}, "limit": {"1"}}
	if err := c.get(ctx, "pair_candles", q, &pc); err != nil {
		return nil, err
	}
	if len(pc.Data) == 0 {
		return nil, fmt.Errorf("pair_candles %s: no candles", pair)
	}
	last := pc.Data[len(pc.Data)-1]
	row := make(map[string]any, len(pc.Columns))
	for i, col := range pc.Columns {
		if i < len(last) {
			row[col] = last[i]
		}
	}
	return row, nil
}
