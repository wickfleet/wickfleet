// Package drafter turns one sentence into a strategy spec with a language
// model. The model only proposes JSON; strategy.Parse decides, and a
// rejected draft goes back once with the validation error.
package drafter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"wickfleet/internal/strategy"
)

// Provider answers with one JSON object for the system and user prompt.
type Provider interface {
	Name() string
	Complete(ctx context.Context, system string, msgs []Message, schema json.RawMessage) (string, error)
}

// Message is one chat turn: role user or assistant.
type Message struct {
	Role    string
	Content string
}

// Result is a valid draft and how it came about.
type Result struct {
	Spec     strategy.Spec `json:"spec"`
	Describe string        `json:"describe"`
	Python   string        `json:"python"`
	Provider string        `json:"provider"`
	Attempts int           `json:"attempts"`
	Seconds  float64       `json:"seconds"`
}

// Drafter tries its providers in order; the first valid spec wins.
type Drafter struct {
	Providers []Provider
	// Attempts per provider; the second one sees the validation error.
	Attempts int
}

// ErrNoDraft wraps the last reason every provider failed.
var ErrNoDraft = errors.New("no valid strategy came back")

// Draft asks for a spec matching the sentence.
func (d Drafter) Draft(ctx context.Context, sentence string) (Result, error) {
	sentence = strings.TrimSpace(sentence)
	if len(sentence) < 10 || len(sentence) > 1000 {
		return Result{}, errors.New("describe the strategy in 10 to 1000 characters")
	}
	attempts := d.Attempts
	if attempts < 1 {
		attempts = 2
	}
	var last error = errors.New("no provider configured")
	for _, p := range d.Providers {
		start := time.Now()
		msgs := []Message{{Role: "user", Content: userPrompt(sentence)}}
		for a := 1; a <= attempts; a++ {
			out, err := p.Complete(ctx, SystemPrompt, msgs, Schema())
			if errors.Is(err, ErrNoCall) {
				last = fmt.Errorf("%s: %w", p.Name(), err)
				msgs = append(msgs, Message{Role: "user", Content: "Call the submit_spec tool with the spec."})
				continue
			}
			if err != nil {
				last = fmt.Errorf("%s: %w", p.Name(), err)
				break // a provider that errors is down; try the next one
			}
			s, err := strategy.Parse([]byte(extractJSON(out)))
			if err == nil {
				src, _ := s.Python()
				return Result{Spec: s, Describe: s.Describe(), Python: src, Provider: p.Name(),
					Attempts: a, Seconds: time.Since(start).Seconds()}, nil
			}
			last = fmt.Errorf("%s: %w", p.Name(), err)
			msgs = append(msgs, Message{Role: "assistant", Content: out},
				Message{Role: "user", Content: "That spec is invalid: " + err.Error() + ". Return the corrected spec as JSON only."})
		}
	}
	return Result{}, fmt.Errorf("%w: %v", ErrNoDraft, last)
}

// extractJSON tolerates a model that wraps the object in prose or a fence.
func extractJSON(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

func userPrompt(sentence string) string {
	return "Write the strategy spec for this description. Answer with the JSON object only.\n\nDescription: " + sentence
}

// SystemPrompt teaches the format. Its examples are the hub's own testdata.
const SystemPrompt = `You write trading strategies as JSON specs for freqtrade, spot market, long only.

Format (version 1):
- name: a class name, CamelCase letters and digits, e.g. "RsiDip".
- description: one plain sentence, no quotes.
- timeframe: one of 1m 3m 5m 15m 30m 1h 2h 4h 6h 8h 12h 1d 1w. Pick what the description implies; daily if unsure.
- stoploss: a negative ratio, e.g. -0.1 for 10%. Default -0.1 if not given.
- roi (optional): minutes -> profit ratio to take, e.g. {"0": 0.1}. Omit unless the description asks for a profit target.
- trailing (optional): {"positive": 0.02, "offset": 0.05, "only_offset_reached": true}.
- indicators: a list of {"id", "kind", parameters}. id is lower case, starts with a letter, a-z 0-9 _.
  kinds and their parameters, nothing else:
  sma, ema, tema, highest, lowest: source, period, shift (optional; shift 1 = up to the previous candle)
  rsi: source, period.   atr, adx: period.
  bollinger: source, period, stddev -> refer to id.upper, id.middle, id.lower
  macd: source, fast, slow, signal -> refer to id.macd, id.signal, id.hist
  stoch: period, smooth_k, smooth_d -> refer to id.k, id.d
  source is one of open high low close volume.
- entry and exit: {"all": [conditions]} or {"any": [conditions]}, never both, at least one condition.
  A condition is {"left": ref, "op": op, "right": ref or number}, or {"left": ref, "op": "rising" or "falling", "bars": n}.
  ops: > < >= <= crosses_above crosses_below. A ref is a price (open high low close volume) or an indicator id (or id.field).
  left is always a ref, never a number.
- Do not add a model unless the description asks for machine learning.

Example, "daily: buy when EMA 12 crosses above EMA 26 while price is above the 200-day average, sell when it crosses back, 15% stop":
{"version":1,"name":"EmaCross","description":"Daily EMA 12/26 cross above SMA200.","timeframe":"1d","stoploss":-0.15,
 "indicators":[{"id":"ema12","kind":"ema","source":"close","period":12},{"id":"ema26","kind":"ema","source":"close","period":26},{"id":"sma200","kind":"sma","source":"close","period":200}],
 "entry":{"all":[{"left":"ema12","op":"crosses_above","right":"ema26"},{"left":"close","op":">","right":"sma200"}]},
 "exit":{"any":[{"left":"ema12","op":"crosses_below","right":"ema26"}]}}

Example, "hourly: buy when RSI 14 crosses up through 30, sell when it goes above 70, 5% stop":
{"version":1,"name":"RsiDip","description":"Hourly RSI dip buyer.","timeframe":"1h","stoploss":-0.05,
 "indicators":[{"id":"rsi14","kind":"rsi","source":"close","period":14}],
 "entry":{"all":[{"left":"rsi14","op":"crosses_above","right":30}]},
 "exit":{"any":[{"left":"rsi14","op":">","right":70}]}}`

// Schema constrains the providers' output, in generation order: settings,
// then indicators, then the rules that use them. One variant per indicator
// kind and condition form, each with exactly its own fields. It narrows the
// model; strategy.Parse still checks every rule.
func Schema() json.RawMessage { return json.RawMessage(schema) }

const schema = `{
"type": "object",
"properties": {
  "version": {"type": "integer", "enum": [1]},
  "name": {"type": "string"},
  "description": {"type": "string"},
  "timeframe": {"type": "string", "enum": ["1m","3m","5m","15m","30m","1h","2h","4h","6h","8h","12h","1d","1w"]},
  "stoploss": {"type": "number"},
  "roi": {"type": "object", "additionalProperties": {"type": "number"}},
  "trailing": {"type": "object", "properties": {"positive": {"type": "number"}, "offset": {"type": "number"}, "only_offset_reached": {"type": "boolean"}}, "required": ["positive"], "additionalProperties": false},
  "indicators": {"type": "array", "items": {"anyOf": [
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["sma","ema","tema","highest","lowest"]}, "source": {"$ref": "#/$defs/source"}, "period": {"type": "integer"}, "shift": {"type": "integer"}}, "required": ["id","kind","source","period"], "additionalProperties": false},
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["rsi"]}, "source": {"$ref": "#/$defs/source"}, "period": {"type": "integer"}}, "required": ["id","kind","source","period"], "additionalProperties": false},
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["atr","adx"]}, "period": {"type": "integer"}}, "required": ["id","kind","period"], "additionalProperties": false},
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["bollinger"]}, "source": {"$ref": "#/$defs/source"}, "period": {"type": "integer"}, "stddev": {"type": "number"}}, "required": ["id","kind","source","period","stddev"], "additionalProperties": false},
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["macd"]}, "source": {"$ref": "#/$defs/source"}, "fast": {"type": "integer"}, "slow": {"type": "integer"}, "signal": {"type": "integer"}}, "required": ["id","kind","source","fast","slow","signal"], "additionalProperties": false},
    {"type": "object", "properties": {"id": {"type": "string"}, "kind": {"enum": ["stoch"]}, "period": {"type": "integer"}, "smooth_k": {"type": "integer"}, "smooth_d": {"type": "integer"}}, "required": ["id","kind","period","smooth_k","smooth_d"], "additionalProperties": false}
  ]}},
  "entry": {"$ref": "#/$defs/group"},
  "exit": {"$ref": "#/$defs/group"}
},
"required": ["version","name","description","timeframe","stoploss","indicators","entry","exit"],
"additionalProperties": false,
"$defs": {
  "source": {"type": "string", "enum": ["close","open","high","low","volume"]},
  "condition": {"anyOf": [
    {"type": "object", "properties": {"left": {"type": "string"}, "op": {"enum": [">","<",">=","<=","crosses_above","crosses_below"]}, "right": {"anyOf": [{"type": "number"}, {"type": "string"}]}}, "required": ["left","op","right"], "additionalProperties": false},
    {"type": "object", "properties": {"left": {"type": "string"}, "op": {"enum": ["rising","falling"]}, "bars": {"type": "integer"}}, "required": ["left","op","bars"], "additionalProperties": false}
  ]},
  "group": {"anyOf": [
    {"type": "object", "properties": {"all": {"type": "array", "items": {"$ref": "#/$defs/condition"}}}, "required": ["all"], "additionalProperties": false},
    {"type": "object", "properties": {"any": {"type": "array", "items": {"$ref": "#/$defs/condition"}}}, "required": ["any"], "additionalProperties": false}
  ]}
}
}`
