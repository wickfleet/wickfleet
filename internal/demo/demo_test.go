package demo

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWrap holds on its own: the inner handler accepts anything, so a write
// is refused here or nowhere.
func TestWrap(t *testing.T) {
	var seen http.Header
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
	for method, want := range map[string]int{"GET": 200, "HEAD": 200, "OPTIONS": 200,
		"POST": 403, "PUT": 403, "PATCH": 403, "DELETE": 403} {
		seen = nil
		r := httptest.NewRequest(method, "/hub/api/bots/ma", nil)
		r.Header.Set("X-Auth-Request-User", "root")
		r.Header.Set("X-Auth-Request-Groups", "trade_admins")
		r.Header.Set("X-Auth-Request-Email", "root@example.org")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: %d, want %d", method, w.Code, want)
		}
		if want == 403 {
			if seen != nil {
				t.Errorf("%s reached the inner handler", method)
			}
			continue
		}
		if seen.Get("X-Auth-Request-User") != "" || seen.Get("X-Auth-Request-Email") != "" ||
			seen.Get("X-Auth-Request-Preferred-Username") != User || seen.Get("X-Auth-Request-Groups") != group {
			t.Errorf("%s: identity not replaced: %v", method, seen)
		}
	}
}
