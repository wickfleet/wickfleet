package strategy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const examples = "../../examples/registry/strategies/"

func load(t *testing.T, name string) Spec {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if os.IsNotExist(err) {
		raw, err = os.ReadFile(examples + name + ".json")
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Each example spec ships its generated source. EmaCross and RsiReversion backtest
// trade-for-trade like their twins in examples/equivalence (make equivalence).
func TestExamplesGenerated(t *testing.T) {
	specs, _ := filepath.Glob(examples + "*.json")
	if len(specs) < 2 {
		t.Fatalf("example specs %v", specs)
	}
	for _, path := range specs {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		got, err := load(t, name).Python()
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(examples + name + ".py")
		if got != string(want) {
			t.Fatalf("%s: generated source changed:\n%s", name, got)
		}
	}
}

// The golden file pins the generated FreqAI source. Trade-for-trade equivalence
// of generated and hand-written strategies is proven by scripts/equivalence.
func TestFreqaiExampleGolden(t *testing.T) {
	s := load(t, "FreqaiExample")
	got, err := s.Python()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/FreqaiExample.py.golden")
	if got != string(want) {
		t.Fatalf("generated source changed:\n%s", got)
	}
	if !strings.Contains(s.Describe(), "the model trusts its prediction and the predicted mean return over the model's horizon is above 2%") {
		t.Fatalf("readback %q", s.Describe())
	}
}

func TestModelRules(t *testing.T) {
	m := func(mut func(map[string]any)) error {
		raw, _ := os.ReadFile("testdata/FreqaiExample.json")
		var x map[string]any
		json.Unmarshal(raw, &x)
		mut(x)
		_, err := Parse(mustJSON(x))
		return err
	}
	if err := m(func(map[string]any) {}); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(map[string]any){
		"no startup with a model": func(x map[string]any) { delete(x, "startup_candles") },
		"unknown feature set":     func(x map[string]any) { x["model"].(map[string]any)["features"] = "mine" },
		"model operand, no model": func(x map[string]any) { delete(x, "model") },
		"== against a column": func(x map[string]any) {
			x["entry"].(map[string]any)["all"].([]any)[0].(map[string]any)["right"] = "close"
		},
		"guard too wide": func(x map[string]any) { x["entry_guard"] = map[string]any{"max_above_close": 0.5} },
	} {
		if m(mut) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllKindsGenerates(t *testing.T) {
	s := load(t, "AllKinds")
	src, err := s.Python()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"import talib.abstract as ta",
		`nbdevup=2.0, nbdevdn=2.0`, // talib rejects an int here
		`df["m.macd"], df["m.signal"], df["m.hist"] = _macd["macd"], _macd["macdsignal"], _macd["macdhist"]`,
		`(df["ema12"] > df["sma20"]) & (df["ema12"].shift(1) <= df["sma20"].shift(1))`,
		`(df["rsi14"] > 30) & (df["rsi14"].shift(1) <= 30)`,
		`df["adx14"] > df["adx14"].shift(3)`,
		`minimal_roi = {"0": 0.5, "1440": 0.2}`,
		"trailing_only_offset_is_reached = True",
		"startup_candle_count = 42",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q", want)
		}
	}
	if s.Startup() != 42 {
		t.Fatalf("startup %d", s.Startup())
	}
}

func TestDescribe(t *testing.T) {
	got := load(t, "EmaCross").Describe()
	want := "EmaCross trades 1d candles, long only.\n" +
		"Buy when the 12-candle exponential average of the close crosses above the 26-candle exponential average of the close and the close is above the 200-candle average of the close.\n" +
		"Sell when the 12-candle exponential average of the close crosses below the 26-candle exponential average of the close, or when the loss reaches 15%.\n" +
		"It needs 200 candles of history before the first signal."
	if got != want {
		t.Fatalf("got\n%s", got)
	}
	entry, exit := load(t, "EmaCross").Rules()
	if entry != "the 12-candle exponential average of the close crosses above the 26-candle exponential average of the close and the close is above the 200-candle average of the close" ||
		exit != "the 12-candle exponential average of the close crosses below the 26-candle exponential average of the close" {
		t.Fatalf("rules %q / %q", entry, exit)
	}
}

// Kinds is every kind the validator takes; AllKinds uses each once.
func TestKinds(t *testing.T) {
	var used []string
	for _, in := range load(t, "AllKinds").Indicators {
		used = append(used, in.Kind)
	}
	sort.Strings(used)
	if got := strings.Join(Kinds(), ","); got != strings.Join(used, ",") || len(used) != 11 {
		t.Fatalf("Kinds %s, AllKinds uses %v", got, used)
	}
}

// Every rejection below is a way user text could reach the Python source or
// a spec could mean something other than it says.
func TestRejects(t *testing.T) {
	base := func() map[string]any {
		raw, _ := os.ReadFile(examples + "EmaCross.json")
		var m map[string]any
		json.Unmarshal(raw, &m)
		return m
	}
	ind := func(m map[string]any, i int) map[string]any {
		return m["indicators"].([]any)[i].(map[string]any)
	}
	cond := func(m map[string]any, side string, i int) map[string]any {
		g := m[side].(map[string]any)
		for _, k := range []string{"all", "any"} {
			if c, ok := g[k]; ok {
				return c.([]any)[i].(map[string]any)
			}
		}
		return nil
	}
	cases := map[string]func(m map[string]any){
		"code in name":        func(m map[string]any) { m["name"] = "X(IStrategy):\n    import os" },
		"lower-case name":     func(m map[string]any) { m["name"] = "trend" },
		"quote in desc":       func(m map[string]any) { m["description"] = `say "hi"` },
		"newline in desc":     func(m map[string]any) { m["description"] = "a\nimport os" },
		"version":             func(m map[string]any) { m["version"] = 2 },
		"timeframe":           func(m map[string]any) { m["timeframe"] = "7m" },
		"positive stoploss":   func(m map[string]any) { m["stoploss"] = 0.1 },
		"unknown field":       func(m map[string]any) { m["exec"] = "x" },
		"code in id":          func(m map[string]any) { ind(m, 0)["id"] = `a"]; import os; df["b` },
		"duplicate id":        func(m map[string]any) { ind(m, 1)["id"] = "sma50" },
		"id is a price":       func(m map[string]any) { ind(m, 0)["id"] = "close" },
		"unknown kind":        func(m map[string]any) { ind(m, 0)["kind"] = "eval" },
		"bad source":          func(m map[string]any) { ind(m, 0)["source"] = "hlc3" },
		"huge period":         func(m map[string]any) { ind(m, 0)["period"] = 100000 },
		"stray param":         func(m map[string]any) { ind(m, 0)["stddev"] = 2 },
		"unknown ref":         func(m map[string]any) { cond(m, "entry", 0)["right"] = "sma100" },
		"code in ref":         func(m map[string]any) { cond(m, "entry", 0)["right"] = `sma50"] or __import__("os")` },
		"number on the left":  func(m map[string]any) { cond(m, "entry", 0)["left"] = 5 },
		"unknown op":          func(m map[string]any) { cond(m, "entry", 0)["op"] = "==" },
		"rising with right":   func(m map[string]any) { cond(m, "exit", 0)["op"] = "rising"; cond(m, "exit", 0)["bars"] = 2 },
		"bars on a compare":   func(m map[string]any) { cond(m, "exit", 0)["bars"] = 2 },
		"all and any":         func(m map[string]any) { m["entry"].(map[string]any)["any"] = m["exit"].(map[string]any)["any"] },
		"empty exit":          func(m map[string]any) { m["exit"] = map[string]any{} },
		"bad roi key":         func(m map[string]any) { m["roi"] = map[string]any{"0); import os; (": 0.1} },
		"trailing below stop": func(m map[string]any) { m["trailing"] = map[string]any{"positive": 0.05, "offset": 0.01} },
	}
	for name, mutate := range cases {
		m := base()
		mutate(m)
		raw, _ := json.Marshal(m)
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(mustJSON(base())); err != nil {
		t.Fatalf("the unmutated base must pass: %v", err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestOperandJSON(t *testing.T) {
	var c Condition
	if err := json.Unmarshal([]byte(`{"left":"rsi14","op":"<","right":30}`), &c); err != nil || c.Right.Num == nil || *c.Right.Num != 30 || c.Left.Ref != "rsi14" {
		t.Fatalf("%+v %v", c, err)
	}
	out, _ := json.Marshal(c)
	var back Condition
	if err := json.Unmarshal(out, &back); err != nil || back.Op != "<" || back.Left.Ref != "rsi14" || *back.Right.Num != 30 || back.Right.Ref != "" {
		t.Fatalf("round trip %s %+v", out, back)
	}
}
