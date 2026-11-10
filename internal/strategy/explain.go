package strategy

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Bar is one stored candle; Time is its open in Unix seconds.
type Bar struct {
	Time                           int64
	Open, High, Low, Close, Volume float64
}

// Fill loads sorted bars like freqtrade's backtest: from the first at or after
// from, gaps of tf seconds filled with flat zero-volume candles at the last close.
func Fill(bars []Bar, from, tf int64) Frame {
	var f Frame
	put := func(b Bar) {
		f.Time = append(f.Time, b.Time)
		f.Open, f.High, f.Low = append(f.Open, b.Open), append(f.High, b.High), append(f.Low, b.Low)
		f.Close, f.Volume = append(f.Close, b.Close), append(f.Volume, b.Volume)
	}
	for _, b := range bars {
		n := len(f.Time)
		switch {
		case b.Time < from || (n > 0 && b.Time <= f.Time[n-1]):
			continue
		case n > 0 && tf > 0:
			for t, c := f.Time[n-1]+tf, f.Close[n-1]; t < b.Time; t += tf {
				put(Bar{Time: t, Open: c, High: c, Low: c, Close: c})
			}
		}
		put(b)
	}
	return f
}

// at is the operand's value at candle i, NaN off the frame like a pandas
// shift; ok is false when it cannot be computed here (a model column).
func (o Operand) at(f Frame, cols map[string][]float64, i int) (float64, bool) {
	if o.Num != nil {
		return *o.Num, true
	}
	x := f.price(o.Ref)
	if x == nil {
		x = cols[o.Ref]
	}
	switch {
	case x == nil:
		return math.NaN(), false
	case i < 0 || i >= len(x):
		return math.NaN(), true
	}
	return x[i], true
}

// holds evaluates c at candle i like the generated pandas code; known is
// false when an operand cannot be computed.
func (c Condition) holds(f Frame, cols map[string][]float64, i int) (holds, known bool) {
	l, okL := c.Left.at(f, cols, i)
	r, okR := c.Right.at(f, cols, i)
	switch c.Op {
	case "rising", "falling":
		b, okB := c.Left.at(f, cols, i-c.Bars)
		if !okL || !okB {
			return false, false
		}
		if c.Op == "rising" {
			return l > b, true
		}
		return l < b, true
	case "crosses_above", "crosses_below":
		lp, okLp := c.Left.at(f, cols, i-1)
		rp, okRp := c.Right.at(f, cols, i-1)
		if !okL || !okR || !okLp || !okRp {
			return false, false
		}
		if c.Op == "crosses_above" {
			return l > r && lp <= rp, true
		}
		return l < r && lp >= rp, true
	}
	if !okL || !okR {
		return false, false
	}
	switch c.Op {
	case ">":
		return l > r, true
	case "<":
		return l < r, true
	case ">=":
		return l >= r, true
	case "<=":
		return l <= r, true
	}
	return l == r, true
}

// Fires reports whether the entry (or exit) rules signal at candle i, as
// populate_entry_trend would mark it; known is false when a model column decides.
func (s Spec) Fires(entry bool, f Frame, cols map[string][]float64, i int) (fires, known bool) {
	g := s.Exit
	if entry {
		g = s.Entry
	}
	conds, anyOf := g.All, len(g.Any) > 0
	if anyOf {
		conds = g.Any
	}
	fires, known = !anyOf, true
	for _, c := range conds {
		h, k := c.holds(f, cols, i)
		known = known && k
		if anyOf {
			fires = fires || (h && k)
		} else {
			fires = fires && (h || !k)
		}
	}
	return fires, known || (anyOf && fires)
}

var opShort = map[string]string{"<": "under", "<=": "at or under", ">": "above", ">=": "at or above",
	"crosses_above": "above", "crosses_below": "below", "rising": "rising", "falling": "falling"}

var kindShort = map[string]string{"sma": "SMA", "ema": "EMA", "tema": "TEMA", "rsi": "RSI", "atr": "ATR", "adx": "ADX",
	"bollinger": "Bollinger bands", "macd": "MACD", "stoch": "stochastic"}

var priceWords = map[string]string{"close": "the price", "open": "the open", "high": "the high", "low": "the low", "volume": "the volume"}

// names labels every operand for a trade line: exact with periods for the
// players ("EMA 50"), plain without numbers for the demo ("the slow EMA").
func (s Spec) names() (exact, plain map[string]string) {
	exact, plain = map[string]string{}, map[string]string{}
	for p, w := range priceWords {
		exact[p], plain[p] = w, w
	}
	exact["prediction"], plain["prediction"] = "the model's predicted return", "the model's predicted return"
	for _, in := range s.Indicators {
		e, p := s.base(in)
		dup := s.peers(in) > 1
		switch in.Kind {
		case "sma", "ema", "tema":
			e = fmt.Sprintf("%s %d", kindShort[in.Kind], in.Period)
			if in.Source != "close" {
				e, p = e+" of the "+in.Source, p+" of the "+in.Source
			}
			exact[in.ID], plain[in.ID] = e, "the "+p
		case "rsi", "atr", "adx":
			if dup {
				e = fmt.Sprintf("%s %d", e, in.Period)
				p = "the " + p
			}
			exact[in.ID], plain[in.ID] = e, p
		case "highest", "lowest":
			w := map[string]string{"highest": "high", "lowest": "low"}[in.Kind]
			if in.Source != w {
				w = in.Kind + " " + in.Source
			}
			exact[in.ID], plain[in.ID] = fmt.Sprintf("the %d-candle %s", in.Period, w), "the recent "+w
		default:
			for _, f := range fields[in.Kind] {
				w := map[string]string{"upper": "upper Bollinger band", "middle": "middle Bollinger band", "lower": "lower Bollinger band",
					"macd": "MACD line", "signal": "MACD signal line", "hist": "MACD histogram", "k": "stochastic %K", "d": "stochastic %D"}[f]
				pw := w
				if dup {
					w, pw = fmt.Sprintf("%s (%d)", w, in.Period+in.Slow), strings.TrimSuffix(p, " "+kindShort[in.Kind])+" "+w
				}
				exact[in.ID+"."+f], plain[in.ID+"."+f] = "the "+w, "the "+pw
			}
		}
	}
	return exact, plain
}

// peers counts the spec's indicators of in's kind.
func (s Spec) peers(in Indicator) int {
	n := 0
	for _, x := range s.Indicators {
		if x.Kind == in.Kind {
			n++
		}
	}
	return n
}

// base names an indicator without numbers: "EMA", or "fast EMA" next to a slower one.
func (s Spec) base(in Indicator) (exact, plain string) {
	w := kindShort[in.Kind]
	if w == "" {
		w = map[string]string{"highest": "recent high", "lowest": "recent low"}[in.Kind]
	}
	length := func(x Indicator) int { return x.Period + x.Slow }
	var ls []int
	for _, x := range s.Indicators {
		if x.Kind == in.Kind {
			ls = append(ls, length(x))
		}
	}
	if len(ls) < 2 {
		return w, w
	}
	slices.Sort(ls)
	switch length(in) {
	case ls[0]:
		return w, "fast " + w
	case ls[len(ls)-1]:
		return w, "slow " + w
	}
	return w, "middle " + w
}

// val shows a computed value with 3 significant digits and at least one
// decimal, plus extra decimals; 10 at most.
func val(v float64, extra int) string {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	d := 2 - int(math.Floor(math.Log10(math.Abs(v))))
	return strconv.FormatFloat(v, 'f', min(10, max(1, d)+extra), 64)
}

// apart shows a and b like val, with more decimals while rounding would make
// them read equal or the wrong way round; exact is b shown as it is (a level).
func apart(a, b float64, exact bool) (string, string) {
	read := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	for extra := 0; ; extra++ {
		sa, sb := val(a, extra), val(b, extra)
		rb := read(sb)
		if exact {
			rb = b
		}
		if extra >= 10 || math.IsNaN(a) || math.IsNaN(b) || cmp.Compare(read(sa), rb) == cmp.Compare(a, b) {
			return sa, sb
		}
	}
}

// past shows v against the level n it is compared with.
func past(v, n float64) string {
	s, _ := apart(v, n, true)
	return s
}

// zone is the textbook reading of an oscillator past its usual level.
func (s Spec) zone(c Condition) string {
	if c.Right.Num == nil {
		return ""
	}
	n, kind := *c.Right.Num, ""
	for _, in := range s.Indicators {
		if c.Left.Ref == in.ID || c.Left.Ref == in.ID+".k" || c.Left.Ref == in.ID+".d" {
			kind = in.Kind
		}
	}
	low, high := map[string]float64{"rsi": 30, "stoch": 20}[kind], map[string]float64{"rsi": 70, "stoch": 80}[kind]
	switch {
	case low == 0:
		return ""
	case (c.Op == "<" || c.Op == "<=") && n <= low:
		return " = oversold"
	case (c.Op == ">" || c.Op == ">=") && n >= high:
		return " = overbought"
	}
	return ""
}

// line is one condition in words: with its values at candle i when vals is
// set, as the rule when not, and without any number when plain.
func (s Spec) line(c Condition, name map[string]string, f Frame, cols map[string][]float64, i int, vals, plain bool, level string) string {
	L, R, op := name[c.Left.Ref], name[c.Right.Ref], opShort[c.Op]
	l, _ := c.Left.at(f, cols, i)
	r, _ := c.Right.at(f, cols, i)
	numeric := c.Right.Num != nil
	n := ""
	if numeric {
		n = num(*c.Right.Num)
		if c.Left.Ref == "prediction" {
			n, vals = num(*c.Right.Num*100)+"%", false
		}
	}
	if plain && numeric {
		n = "the " + level + " level"
	}
	zone := s.zone(c)
	switch {
	case c.Left.Ref == "do_predict" && c.Op == "==":
		return "the model trusted its prediction"
	case c.Op == "==":
		if plain {
			return L + " hit " + n
		}
		return L + " was " + n
	case c.Op == "rising" || c.Op == "falling":
		switch {
		case plain:
			return L + " was " + op
		case vals:
			b, _ := c.Left.at(f, cols, i-c.Bars)
			sb, sl := apart(b, l, false)
			return fmt.Sprintf("%s was %s over %d candles (%s -> %s)", L, op, c.Bars, sb, sl)
		}
		return fmt.Sprintf("%s was %s over %d candles", L, op, c.Bars)
	case c.Op == "crosses_above" || c.Op == "crosses_below":
		if !numeric {
			n = R
		}
		switch {
		case vals && numeric:
			lp, _ := c.Left.at(f, cols, i-1)
			return fmt.Sprintf("%s crossed %s %s (%s -> %s)", L, op, n, past(lp, r), past(l, r))
		case vals:
			sl, sr := apart(l, r, false)
			return fmt.Sprintf("%s crossed %s %s (%s vs %s)", L, op, n, sl, sr)
		}
		return fmt.Sprintf("%s crossed %s %s", L, op, n)
	case !numeric:
		if vals && !plain {
			sl, sr := apart(l, r, false)
			return fmt.Sprintf("%s was %s, %s %s at %s", L, sl, op, R, sr)
		}
		return fmt.Sprintf("%s was %s %s", L, op, R)
	case vals && !plain:
		return fmt.Sprintf("%s was %s, %s %s%s", L, past(l, r), op, n, zone)
	}
	return fmt.Sprintf("%s was %s %s%s", L, op, n, zone)
}

// Why is the reason line of a signal at candle i (< 0: unknown): text with the
// values for players (B20), plain without numbers; values that do not fire are left out.
func (s Spec) Why(entry bool, f Frame, cols map[string][]float64, i int) (text, plain string) {
	g, verb, level := s.Exit, "sold: ", "sell"
	if entry {
		g, verb, level = s.Entry, "bought: ", "buy"
	}
	conds, join := g.All, " and "
	if len(g.Any) > 0 {
		conds, join = g.Any, " or "
	}
	fires, _ := s.Fires(entry, f, cols, i)
	vals := i >= 0 && fires
	if vals && len(g.Any) > 0 {
		var held []Condition
		for _, c := range conds {
			if h, k := c.holds(f, cols, i); h && k {
				held = append(held, c)
			}
		}
		conds, join = held, " and "
	}
	exact, plainNames := s.names()
	var t, p []string
	for _, c := range conds {
		t = append(t, s.line(c, exact, f, cols, i, vals, false, level))
		p = append(p, s.line(c, plainNames, f, cols, i, false, true, level))
	}
	return verb + strings.Join(t, join), verb + strings.Join(p, join)
}

// ExitReason says why a trade closed other than on its exit signal, from
// freqtrade's exit reason and the trade's profit in percent.
func (s Spec) ExitReason(reason string, profitPct float64) (text, plain string) {
	pct := func(f float64) string { return strconv.FormatFloat(math.Round(f*10000)/100, 'f', -1, 64) + "%" }
	switch reason {
	case "stop_loss", "stoploss_on_exchange":
		return "sold: the stop loss at " + pct(s.Stoploss) + " hit", "sold: the stop loss hit"
	case "trailing_stop_loss":
		text = "sold: the trailing stop hit"
		if s.Trailing != nil {
			text = "sold: the price fell " + pct(s.Trailing.Positive) + " back from its peak (the trailing stop)"
		}
		return text, "sold: the price fell back from its peak (the trailing stop)"
	case "roi":
		return fmt.Sprintf("sold: the profit target was reached (%+.1f%%)", profitPct), "sold: the profit target was reached"
	case "force_exit":
		return "still open: valued at the last price of the record", "still open: valued at the last price of the record"
	}
	return "sold: " + reason, "sold: " + reason
}

// Series is one indicator column of a chart.
type Series struct {
	Column  string `json:"column"`
	Label   string `json:"label"`
	Overlay bool   `json:"overlay"` // drawn on the price; false = its own pane
}

// Chart lists the indicator columns Columns makes, in spec order.
func (s Spec) Chart() []Series {
	exact, _ := s.names()
	var out []Series
	for _, in := range s.Indicators {
		over := map[string]bool{"sma": true, "ema": true, "tema": true, "highest": true, "lowest": true, "bollinger": true}[in.Kind] && in.Source != "volume"
		cols := []string{in.ID}
		if fs, ok := fields[in.Kind]; ok {
			cols = nil
			for _, f := range fs {
				cols = append(cols, in.ID+"."+f)
			}
		}
		for _, c := range cols {
			out = append(out, Series{Column: c, Label: strings.TrimPrefix(exact[c], "the "), Overlay: over})
		}
	}
	return out
}

// Gene is one value a child has different from its parent (B21).
type Gene struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
	Child  string `json:"child,omitempty"`
}

// Diff lists what child changed from parent: timeframe, stop loss, profit
// targets, trailing stop, indicator settings and rules.
func Diff(parent, child Spec) []Gene {
	var out []Gene
	add := func(name, p, c string) {
		if p != c {
			out = append(out, Gene{Name: name, Parent: p, Child: c})
		}
	}
	pct := func(f float64) string { return strconv.FormatFloat(math.Round(f*1e6)/1e4, 'f', -1, 64) + "%" }
	add("timeframe", parent.Timeframe, child.Timeframe)
	add("stop loss", pct(parent.Stoploss), pct(child.Stoploss))
	roi := func(s Spec) string {
		var parts []string
		for _, k := range s.roiKeys() {
			if s.ROI[k] < 100 {
				parts = append(parts, pct(s.ROI[k])+" after "+k+" min")
			}
		}
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, ", ")
	}
	add("profit targets", roi(parent), roi(child))
	trail := func(t *Trailing) string {
		if t == nil {
			return "off"
		}
		w := pct(t.Positive) + " below the peak"
		if t.Offset > 0 {
			w += " once up " + pct(t.Offset)
		}
		return w
	}
	add("trailing stop", trail(parent.Trailing), trail(child.Trailing))
	params := func(in Indicator) [][2]string {
		return [][2]string{{"source", in.Source}, {"period", itoa(in.Period)}, {"shift", itoa(in.Shift)},
			{"deviations", num0(in.StdDev)}, {"fast period", itoa(in.Fast)}, {"slow period", itoa(in.Slow)},
			{"signal period", itoa(in.Signal)}, {"%K smoothing", itoa(in.SmoothK)}, {"%D smoothing", itoa(in.SmoothD)}}
	}
	ids := pairUp(parent.Indicators, child.Indicators)
	for _, in := range child.Indicators {
		_, label := child.base(in)
		j := slices.IndexFunc(parent.Indicators, func(p Indicator) bool { return p.ID == ids[in.ID] })
		if j < 0 {
			add(label, "none", brief(in))
			continue
		}
		pp, cp := params(parent.Indicators[j]), params(in)
		for i := range cp {
			add(label+" "+cp[i][0], pp[i][1], cp[i][1])
		}
	}
	for _, in := range parent.Indicators {
		if !slices.ContainsFunc(child.Indicators, func(c Indicator) bool { return ids[c.ID] == in.ID }) {
			_, label := parent.base(in)
			add(label, brief(in), "none")
		}
	}
	entryP, exitP := parent.Rules()
	entryC, exitC := child.Rules()
	if entryP != entryC && rulesKey(parent.Entry, nil) != rulesKey(child.Entry, ids) {
		out = append(out, Gene{Name: "buy rule", Parent: entryP, Child: entryC})
	}
	if exitP != exitC && rulesKey(parent.Exit, nil) != rulesKey(child.Exit, ids) {
		out = append(out, Gene{Name: "sell rule", Parent: exitP, Child: exitC})
	}
	return out
}

// pairUp maps child indicator ids to the parent ones they stand for: same settings,
// then same id, then leftovers of a kind by length (fast to fast); new ones get "+id".
func pairUp(parent, child []Indicator) map[string]string {
	ids, used := map[string]string{}, make([]bool, len(parent))
	settings := func(in Indicator) Indicator { in.ID = ""; return in }
	for _, same := range []func(p, c Indicator) bool{
		func(p, c Indicator) bool { return p == c },
		func(p, c Indicator) bool { return settings(p) == settings(c) },
		func(p, c Indicator) bool { return p.ID == c.ID },
	} {
		for _, c := range child {
			if _, done := ids[c.ID]; done {
				continue
			}
			for j, p := range parent {
				if !used[j] && p.Kind == c.Kind && same(p, c) {
					ids[c.ID], used[j] = p.ID, true
					break
				}
			}
		}
	}
	length := func(x Indicator) int { return x.Period + x.Slow }
	var left []Indicator
	for _, c := range child {
		if _, done := ids[c.ID]; !done {
			left = append(left, c)
		}
	}
	var free []int
	for j := range parent {
		if !used[j] {
			free = append(free, j)
		}
	}
	slices.SortStableFunc(left, func(a, b Indicator) int { return length(a) - length(b) })
	slices.SortStableFunc(free, func(a, b int) int { return length(parent[a]) - length(parent[b]) })
	for _, c := range left {
		for k, j := range free {
			if j >= 0 && parent[j].Kind == c.Kind {
				ids[c.ID], free[k] = parent[j].ID, -1
				break
			}
		}
	}
	for _, c := range child {
		if _, done := ids[c.ID]; !done {
			ids[c.ID] = "+" + c.ID
		}
	}
	return ids
}

// rulesKey is g with its indicator refs renamed by ids, conditions in any order:
// equal keys are the same rule over the paired indicators.
func rulesKey(g Group, ids map[string]string) string {
	rename := func(o Operand) Operand {
		id, field, dot := strings.Cut(o.Ref, ".")
		if to, ok := ids[id]; ok && o.Num == nil {
			o.Ref = to
			if dot {
				o.Ref += "." + field
			}
		}
		return o
	}
	var parts []string
	for side, cs := range map[string][]Condition{"all": g.All, "any": g.Any} {
		for _, c := range cs {
			c.Left, c.Right = rename(c.Left), rename(c.Right)
			b, _ := json.Marshal(c)
			parts = append(parts, side+string(b))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "\n")
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func num0(f float64) string {
	if f == 0 {
		return ""
	}
	return num(f)
}

// brief is an indicator and its settings in a few characters: "RSI 14".
func brief(in Indicator) string {
	switch in.Kind {
	case "macd":
		return fmt.Sprintf("MACD %d/%d/%d", in.Fast, in.Slow, in.Signal)
	case "stoch":
		return fmt.Sprintf("stochastic %d/%d/%d", in.Period, in.SmoothK, in.SmoothD)
	case "bollinger":
		return fmt.Sprintf("Bollinger bands %d, %s", in.Period, num(in.StdDev))
	case "highest", "lowest":
		return fmt.Sprintf("%s %s of %d", in.Kind, in.Source, in.Period)
	}
	return fmt.Sprintf("%s %d", kindShort[in.Kind], in.Period)
}
