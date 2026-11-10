package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"

	"wickfleet/internal/authz"
)

// Guard refuses in order: CSRF header, role, content type, body.
func TestGuard(t *testing.T) {
	for _, c := range []struct {
		name, csrf, group, ctype, body string
		code                           int
		want                           string
	}{
		{"no csrf", "", "trade_viewers", "application/json", "{}", 403, "missing X-Wickfleet-Request"},
		{"role", "1", "", "text/plain", "{}", 403, "operators only"},
		{"type", "1", "trade_operators", "text/plain", "{}", 415, "JSON only"},
		{"unknown field", "1", "trade_operators", "application/json", `{"x":1}`, 400, "bad body"},
		{"ok", "1", "trade_admins", "application/json", `{"a":"b"}`, 200, ""},
	} {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(c.body))
		r.Header.Set(CSRFHeader, c.csrf)
		r.Header.Set("Content-Type", c.ctype)
		r.Header.Set("X-Auth-Request-User", "alice")
		r.Header.Set("X-Auth-Request-Groups", c.group)
		w := httptest.NewRecorder()
		var in struct{ A string }
		_, ok := Guard(w, r, authz.Operator, "operators only", &in)
		if c.code == 200 {
			if !ok || in.A != "b" {
				t.Errorf("%s: refused %d %s", c.name, w.Code, w.Body)
			}
			continue
		}
		if ok || w.Code != c.code || !strings.Contains(w.Body.String(), c.want) || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %v %d %s", c.name, ok, w.Code, w.Body)
		}
	}
}
