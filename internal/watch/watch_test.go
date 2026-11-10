package watch

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEval(t *testing.T) {
	row := map[string]any{"date": "2026-10-20T00:00:00Z", "close": 83000.0, "hi20": 87150.0, "sma200": 72000.0, "&-s_close": -0.004, "do_predict": 1.0, "enter_long": nil}
	cfg := map[string]any{"wf_entry_move": 0.01}
	cases := []struct {
		r         Rule
		met       bool
		gap       float64
		err       string
		left, rgt float64
	}{
		{r: Rule{Left: "close", Op: ">", Right: "hi20"}, gap: 0.05, left: 83000, rgt: 87150},
		{r: Rule{Left: "close", Op: ">", Right: "sma200"}, met: true, left: 83000, rgt: 72000},
		{r: Rule{Left: "close", Op: "<", Right: "sma200"}, gap: 11000.0 / 83000, left: 83000, rgt: 72000},
		{r: Rule{Left: "&-s_close", Op: ">", Right: "config.wf_entry_move"}, gap: 0.014 / 0.004, left: -0.004, rgt: 0.01},
		{r: Rule{Left: "do_predict", Op: "==", Right: "1"}, met: true, left: 1, rgt: 1},
		{r: Rule{Left: "rsi", Op: ">", Right: "30"}, err: "column rsi missing"},
		{r: Rule{Left: "enter_long", Op: "==", Right: "1"}, err: "column enter_long: empty"},
		{r: Rule{Left: "close", Op: ">", Right: "config.nope"}, err: "config key nope missing"},
	}
	for _, c := range cases {
		got := Eval(c.r, row, cfg)
		if c.err != "" {
			if got.Error != c.err {
				t.Errorf("%s: error %q, want %q", c.r.Label(), got.Error, c.err)
			}
			continue
		}
		if got.Error != "" || got.Met != c.met || !near(got.Gap, c.gap) || got.Left != c.left || got.Right != c.rgt {
			t.Errorf("%s: %+v", c.r.Label(), got)
		}
		if got.Date != "2026-10-20T00:00:00Z" {
			t.Errorf("date %q", got.Date)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, r := range []Rule{{Op: ">", Right: "1"}, {Left: "a", Op: "=>", Right: "1"}, {Left: "a", Op: ">"}} {
		if r.Validate() == nil {
			t.Errorf("%+v passed", r)
		}
	}
	if err := (Rule{Left: "a", Op: "<=", Right: "b"}).Validate(); err != nil {
		t.Error(err)
	}
	if (Rule{Name: "breakout", Left: "a", Op: ">", Right: "b"}).Label() != "breakout" {
		t.Error("name not used as label")
	}
}

type fake struct {
	tfErr error
	rows  map[string]map[string]any
}

func (f fake) Timeframe(context.Context) (string, error) { return "1d", f.tfErr }
func (f fake) LastCandle(_ context.Context, pair, tf string) (map[string]any, error) {
	if tf != "1d" {
		return nil, errors.New("wrong timeframe")
	}
	r, ok := f.rows[pair]
	if !ok {
		return nil, errors.New("no candles")
	}
	return r, nil
}

func TestCollectAndMetrics(t *testing.T) {
	rules := []Rule{{Name: "breakout", Left: "close", Op: ">", Right: "hi20"}}
	targets := []Target{
		{Bot: "trend", Owner: "dhumpf", Pairs: []string{"ETH/USDT", "BTC/USDT"}, Rules: rules,
			Source: fake{rows: map[string]map[string]any{"BTC/USDT": {"close": 100.0, "hi20": 105.0}}}},
		{Bot: "down", Owner: "dhumpf", Pairs: []string{"BTC/USDT"}, Rules: rules, Source: fake{tfErr: errors.New("HTTP 401")}},
		{Bot: "quiet", Owner: "dhumpf", Pairs: []string{"BTC/USDT"}, Source: fake{}},
	}
	rs := Collect(context.Background(), targets)
	if len(rs) != 3 {
		t.Fatalf("results %d: %+v", len(rs), rs)
	}
	if rs[0].Bot != "down" || rs[0].Error != "HTTP 401" {
		t.Errorf("down: %+v", rs[0])
	}
	if rs[1].Pair != "BTC/USDT" || rs[1].Met || !near(rs[1].Gap, 0.05) {
		t.Errorf("trend BTC: %+v", rs[1])
	}
	if rs[2].Pair != "ETH/USDT" || rs[2].Error != "no candles" {
		t.Errorf("trend ETH: %+v", rs[2])
	}
	var b bytes.Buffer
	WriteMetrics(&b, rs)
	out := b.String()
	for _, want := range []string{
		`wickfleet_watch_up{bot="down",owner="dhumpf",pair="BTC/USDT",rule="breakout"} 0`,
		`wickfleet_watch_met{bot="trend",owner="dhumpf",pair="BTC/USDT",rule="breakout"} 0`,
		`wickfleet_watch_gap_ratio{bot="trend",owner="dhumpf",pair="BTC/USDT",rule="breakout"} 0.05`,
		`wickfleet_watch_right{bot="trend",owner="dhumpf",pair="BTC/USDT",rule="breakout"} 105`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics miss %s\n%s", want, out)
		}
	}
	if strings.Contains(out, `wickfleet_watch_met{bot="down"`) {
		t.Error("a failed read must not export values")
	}
}
