package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wickfleet/internal/authz"
	"wickfleet/internal/login"
	"wickfleet/internal/notify"
)

// builtinHub is a hub in WICKFLEET_AUTH=builtin with bot ma (alice) and lu
// (bob), and users alice (operator), bob (viewer) and root (admin).
func builtinHub(t *testing.T) (http.Handler, map[string]*http.Cookie) {
	t.Helper()
	old := authz.Groups
	authz.Groups = login.Groups
	t.Cleanup(func() { authz.Groups = old })
	reg, sec, state := t.TempDir(), t.TempDir(), t.TempDir()
	for name, owner := range map[string]string{"ma": "alice", "lu": "bob"} {
		os.WriteFile(filepath.Join(reg, name+".yaml"), []byte("name: "+name+"\nowner: "+owner+"\nstrategy: S\n"), 0o600)
		os.WriteFile(filepath.Join(sec, name+".api.password"), []byte("pw"), 0o600)
	}
	h := &hub{cfg: config{RegistryDir: reg, SecretsDir: sec, StateDir: state, CacheFor: time.Minute}}
	h.login = &login.Service{Users: filepath.Join(state, "users.json"), Setup: filepath.Join(state, "setup-token"),
		TTL: time.Hour, Log: func(string, ...any) {}}
	h.reload()
	srv := h.routes(notify.Discard{})
	cookies := map[string]*http.Cookie{}
	for user, role := range map[string]string{"alice": "operator", "bob": "viewer", "root": "admin"} {
		var out, errb bytes.Buffer
		if login.CLI(state, []string{"user", "add", user, role}, strings.NewReader("a long password\n"), &out, &errb) != 0 {
			t.Fatal(errb.String())
		}
		r := httptest.NewRequest("POST", "/hub/auth/login", strings.NewReader(`{"user":"`+user+`","password":"a long password"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Wickfleet-Request", "1")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		for _, c := range w.Result().Cookies() {
			cookies[user] = c
		}
		if w.Code != 200 || cookies[user] == nil {
			t.Fatalf("login %s: %d", user, w.Code)
		}
	}
	return srv, cookies
}

func TestBuiltinRoles(t *testing.T) {
	srv, cookies := builtinHub(t)
	authzCode := func(who, method, uri string, forge bool) int {
		r := httptest.NewRequest("GET", "/authz", nil)
		r.Header.Set("X-Forwarded-Method", method)
		r.Header.Set("X-Forwarded-Uri", uri)
		if c := cookies[who]; c != nil {
			r.AddCookie(c)
		}
		if forge {
			r.Header.Set("X-Auth-Request-Preferred-Username", "root")
			r.Header.Set("X-Auth-Request-Groups", "trade_admins,wickfleet-admin")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		who, method, uri string
		forge            bool
		want             int
	}{
		{"", "GET", "/bots/ma/api/v1/status", false, 403},
		{"", "GET", "/bots/ma/api/v1/status", true, 403}, // proxy headers count for nothing here
		{"bob", "GET", "/bots/lu/api/v1/status", false, 200},
		{"bob", "POST", "/bots/lu/api/v1/forcebuy", false, 403},
		{"bob", "GET", "/bots/ma/api/v1/status", false, 403},
		{"bob", "GET", "/bots/ma/api/v1/status", true, 403},
		{"alice", "POST", "/bots/ma/api/v1/forcebuy", false, 200},
		{"alice", "POST", "/bots/lu/api/v1/forcebuy", false, 403},
		{"root", "POST", "/bots/lu/api/v1/forcebuy", false, 200},
	} {
		if got := authzCode(c.who, c.method, c.uri, c.forge); got != c.want {
			t.Errorf("%s %s %s forged=%v: %d, want %d", c.who, c.method, c.uri, c.forge, got, c.want)
		}
	}

	api := func(who, method, path string, csrf bool) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		if csrf {
			r.Header.Set("X-Wickfleet-Request", "1")
		}
		if c := cookies[who]; c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	for who, want := range map[string]int{"": 403, "bob": 403, "alice": 403, "root": 200} {
		if got, _ := api(who, "GET", "/hub/api/reconcile", false); got != want {
			t.Errorf("reconcile as %q: %d, want %d", who, got, want)
		}
	}
	if code, body := api("alice", "DELETE", "/hub/api/bots/ma", false); code != 403 || !strings.Contains(body, "missing X-Wickfleet-Request") {
		t.Errorf("write without the CSRF header: %d %s", code, body)
	}
	if _, body := api("alice", "DELETE", "/hub/api/bots/ma", true); strings.Contains(body, "missing X-Wickfleet-Request") {
		t.Errorf("write with the header still refused for it: %s", body)
	}
}

func TestProxyModeUnchanged(t *testing.T) {
	h := &hub{cfg: config{RegistryDir: t.TempDir(), StateDir: t.TempDir()}}
	h.reload()
	srv := h.routes(notify.Discard{})
	r := httptest.NewRequest("GET", "/hub/api/me", nil)
	r.Header.Set("X-Auth-Request-Preferred-Username", "alice")
	r.Header.Set("X-Auth-Request-Groups", "trade_admins")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"admin"`) {
		t.Fatalf("proxy headers: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest("GET", "/hub/login", nil))
	if w.Code != 404 {
		t.Fatalf("login page in proxy mode: %d", w.Code)
	}
}
