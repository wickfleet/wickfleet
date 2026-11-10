package strategy

import (
	"fmt"
	"sort"
	"strings"
)

var kindWords = map[string]string{
	"sma": "simple average", "ema": "exponential average", "tema": "triple exponential average",
	"highest": "highest", "lowest": "lowest", "rsi": "RSI", "atr": "ATR", "adx": "ADX",
	"bollinger": "Bollinger bands", "macd": "MACD", "stoch": "stochastic",
}

var opWords = map[string]string{
	">": "is above", "<": "is below", ">=": "is at or above", "<=": "is at or below",
	"crosses_above": "crosses above", "crosses_below": "crosses below",
}

// Kinds are the indicator kinds a spec may use, sorted.
func Kinds() []string {
	out := make([]string, 0, len(kindWords))
	for k := range kindWords {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Rules are the entry and exit conditions in words: why a trade opened or closed on a signal.
func (s Spec) Rules() (entry, exit string) {
	group := s.groupWords()
	return group(s.Entry), group(s.Exit)
}

// Describe is the plain-language readback a person checks before a bot starts.
func (s Spec) Describe() string {
	var b strings.Builder
	group := s.groupWords()
	fmt.Fprintf(&b, "%s trades %s candles, long only.\n", s.Name, s.Timeframe)
	if s.Model != nil {
		b.WriteString("A FreqAI model, retrained as the bot's FreqAI config says, predicts the mean return over the next candles from price, volume and indicator features.\n")
	}
	fmt.Fprintf(&b, "Buy when %s.\n", group(s.Entry))
	fmt.Fprintf(&b, "Sell when %s", group(s.Exit))
	fmt.Fprintf(&b, ", or when the loss reaches %s%%", num(-s.Stoploss*100))
	if t := s.Trailing; t != nil {
		if t.Offset > 0 {
			fmt.Fprintf(&b, ", or %s%% below the peak once up %s%%", num(t.Positive*100), num(t.Offset*100))
		} else {
			fmt.Fprintf(&b, ", or %s%% below the peak", num(t.Positive*100))
		}
	}
	for _, k := range s.roiKeys() {
		if s.ROI[k] >= 100 {
			continue
		}
		if k == "0" {
			fmt.Fprintf(&b, ", or at %s%% profit", num(s.ROI[k]*100))
		} else {
			fmt.Fprintf(&b, ", or after %s minutes at %s%% profit", k, num(s.ROI[k]*100))
		}
	}
	b.WriteString(".\n")
	if g := s.EntryGuard; g != nil {
		fmt.Fprintf(&b, "Skip a buy that would fill more than %s%% above the last close.\n", num(g.MaxAboveClose*100))
	}
	fmt.Fprintf(&b, "It needs %d candles of history before the first signal.", s.Startup())
	return b.String()
}

// groupWords renders a condition group in words, operands by their indicator names.
func (s Spec) groupWords() func(Group) string {
	names := map[string]string{
		"prediction": "predicted mean return over the model's horizon",
		"do_predict": "model's confidence flag",
	}
	for _, in := range s.Indicators {
		names[in.ID] = in.words("")
		for _, f := range fields[in.Kind] {
			names[in.ID+"."+f] = in.words(f)
		}
	}
	ref := func(o Operand) string {
		if o.Num != nil {
			return num(*o.Num)
		}
		if n, ok := names[o.Ref]; ok {
			return "the " + n
		}
		return "the " + o.Ref
	}
	group := func(g Group) string {
		conds, join := g.All, " and "
		if len(g.Any) > 0 {
			conds, join = g.Any, " or "
		}
		parts := make([]string, len(conds))
		for i, c := range conds {
			switch {
			case c.Left.Ref == "do_predict" && c.Op == "==" && c.Right.Num != nil && *c.Right.Num == 1:
				parts[i] = "the model trusts its prediction"
			case c.Op == "==":
				parts[i] = fmt.Sprintf("%s equals %s", ref(c.Left), ref(c.Right))
			case c.Op == "rising" || c.Op == "falling":
				parts[i] = fmt.Sprintf("%s is %s over %d candles", ref(c.Left), c.Op, c.Bars)
			case c.Left.Ref == "prediction" && c.Right.Num != nil:
				parts[i] = fmt.Sprintf("%s %s %s%%", ref(c.Left), opWords[c.Op], num(*c.Right.Num*100))
			default:
				parts[i] = fmt.Sprintf("%s %s %s", ref(c.Left), opWords[c.Op], ref(c.Right))
			}
		}
		return strings.Join(parts, join)
	}
	return group
}

func (in Indicator) words(field string) string {
	var w string
	switch in.Kind {
	case "sma":
		w = fmt.Sprintf("%d-candle average of the %s", in.Period, in.Source)
	case "ema", "tema":
		w = fmt.Sprintf("%d-candle %s of the %s", in.Period, kindWords[in.Kind], in.Source)
	case "highest", "lowest":
		w = fmt.Sprintf("%s %s of the last %d candles", in.Kind, in.Source, in.Period)
	case "rsi":
		w = fmt.Sprintf("%d-candle RSI", in.Period)
	case "atr", "adx":
		w = fmt.Sprintf("%d-candle %s", in.Period, kindWords[in.Kind])
	case "bollinger":
		w = fmt.Sprintf("%s Bollinger band (%d candles, %s deviations)", field, in.Period, num(in.StdDev))
	case "macd":
		w = fmt.Sprintf("MACD %s (%d/%d/%d)", map[string]string{"macd": "line", "signal": "signal line", "hist": "histogram"}[field], in.Fast, in.Slow, in.Signal)
	case "stoch":
		w = fmt.Sprintf("stochastic %%%s (%d)", strings.ToUpper(field), in.Period)
	}
	switch {
	case in.Shift == 1:
		w += " up to the previous candle"
	case in.Shift > 1:
		w += fmt.Sprintf(" as of %d candles back", in.Shift)
	}
	return w
}
