package drafter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type tempKey struct{}

// WithTemperature asks Ollama to sample at t on calls under ctx (default 0);
// Anthropic ignores it, current models refuse sampling parameters.
func WithTemperature(ctx context.Context, t float64) context.Context {
	return context.WithValue(ctx, tempKey{}, t)
}

// Temperature is what ctx asks for, 0 unless WithTemperature set it.
func Temperature(ctx context.Context) float64 {
	t, _ := ctx.Value(tempKey{}).(float64)
	return t
}

// Ollama talks to /api/chat and constrains the reply with the schema.
type Ollama struct {
	Base  string // e.g. http://ollama:11434
	Model string
	HTTP  *http.Client
}

func (o Ollama) Name() string { return "ollama/" + o.Model }

func (o Ollama) Complete(ctx context.Context, system string, msgs []Message, schema json.RawMessage) (string, error) {
	chat := []chatMsg{{"system", system}}
	for _, m := range msgs {
		chat = append(chat, chatMsg{m.Role, m.Content})
	}
	body := map[string]any{
		"model":  o.Model,
		"stream": false,
		"think":  false, // a thinking model reasons for minutes on a CPU first
		"format": schema,
		// A grammar can loop; a capped reply is just invalid and gets the retry.
		// The default 4096 context truncates the prompt once a retry adds history.
		"options":  map[string]any{"temperature": Temperature(ctx), "num_predict": 1200, "num_ctx": 8192},
		"messages": chat,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.Base, "/")+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute} // CPU inference, first load included
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ErrNoCall: the model answered without calling submit_spec; worth a retry.
var ErrNoCall = errors.New("the model did not call submit_spec")

// Anthropic asks Claude to call a submit_spec tool whose input is the spec.
// Current models reject a forced tool_choice, so the prompt asks for the call
// and a reply without one is an error the drafter retries. The tool is not
// strict: the schema guides Claude but does not bind it, so callers still check.
type Anthropic struct {
	Key     string
	Model   string // e.g. claude-opus-5-5
	BaseURL string // tests only
}

func (a Anthropic) Name() string { return "anthropic/" + a.Model }

func (a Anthropic) Complete(ctx context.Context, system string, msgs []Message, schema json.RawMessage) (string, error) {
	opts := []option.RequestOption{option.WithAPIKey(a.Key)}
	if a.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(a.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	var top struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
		Defs       json.RawMessage `json:"$defs"`
		AnyOf      json.RawMessage `json:"anyOf"`
	}
	if err := json.Unmarshal(schema, &top); err != nil {
		return "", err
	}
	// A tool input is an object, so a union goes in one field and comes back out.
	union := top.AnyOf != nil
	if union {
		top.Properties, _ = json.Marshal(map[string]json.RawMessage{"answer": schema})
		top.Required = []string{"answer"}
	}
	tool := anthropic.ToolParam{
		Name:        "submit_spec",
		Description: anthropic.String("Submit the finished strategy spec."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: top.Properties, Required: top.Required,
			ExtraFields: map[string]any{"$defs": top.Defs},
		},
	}
	var history []anthropic.MessageParam
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == "assistant" {
			history = append(history, anthropic.NewAssistantMessage(block))
		} else {
			history = append(history, anthropic.NewUserMessage(block))
		}
	}
	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.Model),
		MaxTokens: 16000,
		System:    []anthropic.TextBlockParam{{Text: system + "\n\nSubmit the spec by calling the submit_spec tool exactly once."}},
		Messages:  history,
		Tools:     []anthropic.ToolUnionParam{{OfTool: &tool}},
	})
	if err != nil {
		return "", err
	}
	for _, block := range resp.Content {
		if v, ok := block.AsAny().(anthropic.ToolUseBlock); ok && v.Name == "submit_spec" {
			var in struct{ Answer json.RawMessage }
			if union && json.Unmarshal([]byte(v.JSON.Input.Raw()), &in) == nil && in.Answer != nil {
				return string(in.Answer), nil
			}
			return v.JSON.Input.Raw(), nil
		}
	}
	return "", ErrNoCall
}
