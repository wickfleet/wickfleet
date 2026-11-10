package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wickfleet/internal/authz"
	"wickfleet/internal/demo"
	"wickfleet/internal/notify"
)

// demoHub is a hub with bot ma owned by the demo user and lu owned by dan,
// in the given auth mode.
func demoHub(t *testing.T, auth string) *hub {
	t.Helper()
	if auth == "demo" {
		old := authz.Groups
		authz.Groups = demo.Groups
		t.Cleanup(func() { authz.Groups = old })
	}
	reg, sec := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(reg, "bots"), 0o755)
	for name, owner := range map[string]string{"ma": demo.User, "lu": "dan"} {
		os.WriteFile(filepath.Join(reg, "bots", name+".yaml"), []byte("name: "+name+"\nowner: "+owner+"\nstrategy: S\n"), 0o600)
		os.WriteFile(filepath.Join(sec, name+".api.password"), []byte("pw"), 0o600)
	}
	h := &hub{cfg: config{Auth: auth, RegistryDir: reg, SecretsDir: sec, StateDir: t.TempDir(), BotURL: "http://127.0.0.1:1/%s"}}
	if err := h.reload(); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestDemoRefusesEveryWrite sends every /hub/api route as every person, forged
// admin headers included. Demo refuses each write; proxy mode is the control.
func TestDemoRefusesEveryWrite(t *testing.T) {
	vars := map[string]string{"name": "ma", "id": "x1"}
	ctl := matrix(t, demoHub(t, "proxy"), vars, nil) // before demo swaps authz.Groups
	got := matrix(t, demoHub(t, "demo"), vars, nil)
	writes := 0
	for p, codes := range got {
		method, _, ok := strings.Cut(p, " ")
		if !ok {
			method = "GET"
		}
		for i, c := range codes {
			if c != codes[0] {
				t.Errorf("%s: person %d got %d, want the same answer for everyone (%d)", p, i, c, codes[0])
			}
		}
		if method == "GET" {
			continue
		}
		if c := ctl[p]; c[4] == http.StatusForbidden {
			t.Errorf("control: proxy-mode admin %s got 403, so the demo 403 proves nothing", p)
		}
		writes++
		if codes[0] != http.StatusForbidden {
			t.Errorf("%s: demo answered %d, want 403", p, codes[0])
		}
	}
	if writes == 0 || writes != len(got)-reads(got) {
		t.Fatalf("%d write routes checked, the matrix did not run", writes)
	}
}

// reads counts the GET routes, bare patterns included.
func reads(m map[string][5]int) int {
	n := 0
	for p := range m {
		if method, _, ok := strings.Cut(p, " "); !ok || method == "GET" {
			n++
		}
	}
	return n
}

func TestDemoReads(t *testing.T) {
	srv := demoHub(t, "demo").routes(notify.Discard{})
	get := func(path string, forge bool) (int, string) {
		r := httptest.NewRequest("GET", path, nil)
		if forge {
			r.Header.Set("X-Auth-Request-User", "root")
			r.Header.Set("X-Auth-Request-Groups", "trade_admins")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if code, body := get("/hub/api/me", true); code != 200 || body != `{"role":"viewer","user":"demo","version":"dev"}` {
		t.Fatalf("/me with forged admin headers: %d %s", code, body)
	}
	code, body := get("/hub/api/bots", false)
	if code != 200 || !strings.Contains(body, `"ma"`) || strings.Contains(body, `"lu"`) {
		t.Fatalf("bots: %d %s, want ma only", code, body)
	}
	if code, _ := get("/hub/api/reconcile", true); code != http.StatusForbidden {
		t.Fatalf("reconcile is admin only, forged admin got %d", code)
	}
	if code, _ := get("/hub/login", false); code != http.StatusNotFound {
		t.Fatalf("demo has no login page: %d", code)
	}
}

// TestDemoBotPolicy is the ForwardAuth answer for bot API calls: reads and the
// FreqUI token login pass on the demo bot, every write is refused.
func TestDemoBotPolicy(t *testing.T) {
	authzCode := func(srv http.Handler, user, groups, method, uri string) int {
		r := httptest.NewRequest("GET", "/authz", nil)
		r.Header.Set("X-Forwarded-Method", method)
		r.Header.Set("X-Forwarded-Uri", uri)
		if user != "" {
			r.Header.Set("X-Auth-Request-User", user)
			r.Header.Set("X-Auth-Request-Groups", groups)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code
	}
	ctl := demoHub(t, "proxy").routes(notify.Discard{})
	if got := authzCode(ctl, "root", "trade_admins", "POST", "/bots/ma/api/v1/forceenter"); got != 200 {
		t.Fatalf("control: proxy-mode admin forceenter got %d, want 200", got)
	}
	srv := demoHub(t, "demo").routes(notify.Discard{}) // swaps authz.Groups, so after the control
	for _, c := range []struct {
		user, groups, method, uri string
		want                      int
	}{
		{"", "", "GET", "/bots/ma/api/v1/status", 200},
		{"", "", "POST", "/bots/ma/api/v1/token/login", 200},
		{"", "", "POST", "/bots/ma/api/v1/forceenter", 403},
		{"", "", "POST", "/bots/ma/api/v1/stop", 403},
		{"", "", "DELETE", "/bots/ma/api/v1/trades/1", 403},
		{"root", "trade_admins", "POST", "/bots/ma/api/v1/forceenter", 403},
		{"", "", "GET", "/bots/lu/api/v1/status", 403},
	} {
		if got := authzCode(srv, c.user, c.groups, c.method, c.uri); got != c.want {
			t.Errorf("demo %s %s as %q: %d, want %d", c.method, c.uri, c.user, got, c.want)
		}
	}
}

// TestDemoExampleRegistry loads examples/registry with examples/demo/bots in
// place of its bots/, as a demo instance runs it: visitors see both bots.
func TestDemoExampleRegistry(t *testing.T) {
	reg := t.TempDir()
	copyFile := func(src, dst string) {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copyFile("../../examples/registry/defaults.yaml", filepath.Join(reg, "defaults.yaml"))
	for _, s := range []string{"EmaCross.json", "EmaCross.py", "RsiReversion.json", "RsiReversion.py"} {
		copyFile("../../examples/registry/strategies/"+s, filepath.Join(reg, "strategies", s))
	}
	bots, _ := filepath.Glob("../../examples/demo/bots/*.yaml")
	if len(bots) != 2 {
		t.Fatalf("examples/demo/bots: %d files, want 2", len(bots))
	}
	for _, b := range bots {
		copyFile(b, filepath.Join(reg, "bots", filepath.Base(b)))
	}
	old := authz.Groups
	authz.Groups = demo.Groups
	t.Cleanup(func() { authz.Groups = old })
	h := &hub{cfg: config{Auth: "demo", RegistryDir: reg, SecretsDir: t.TempDir(), StateDir: t.TempDir()}}
	if err := h.reload(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.routes(notify.Discard{}).ServeHTTP(w, httptest.NewRequest("GET", "/hub/api/bots", nil))
	for _, name := range []string{`"ema-cross"`, `"rsi-reversion"`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), name) {
			t.Fatalf("bots: %d, %s missing from %s", w.Code, name, w.Body.String())
		}
	}
}
