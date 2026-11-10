package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wickfleet/internal/registry"
)

type rec struct {
	got []string
	err error
}

func (r *rec) Send(_ context.Context, s string) error {
	r.got = append(r.got, s)
	return r.err
}

func TestWebhook(t *testing.T) {
	bots := func() []registry.Bot {
		return []registry.Bot{{Name: "ma", Owner: "alice"}, {Name: "a1", Managed: "zoo"}}
	}
	cases := []struct {
		name, method, path, body, xff string
		code                          int
		sent                          string
	}{
		{"ok", "POST", "/hub/webhook/ma", `{"text":"BTC/USDT entry"}`, "", 204, "[ma] BTC/USDT entry"},
		{"via traefik", "POST", "/hub/webhook/ma", `{"text":"x"}`, "1.2.3.4", 403, ""},
		{"unknown bot", "POST", "/hub/webhook/zz", `{"text":"x"}`, "", 404, ""},
		{"get", "GET", "/hub/webhook/ma", "", "", 405, ""},
		{"empty text", "POST", "/hub/webhook/ma", `{"text":""}`, "", 400, ""},
		{"not json", "POST", "/hub/webhook/ma", `text`, "", 400, ""},
		{"managed bot dropped", "POST", "/hub/webhook/a1", `{"text":"bought"}`, "", 204, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &rec{}
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			w := httptest.NewRecorder()
			Webhook{Bots: bots, Sender: s}.ServeHTTP(w, req)
			if w.Code != c.code {
				t.Fatalf("code %d want %d", w.Code, c.code)
			}
			if c.sent == "" && len(s.got) > 0 || c.sent != "" && (len(s.got) != 1 || s.got[0] != c.sent) {
				t.Fatalf("sent %q", s.got)
			}
		})
	}
	s := &rec{err: errors.New("down")}
	w := httptest.NewRecorder()
	Webhook{Bots: bots, Sender: s}.ServeHTTP(w, httptest.NewRequest("POST", "/hub/webhook/ma", strings.NewReader(`{"text":"x"}`)))
	if w.Code != 502 {
		t.Fatalf("send failure code %d", w.Code)
	}
}

func TestTelegram(t *testing.T) {
	var gotPath, gotChat, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotPath, gotChat, gotText = r.URL.Path, r.Form.Get("chat_id"), r.Form.Get("text")
		if gotChat == "bad" {
			http.Error(w, "x", 400)
		}
	}))
	defer srv.Close()
	tg := Telegram{Token: "123:abc", ChatID: "-5", API: srv.URL}
	if err := tg.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bot123:abc/sendMessage" || gotChat != "-5" || gotText != "hi" {
		t.Fatalf("%s %s %s", gotPath, gotChat, gotText)
	}
	tg.ChatID = "bad"
	if err := tg.Send(context.Background(), "hi"); err == nil {
		t.Fatal("want error on 400")
	}
	tg.API = "http://127.0.0.1:1"
	err := tg.Send(context.Background(), "hi")
	if err == nil || strings.Contains(err.Error(), "abc") {
		t.Fatalf("transport error must not carry the token: %v", err)
	}
}
