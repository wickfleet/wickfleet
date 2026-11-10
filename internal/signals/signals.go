// Package signals computes slow, order-free signals for markets the hub
// cannot trade itself (ETFs and stocks at a broker without an API, or any
// symbol worth watching). The rule is the monthly 10-month SMA: hold while
// the last completed month closes above it.
package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Bar is one adjusted daily close.
type Bar struct {
	Day   time.Time
	Close float64
}

// Source returns daily bars for a symbol, oldest first.
type Source interface {
	Daily(ctx context.Context, symbol string) ([]Bar, error)
}

// Signal is the rule's verdict for one symbol at one month end.
type Signal struct {
	Symbol string  `json:"symbol"`
	Month  string  `json:"month"` // YYYY-MM, the completed month the verdict uses
	Hold   bool    `json:"hold"`
	Close  float64 `json:"close"`
	SMA    float64 `json:"sma10m"`
	Gap    float64 `json:"gap"` // close / sma - 1
	Error  string  `json:"error,omitempty"`
}

// Months is the length of the moving average, in month-end closes.
const Months = 10

// Monthly evaluates the rule on months that ended before `now` (in loc).
// The current month never counts: its close is not final.
func Monthly(bars []Bar, now time.Time, loc *time.Location) (Signal, error) {
	cur := now.In(loc).Format("2006-01")
	var months []string
	last := map[string]float64{}
	for _, b := range bars {
		m := b.Day.In(loc).Format("2006-01")
		if m >= cur {
			break
		}
		if _, seen := last[m]; !seen {
			months = append(months, m)
		}
		last[m] = b.Close
	}
	if len(months) < Months {
		return Signal{}, fmt.Errorf("need %d completed months, have %d", Months, len(months))
	}
	var sum float64
	for _, m := range months[len(months)-Months:] {
		sum += last[m]
	}
	m := months[len(months)-1]
	s := Signal{Month: m, Close: last[m], SMA: sum / Months}
	s.Hold = s.Close > s.SMA
	s.Gap = s.Close/s.SMA - 1
	return s, nil
}

// Yahoo reads adjusted daily closes from the public chart endpoint.
type Yahoo struct {
	Base string // default https://query1.finance.yahoo.com
	HTTP *http.Client
}

func (y Yahoo) Daily(ctx context.Context, symbol string) ([]Bar, error) {
	base := y.Base
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	u := fmt.Sprintf("%s/v8/finance/chart/%s?range=2y&interval=1d&events=div%%2Csplit", base, symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	hc := y.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", symbol, resp.StatusCode)
	}
	var body struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Adj []struct {
						Adjclose []*float64 `json:"adjclose"`
					} `json:"adjclose"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Chart.Result) == 0 || len(body.Chart.Result[0].Indicators.Adj) == 0 {
		return nil, errors.New(symbol + ": no data")
	}
	r := body.Chart.Result[0]
	adj := r.Indicators.Adj[0].Adjclose
	bars := make([]Bar, 0, len(r.Timestamp))
	for i, ts := range r.Timestamp {
		if i < len(adj) && adj[i] != nil {
			bars = append(bars, Bar{Day: time.Unix(ts, 0).UTC(), Close: *adj[i]})
		}
	}
	return bars, nil
}

// Evaluate runs the rule for every symbol; a failing symbol carries Error.
func Evaluate(ctx context.Context, src Source, symbols []string, now time.Time, loc *time.Location) []Signal {
	out := make([]Signal, 0, len(symbols))
	for _, sym := range symbols {
		bars, err := src.Daily(ctx, sym)
		var s Signal
		if err == nil {
			s, err = Monthly(bars, now, loc)
		}
		s.Symbol = sym
		if err != nil {
			s.Error = err.Error()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// Mode decides what a flip asks for. In a taxable account every sale
// realizes gains, so the signal steers new money instead.
type Mode struct {
	Taxable bool
	Parking string // where new money goes while a fund is OUT, e.g. CLF
}

func (m Mode) flip(hold bool) string {
	switch {
	case !m.Taxable && hold:
		return "  <- BUY at the next open"
	case !m.Taxable:
		return "  <- SELL at the next open"
	case hold:
		return "  <- back IN: new money goes here again"
	case m.Parking != "":
		return "  <- OUT: stop buying, new money to " + m.Parking + "; sell only at a loss"
	default:
		return "  <- OUT: stop buying; sell only at a loss"
	}
}

// Summary is the month-end message. prev holds last month's verdicts.
func Summary(sigs []Signal, prev map[string]bool, mode Mode) string {
	var b strings.Builder
	month := ""
	for _, s := range sigs {
		if s.Error == "" {
			month = s.Month
			break
		}
	}
	fmt.Fprintf(&b, "Watchlist signals, %s close (10-month average):\n", month)
	if mode.Taxable {
		b.WriteString("Taxable account: signals steer new money, not sales.\n")
	}
	for _, s := range sigs {
		if s.Error != "" {
			fmt.Fprintf(&b, "%s: no signal (%s)\n", s.Symbol, s.Error)
			continue
		}
		verdict := "OUT"
		if s.Hold {
			verdict = "HOLD"
		}
		change := ""
		if was, ok := prev[s.Symbol]; ok && was != s.Hold {
			change = mode.flip(s.Hold)
		}
		fmt.Fprintf(&b, "%s: %s, %+.1f%% vs average%s\n", s.Symbol, verdict, s.Gap*100, change)
	}
	return strings.TrimRight(b.String(), "\n")
}
