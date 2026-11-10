// Package watch evaluates a bot's entry conditions against its last closed
// candle, so the hub can show what each bot is waiting for.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Rule compares a dataframe column with a number, another column or a key
// of the bot's config ("config.wf_entry_move").
type Rule struct {
	Name  string `yaml:"name" json:"name,omitempty"`
	Left  string `yaml:"left" json:"left"`
	Op    string `yaml:"op" json:"op"`
	Right string `yaml:"right" json:"right"`
}

var ops = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true}

// Validate rejects a rule the hub could never evaluate.
func (r Rule) Validate() error {
	switch {
	case r.Left == "":
		return fmt.Errorf("watch rule: left missing")
	case !ops[r.Op]:
		return fmt.Errorf("watch rule %s: bad op %q", r.Label(), r.Op)
	case r.Right == "":
		return fmt.Errorf("watch rule %s: right missing", r.Label())
	}
	return nil
}

// Label names the rule in metrics and the UI.
func (r Rule) Label() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Left + " " + r.Op + " " + r.Right
}

func num(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case json.Number:
		return t.Float64()
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case nil:
		return 0, fmt.Errorf("empty")
	}
	return 0, fmt.Errorf("not a number: %v", v)
}

func value(ref string, row, cfg map[string]any) (float64, error) {
	if f, err := strconv.ParseFloat(ref, 64); err == nil {
		return f, nil
	}
	if key, ok := strings.CutPrefix(ref, "config."); ok {
		v, ok := cfg[key]
		if !ok {
			return 0, fmt.Errorf("config key %s missing", key)
		}
		return num(v)
	}
	v, ok := row[ref]
	if !ok {
		return 0, fmt.Errorf("column %s missing", ref)
	}
	f, err := num(v)
	if err != nil {
		return 0, fmt.Errorf("column %s: %w", ref, err)
	}
	return f, nil
}

// Result is one rule on one pair's last closed candle.
type Result struct {
	Bot   string  `json:"bot"`
	Owner string  `json:"owner"`
	Pair  string  `json:"pair"`
	Rule  string  `json:"rule"`
	Date  string  `json:"date,omitempty"`
	Met   bool    `json:"met"`
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
	// Gap is how far left still has to move, relative to left; 0 when met.
	Gap   float64 `json:"gap"`
	Error string  `json:"error,omitempty"`
}

// Eval checks one rule against a candle row.
func Eval(r Rule, row, cfg map[string]any) Result {
	res := Result{Rule: r.Label()}
	if d, ok := row["date"].(string); ok {
		res.Date = d
	}
	l, err := value(r.Left, row, cfg)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	rt, err := value(r.Right, row, cfg)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Left, res.Right = l, rt
	switch r.Op {
	case ">":
		res.Met = l > rt
	case ">=":
		res.Met = l >= rt
	case "<":
		res.Met = l < rt
	case "<=":
		res.Met = l <= rt
	case "==":
		res.Met = l == rt
	}
	if !res.Met {
		d := rt - l
		if r.Op == "<" || r.Op == "<=" {
			d = l - rt
		}
		if r.Op == "==" {
			d = math.Abs(d)
		}
		if l != 0 {
			d /= math.Abs(l)
		}
		res.Gap = d
	}
	return res
}

// Source reads one bot's timeframe and last closed candle.
type Source interface {
	Timeframe(context.Context) (string, error)
	LastCandle(ctx context.Context, pair, timeframe string) (map[string]any, error)
}

// Target is one bot with rules to watch.
type Target struct {
	Bot    string
	Owner  string
	Pairs  []string
	Rules  []Rule
	Config map[string]any
	Source Source
}

func check(ctx context.Context, t Target) []Result {
	var out []Result
	fail := func(pair, msg string) {
		for _, r := range t.Rules {
			out = append(out, Result{Bot: t.Bot, Owner: t.Owner, Pair: pair, Rule: r.Label(), Error: msg})
		}
	}
	tf, err := t.Source.Timeframe(ctx)
	if err != nil {
		for _, p := range t.Pairs {
			fail(p, err.Error())
		}
		return out
	}
	for _, p := range t.Pairs {
		row, err := t.Source.LastCandle(ctx, p, tf)
		if err != nil {
			fail(p, err.Error())
			continue
		}
		for _, r := range t.Rules {
			res := Eval(r, row, t.Config)
			res.Bot, res.Owner, res.Pair = t.Bot, t.Owner, p
			out = append(out, res)
		}
	}
	return out
}

// Collect checks every target in parallel; results are sorted by bot, pair, rule.
func Collect(ctx context.Context, targets []Target) []Result {
	parts := make([][]Result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		if len(t.Rules) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[i] = check(ctx, t)
		}()
	}
	wg.Wait()
	var out []Result
	for _, p := range parts {
		out = append(out, p...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Bot != b.Bot {
			return a.Bot < b.Bot
		}
		return a.Pair < b.Pair
	})
	return out
}

// WriteMetrics renders results in Prometheus text format. A rule that could
// not be read gets wickfleet_watch_up 0 and no values.
func WriteMetrics(w io.Writer, rs []Result) {
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	gauges := []struct {
		name, help string
		val        func(Result) float64
		always     bool
	}{
		{"wickfleet_watch_up", "1 when the hub could read the rule's candle.", func(r Result) float64 { return b2f(r.Error == "") }, true},
		{"wickfleet_watch_met", "1 when the entry rule holds on the last closed candle.", func(r Result) float64 { return b2f(r.Met) }, false},
		{"wickfleet_watch_left", "Left side of the rule on the last closed candle.", func(r Result) float64 { return r.Left }, false},
		{"wickfleet_watch_right", "Right side of the rule (threshold).", func(r Result) float64 { return r.Right }, false},
		{"wickfleet_watch_gap_ratio", "Move of the left side still needed, relative to it; 0 when met.", func(r Result) float64 { return r.Gap }, false},
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, r := range rs {
			if r.Error != "" && !g.always {
				continue
			}
			fmt.Fprintf(w, "%s{bot=%q,owner=%q,pair=%q,rule=%q} %g\n", g.name, r.Bot, r.Owner, r.Pair, r.Rule, g.val(r))
		}
	}
}
