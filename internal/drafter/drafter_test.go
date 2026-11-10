package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const valid = `{"version":1,"name":"RsiDip","timeframe":"1h","stoploss":-0.05,
 "indicators":[{"id":"rsi14","kind":"rsi","source":"close","period":14}],
 "entry":{"all":[{"left":"rsi14","op":"crosses_above","right":30}]},
 "exit":{"any":[{"left":"rsi14","op":">","right":70}]}}`

type script struct {
	name    string
	replies []string
	errs    []error
	seen    [][]Message
}

func (s *script) Name() string { return s.name }
func (s *script) Complete(_ context.Context, _ string, msgs []Message, _ json.RawMessage) (string, error) {
	i := len(s.seen)
	s.seen = append(s.seen, append([]Message(nil), msgs...))
	if i < len(s.errs) && s.errs[i] != nil {
		return "", s.errs[i]
	}
	if i < len(s.replies) {
		return s.replies[i], nil
	}
	return "", errors.New("script ran out")
}

func TestRetryCarriesTheValidationError(t *testing.T) {
	p := &script{name: "fake", replies: []string{
		`{"version":1,"name":"bad name","timeframe":"1h","stoploss":-0.05,"indicators":[],"entry":{"all":[]},"exit":{"all":[]}}`,
		"Here you go:\n```json\n" + valid + "\n```",
	}}
	r, err := Drafter{Providers: []Provider{p}}.Draft(context.Background(), "buy the rsi dip on hourly candles")
	if err != nil {
		t.Fatal(err)
	}
	if r.Attempts != 2 || r.Spec.Name != "RsiDip" || r.Provider != "fake" || !strings.Contains(r.Python, "class RsiDip") {
		t.Fatalf("%+v", r)
	}
	second := p.seen[1]
	if len(second) != 3 || second[1].Role != "assistant" || !strings.Contains(second[2].Content, `name "bad name"`) {
		t.Fatalf("the retry must show the reply and the error: %+v", second)
	}
}

func TestDownProviderFallsThrough(t *testing.T) {
	down := &script{name: "down", errs: []error{errors.New("connection refused")}}
	up := &script{name: "up", replies: []string{valid}}
	r, err := Drafter{Providers: []Provider{down, up}}.Draft(context.Background(), "buy the rsi dip on hourly candles")
	if err != nil || r.Provider != "up" || len(down.seen) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestNoCallIsRetried(t *testing.T) {
	p := &script{name: "claude", errs: []error{ErrNoCall, nil}, replies: []string{"", valid}}
	r, err := Drafter{Providers: []Provider{p}}.Draft(context.Background(), "buy the rsi dip on hourly candles")
	if err != nil || r.Attempts != 2 || !strings.Contains(p.seen[1][1].Content, "submit_spec") {
		t.Fatalf("%+v %v %+v", r, err, p.seen)
	}
}

func TestGivesUp(t *testing.T) {
	p := &script{name: "fake", replies: []string{"nope", "still nope"}}
	if _, err := (Drafter{Providers: []Provider{p}}).Draft(context.Background(), "buy the rsi dip on hourly candles"); !errors.Is(err, ErrNoDraft) {
		t.Fatalf("%v", err)
	}
	if _, err := (Drafter{Providers: []Provider{p}}).Draft(context.Background(), "short"); err == nil {
		t.Fatal("a too short sentence must be refused")
	}
}

func TestOllamaRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.Error(w, "path", 404)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": valid}})
	}))
	defer srv.Close()
	out, err := Ollama{Base: srv.URL, Model: "qwen3:8b"}.Complete(context.Background(), "sys", []Message{{"user", "hi"}}, Schema())
	if err != nil || out != valid {
		t.Fatalf("%q %v", out, err)
	}
	msgs := got["messages"].([]any)
	if got["model"] != "qwen3:8b" || got["stream"] != false || got["think"] != false || got["format"] == nil ||
		got["options"].(map[string]any)["temperature"] != float64(0) || got["options"].(map[string]any)["num_ctx"] != float64(8192) ||
		msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("%+v", got)
	}
	ctx := WithTemperature(context.Background(), 0.9)
	if _, err := (Ollama{Base: srv.URL, Model: "qwen3:8b"}).Complete(ctx, "sys", []Message{{"user", "hi"}}, Schema()); err != nil {
		t.Fatal(err)
	}
	if got["options"].(map[string]any)["temperature"] != 0.9 {
		t.Fatalf("a call may ask for a higher temperature: %+v", got["options"])
	}
}

// The fake speaks the Messages API: a tool_use reply, and no forced tool_choice
// sent, since current models refuse one.
func TestAnthropicRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "k" {
			http.Error(w, "unexpected "+r.URL.Path, 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"text","text":"Submitting."},{"type":"tool_use","id":"tu_1","name":"submit_spec","input":` + valid + `}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer srv.Close()
	out, err := Anthropic{Key: "k", Model: "claude-opus-5-5", BaseURL: srv.URL}.Complete(context.Background(), "sys", []Message{{"user", "hi"}}, Schema())
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	json.Unmarshal([]byte(out), &a)
	json.Unmarshal([]byte(valid), &b)
	if !jsonEq(a, b) {
		t.Fatalf("tool input %s", out)
	}
	tools := got["tools"].([]any)
	if got["model"] != "claude-opus-5-5" || len(tools) != 1 || tools[0].(map[string]any)["name"] != "submit_spec" {
		t.Fatalf("%+v", got)
	}
	in := tools[0].(map[string]any)["input_schema"].(map[string]any)
	if in["$defs"] == nil || in["properties"].(map[string]any)["indicators"] == nil {
		t.Fatalf("schema must carry properties and $defs: %+v", in)
	}
	if _, forced := got["tool_choice"]; forced {
		t.Fatal("tool_choice must not be forced")
	}
}

// The API refuses a union at the top of a tool input, so it travels in one
// field: Claude sees every form with its enums, the caller gets the bare answer.
func TestAnthropicUnionSchema(t *testing.T) {
	union := `{"anyOf":[{"type":"object","properties":{"act":{"enum":["breed"]},"agent":{"enum":["ai-rsi"]}}},` +
		`{"type":"object","properties":{"act":{"enum":["pass"]},"agent":{"type":"string"}}}]}`
	answer := `{"act":"breed","agent":"ai-rsi"}`
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		reply := `{"answer":` + answer + `}`
		if strings.Contains(string(b), "bare") { // a Claude that skips the wrapper
			reply = answer
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"content":[{"type":"tool_use","id":"tu_1","name":"submit_spec","input":` + reply + `}],
			"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer srv.Close()
	p := Anthropic{Key: "k", Model: "claude-opus-5-5", BaseURL: srv.URL}
	out, err := p.Complete(context.Background(), "sys", []Message{{"user", "hi"}}, json.RawMessage(union))
	if err != nil || out != answer {
		t.Fatalf("%q %v", out, err)
	}
	in := got["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	var want any
	json.Unmarshal([]byte(union), &want)
	if _, top := in["anyOf"]; top || !jsonEq(in["properties"].(map[string]any)["answer"], want) || !jsonEq(in["required"], []string{"answer"}) {
		t.Fatalf("input_schema %+v", in)
	}
	if out, err := p.Complete(context.Background(), "bare", []Message{{"user", "hi"}}, json.RawMessage(union)); err != nil || out != answer {
		t.Fatalf("unwrapped %q %v", out, err)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// The examples in the prompt must be specs the validator accepts.
func TestPromptExamplesAreValid(t *testing.T) {
	n := 0
	for _, line := range strings.Split(SystemPrompt, "\n{") {
		if !strings.HasPrefix(line, `"version"`) {
			continue
		}
		obj := "{" + line
		if i := strings.Index(obj, "\n\nExample"); i > 0 {
			obj = obj[:i]
		}
		if _, err := (Drafter{Providers: []Provider{&script{name: "x", replies: []string{obj}}}}).Draft(context.Background(), "example from the prompt"); err != nil {
			t.Fatalf("example %d: %v\n%s", n, err, obj)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("found %d examples", n)
	}

}

func TestEvalScoring(t *testing.T) {
	cases, err := LoadCases("testdata/eval.json")
	if err != nil || len(cases) != 10 {
		t.Fatalf("%d %v", len(cases), err)
	}
	// The fixed RSI reply fits the first case and misses the second.
	two := []Case{
		{Sentence: "hourly rsi dip buyer for testing", Timeframe: "1h", Kinds: []string{"rsi"}, Ops: []string{"crosses_above", ">"}},
		{Sentence: "daily breakout buyer for testing", Timeframe: "1d", Kinds: []string{"highest"}, Ops: []string{">"}},
	}
	p := &script{name: "fake", replies: []string{valid, valid}}
	var log strings.Builder
	sc := Eval(context.Background(), p, two, &log)
	if sc.Valid != 2 || sc.Correct != 1 || sc.Cases != 2 {
		t.Fatalf("%+v\n%s", sc, log.String())
	}
	if !strings.Contains(log.String(), "missing [timeframe 1d kind highest]") {
		t.Fatalf("log %s", log.String())
	}
}

func TestSchemaIsOrderedJSON(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(Schema(), &m); err != nil {
		t.Fatal(err)
	}
	s := string(Schema())
	order := []string{`"version"`, `"name"`, `"timeframe"`, `"stoploss"`, `"indicators"`, `"entry"`, `"exit"`}
	last := -1
	for _, k := range order {
		i := strings.Index(s, k)
		if i < last {
			t.Fatalf("%s out of order: indicators must come before the rules", k)
		}
		last = i
	}
}
