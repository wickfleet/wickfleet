package strategy

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

const rsiDip = `{"version":1,"name":"RsiDip","timeframe":"15m","stoploss":-0.05,"roi":{"0":0.1,"60":0.04},
 "indicators":[{"id":"rsi","kind":"rsi","source":"close","period":14}],
 "entry":{"all":[{"left":"rsi","op":"<","right":30}]},"exit":{"all":[{"left":"rsi","op":">","right":70}]}}`

const emaCross = `{"version":1,"name":"EmaCross","timeframe":"1h","stoploss":-0.1,
 "indicators":[{"id":"fast","kind":"ema","source":"close","period":9},{"id":"slow","kind":"ema","source":"close","period":21},
  {"id":"bb","kind":"bollinger","source":"close","period":20,"stddev":2}],
 "entry":{"any":[{"left":"fast","op":"crosses_above","right":"slow"},{"left":"close","op":"<","right":"bb.lower"}]},
 "exit":{"all":[{"left":"fast","op":"falling","bars":2}]}}`

func spec(t *testing.T, js string) Spec {
	t.Helper()
	s, err := Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var digit = regexp.MustCompile(`[0-9]`)

func TestWhyRSI(t *testing.T) {
	s := spec(t, rsiDip)
	f := Frame{Time: []int64{0, 900, 1800}, Close: []float64{100, 99, 98}}
	cols := map[string][]float64{"rsi": {35, 28, 27.43}}
	text, plain := s.Why(true, f, cols, 2)
	if text != "bought: RSI was 27.4, under 30 = oversold" || plain != "bought: RSI was under the buy level = oversold" {
		t.Fatalf("%q %q", text, plain)
	}
	// RSI 35 did not signal: the values are not what the strategy acted on, so only the rule shows.
	if text, _ := s.Why(true, f, cols, 0); text != "bought: RSI was under 30 = oversold" {
		t.Fatalf("contradicting values shown: %q", text)
	}
	if text, _ := s.Why(true, f, cols, -1); text != "bought: RSI was under 30 = oversold" {
		t.Fatalf("no candle: %q", text)
	}
	cols["rsi"][2] = 71.26
	if text, plain := s.Why(false, f, cols, 2); text != "sold: RSI was 71.3, above 70 = overbought" || digit.MatchString(plain) {
		t.Fatalf("%q %q", text, plain)
	}
	// Three digits would read 30.0 or 70.0: the value never contradicts its level.
	cols["rsi"][2] = 29.96
	if text, _ := s.Why(true, f, cols, 2); text != "bought: RSI was 29.96, under 30 = oversold" {
		t.Fatalf("rounded onto its level: %q", text)
	}
	cols["rsi"][2] = 70.0004
	if text, _ := s.Why(false, f, cols, 2); text != "sold: RSI was 70.0004, above 70 = overbought" {
		t.Fatalf("rounded onto its level: %q", text)
	}
	c := spec(t, strings.Replace(rsiDip, `"op":"<"`, `"op":"crosses_above"`, 1))
	cols["rsi"] = []float64{35, 29.96, 30.04}
	if text, _ := c.Why(true, f, cols, 2); text != "bought: RSI crossed above 30 (29.96 -> 30.04)" {
		t.Fatalf("a cross rounded onto its level: %q", text)
	}
}

func TestWhyValuesApart(t *testing.T) {
	td := load(t, "EmaCross")
	f := Frame{Time: []int64{0, 86400}, Close: []float64{63000, 64000.04}}
	cols := map[string][]float64{"ema12": {63900, 64000.03}, "ema26": {63950, 64000.02}, "sma200": {60000, 64000.01}}
	want := "bought: EMA 12 crossed above EMA 26 (64000.03 vs 64000.02) and the price was 64000.04, above SMA 200 at 64000.01"
	if text, _ := td.Why(true, f, cols, 1); text != want {
		t.Fatalf("rounded together: %q", text)
	}
	s := spec(t, emaCross)
	f = Frame{Time: []int64{0, 3600, 7200}, Close: []float64{64000, 64100, 64200}}
	cols = map[string][]float64{"fast": {63300, 63400, 63500.04}, "slow": {63460, 63450, 63500.02}, "bb.lower": {62000, 62000, 62000}}
	if text, _ := s.Why(true, f, cols, 2); text != "bought: EMA 9 crossed above EMA 21 (63500.04 vs 63500.02)" {
		t.Fatalf("a cross between equal numbers: %q", text)
	}
	cols["fast"] = []float64{63500.049, 63500.045, 63500.04}
	if text, _ := s.Why(false, f, cols, 2); text != "sold: EMA 9 was falling over 2 candles (63500.05 -> 63500.04)" {
		t.Fatalf("falling to the same number: %q", text)
	}
}

func TestWhyCrossAndAny(t *testing.T) {
	s := spec(t, emaCross)
	f := Frame{Time: []int64{0, 3600, 7200}, Close: []float64{64000, 64100, 64200}}
	cols := map[string][]float64{"fast": {63300, 63400, 63500.1}, "slow": {63460, 63450, 63480.7}, "bb.lower": {62000, 62000, 62000}}
	text, plain := s.Why(true, f, cols, 2)
	// Of an any group, only the condition that held is the reason.
	if text != "bought: EMA 9 crossed above EMA 21 (63500.1 vs 63480.7)" || plain != "bought: the fast EMA crossed above the slow EMA" {
		t.Fatalf("%q %q", text, plain)
	}
	if text, _ := s.Why(true, f, cols, 1); text != "bought: EMA 9 crossed above EMA 21 or the price was under the lower Bollinger band" {
		t.Fatalf("no condition held: %q", text)
	}
	cols["fast"] = []float64{63600, 63550, 63500.1}
	if text, plain := s.Why(false, f, cols, 2); text != "sold: EMA 9 was falling over 2 candles (63600.0 -> 63500.1)" || plain != "sold: the fast EMA was falling" {
		t.Fatalf("%q %q", text, plain)
	}
	td := load(t, "EmaCross")
	f = Frame{Time: []int64{0, 86400}, Close: []float64{63000, 64210.5}}
	cols = map[string][]float64{"ema12": {62000, 63100.5}, "ema26": {62500, 62900.2}, "sma200": {58000, 58000}}
	if text, plain := td.Why(true, f, cols, 1); text != "bought: EMA 12 crossed above EMA 26 (63100.5 vs 62900.2) and the price was 64210.5, above SMA 200 at 58000.0" ||
		plain != "bought: the fast EMA crossed above the slow EMA and the price was above the SMA" {
		t.Fatalf("%q %q", text, plain)
	}
}

// A model's columns are not computed here: the rule stands in for its values.
func TestWhyModel(t *testing.T) {
	s := load(t, "FreqaiExample")
	text, plain := s.Why(true, Frame{Time: []int64{0}, Close: []float64{1}}, s.Columns(Frame{}), 0)
	if text != "bought: the model trusted its prediction and the model's predicted return was above 2%" || digit.MatchString(plain) {
		t.Fatalf("%q %q", text, plain)
	}
}

func TestExitReason(t *testing.T) {
	s := spec(t, rsiDip)
	for reason, want := range map[string]string{
		"stop_loss":          "sold: the stop loss at -5% hit",
		"roi":                "sold: the profit target was reached (+4.1%)",
		"trailing_stop_loss": "sold: the trailing stop hit",
		"force_exit":         "still open: valued at the last price of the record",
	} {
		text, plain := s.ExitReason(reason, 4.12)
		if text != want || digit.MatchString(plain) {
			t.Errorf("%s: %q %q", reason, text, plain)
		}
	}
	s.Trailing = &Trailing{Positive: 0.02}
	if text, _ := s.ExitReason("trailing_stop_loss", 1); text != "sold: the price fell 2% back from its peak (the trailing stop)" {
		t.Fatal(text)
	}
}

func TestDiff(t *testing.T) {
	p := spec(t, rsiDip)
	c := spec(t, rsiDip)
	c.Stoploss, c.Indicators[0].Period, c.ROI["60"] = -0.08, 17, 0.045
	want := []Gene{{"stop loss", "-5%", "-8%"}, {"profit targets", "10% after 0 min, 4% after 60 min", "10% after 0 min, 4.5% after 60 min"},
		{"RSI period", "14", "17"}}
	if got := Diff(p, c); !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	// A cousin changes what it watches and its rules.
	e := spec(t, emaCross)
	got := Diff(p, e)
	var names []string
	for _, g := range got {
		names = append(names, g.Name)
	}
	if strings.Join(names, ",") != "timeframe,stop loss,profit targets,fast EMA,slow EMA,Bollinger bands,RSI,buy rule,sell rule" ||
		got[3].Parent != "none" || got[3].Child != "EMA 9" || got[6].Child != "none" || !strings.Contains(got[7].Child, "crosses above") {
		t.Fatalf("%+v", got)
	}
	if len(Diff(p, p)) != 0 {
		t.Fatal("no genes changed")
	}
}

// emaCross as a cousin drafts it from the readback: its own ids, the EMAs'
// swapped, in another order.
const emaCousin = `{"version":1,"name":"EmaCousin","timeframe":"1h","stoploss":-0.1,
 "indicators":[{"id":"band","kind":"bollinger","source":"close","period":20,"stddev":2},
  {"id":"slow","kind":"ema","source":"close","period":9},{"id":"fast","kind":"ema","source":"close","period":21}],
 "entry":{"any":[{"left":"slow","op":"crosses_above","right":"fast"},{"left":"close","op":"<","right":"band.lower"}]},
 "exit":{"all":[{"left":"slow","op":"falling","bars":2}]}}`

func TestDiffCousin(t *testing.T) {
	if got := Diff(spec(t, emaCross), spec(t, emaCousin)); len(got) != 0 {
		t.Fatalf("other ids, same strategy: %+v", got)
	}
	p := spec(t, rsiDip)
	cousin := func(r ...string) Spec {
		return spec(t, strings.NewReplacer(append([]string{`"id":"rsi"`, `"id":"r"`, `"left":"rsi"`, `"left":"r"`}, r...)...).Replace(rsiDip))
	}
	want := []Gene{{"buy rule", "the 14-candle RSI is below 30", "the 14-candle RSI is below 25"}}
	if got := Diff(p, cousin(`"right":30`, `"right":25`)); !slices.Equal(got, want) {
		t.Fatalf("threshold: %+v", got)
	}
	if got := Diff(p, cousin(`"period":14`, `"period":10`)); !slices.Equal(got, []Gene{{"RSI period", "14", "10"}}) {
		t.Fatalf("period: %+v", got)
	}
	// Same ids on swapped indicators: the rules now watch the other RSI.
	two := `{"version":1,"name":"TwoRsi","timeframe":"15m","stoploss":-0.05,
	 "indicators":[{"id":"a","kind":"rsi","source":"close","period":14},{"id":"b","kind":"rsi","source":"close","period":7}],
	 "entry":{"all":[{"left":"a","op":"<","right":30}]},"exit":{"all":[{"left":"b","op":">","right":70}]}}`
	swapped := strings.NewReplacer(`"period":14`, `"period":7`, `"period":7`, `"period":14`).Replace(two)
	got := Diff(spec(t, two), spec(t, swapped))
	if len(got) != 2 || got[0].Name != "buy rule" || got[0].Child != "the 7-candle RSI is below 30" || got[1].Name != "sell rule" {
		t.Fatalf("swapped: %+v", got)
	}
	// A cousin lists its indicators in its own order: fast still pairs with fast.
	trend := `{"version":1,"name":"TrendCousin","timeframe":"1d","stoploss":-0.15,
	 "indicators":[{"id":"long_avg","kind":"sma","source":"close","period":150},
	  {"id":"slow_line","kind":"ema","source":"close","period":30},
	  {"id":"fast_line","kind":"ema","source":"close","period":10}],
	 "entry":{"all":[{"left":"fast_line","op":"crosses_above","right":"slow_line"},{"left":"close","op":">","right":"long_avg"}]},
	 "exit":{"any":[{"left":"fast_line","op":"crosses_below","right":"slow_line"}]}}`
	periods := func(gs []Gene) string {
		var out []string
		for _, g := range gs {
			out = append(out, g.Name+" "+g.Parent+">"+g.Child)
		}
		slices.Sort(out)
		return strings.Join(out, "; ")
	}
	if got := periods(Diff(load(t, "EmaCross"), spec(t, trend))); got != "SMA period 200>150; fast EMA period 12>10; slow EMA period 26>30" {
		t.Fatalf("trend cousin: %s", got)
	}
	ema := `{"version":1,"name":"EmaCousin2","timeframe":"1h","stoploss":-0.1,
	 "indicators":[{"id":"band","kind":"bollinger","source":"close","period":20,"stddev":2},
	  {"id":"long","kind":"ema","source":"close","period":50},{"id":"short","kind":"ema","source":"close","period":10}],
	 "entry":{"any":[{"left":"short","op":"crosses_above","right":"long"},{"left":"close","op":"<","right":"band.lower"}]},
	 "exit":{"all":[{"left":"short","op":"falling","bars":2}]}}`
	if got := periods(Diff(spec(t, emaCross), spec(t, ema))); got != "fast EMA period 9>10; slow EMA period 21>50" {
		t.Fatalf("ema cousin: %s", got)
	}
}

func TestChart(t *testing.T) {
	got := load(t, "AllKinds").Chart()
	want := map[string]Series{"bb.lower": {"bb.lower", "lower Bollinger band", true}, "rsi14": {"rsi14", "RSI", false},
		"m.hist": {"m.hist", "MACD histogram", false}, "hi10": {"hi10", "10-candle high", true}, "ema12": {"ema12", "EMA 12", true}}
	n := 0
	for _, s := range got {
		if w, ok := want[s.Column]; ok {
			if s != w {
				t.Errorf("%+v, want %+v", s, w)
			}
			n++
		}
	}
	if n != len(want) || len(got) != 16 {
		t.Fatalf("%d series: %+v", len(got), got)
	}
}

func TestVal(t *testing.T) {
	for v, want := range map[float64]string{27.43: "27.4", 64210.53: "64210.5", 0.08213: "0.0821", 0: "0", -12.34: "-12.3", 100: "100.0", 2.5e-7: "0.000000250"} {
		if got := val(v, 0); got != want {
			t.Errorf("val(%v) = %s, want %s", v, got, want)
		}
	}
}
