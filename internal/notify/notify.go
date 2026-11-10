// Package notify forwards bot events to one Telegram chat. Bots post their
// freqtrade webhooks to the hub; one Telegram bot serves the whole fleet.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wickfleet/internal/registry"
)

// Sender delivers one text message.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Telegram sends through the Bot API.
type Telegram struct {
	Token  string
	ChatID string
	API    string // default https://api.telegram.org
	HTTP   *http.Client
}

func (t Telegram) Send(ctx context.Context, text string) error {
	api := t.API
	if api == "" {
		api = "https://api.telegram.org"
	}
	form := url.Values{"chat_id": {t.ChatID}, "text": {text}, "disable_web_page_preview": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/bot"+t.Token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := t.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// The URL holds the token; never let it reach a log.
		return fmt.Errorf("telegram send failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Webhook takes freqtrade webhook posts at /hub/webhook/<bot>. Only bots on
// the internal network may post: anything that came through Traefik carries
// X-Forwarded-For and is refused. Managed bots are dropped quietly.
type Webhook struct {
	Bots   func() []registry.Bot
	Sender Sender
	Log    func(format string, args ...any)
}

func (h Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Forwarded-For") != "" {
		http.Error(w, "internal only", http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/hub/webhook/")
	bot, ok := registry.Find(h.Bots(), name)
	if !ok {
		http.Error(w, "no such bot", http.StatusNotFound)
		return
	}
	if bot.Managed != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil || body.Text == "" {
		http.Error(w, "want JSON {\"text\": ...}", http.StatusBadRequest)
		return
	}
	if err := h.Sender.Send(r.Context(), "["+name+"] "+body.Text); err != nil {
		if h.Log != nil {
			h.Log("notify %s: %v", name, err)
		}
		http.Error(w, "send failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Discard drops messages; used when Telegram is not configured.
type Discard struct{}

func (Discard) Send(context.Context, string) error { return nil }
