package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"wickfleet/internal/notify"
)

// recorder keeps every pattern registered on the /hub/api mux.
type recorder struct {
	*http.ServeMux
	patterns []string
}

func (r *recorder) HandleFunc(p string, f func(http.ResponseWriter, *http.Request)) {
	r.patterns = append(r.patterns, p)
	r.ServeMux.HandleFunc(p, f)
}

// people are the matrix columns: no identity, a viewer without a zoo, two zoo
// owners, an admin without a zoo.
var people = []struct{ user, groups string }{
	{"", ""}, {"carol", "trade_viewers"}, {"dan", "trade_viewers"}, {"lin", "trade_viewers"}, {"root", "trade_admins"},
}

// matrix sends one request per registered route and person; vars fill the
// path wildcards, bodies override the default "{}".
func matrix(t *testing.T, h *hub, vars, bodies map[string]string) map[string][5]int {
	t.Helper()
	rec := &recorder{ServeMux: http.NewServeMux()}
	h.apiRoutes(rec)
	srv := h.routes(notify.Discard{})
	out := map[string][5]int{}
	for _, p := range rec.patterns {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			method, path = "GET", p
		}
		for k, v := range vars {
			path = strings.ReplaceAll(path, "{"+k+"}", v)
		}
		body, ok := bodies[p]
		if !ok {
			body = "{}"
		}
		var codes [5]int
		for i, who := range people {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Wickfleet-Request", "1")
			if who.user != "" {
				r.Header.Set("X-Auth-Request-User", who.user+"@idm.example.org")
				r.Header.Set("X-Auth-Request-Groups", who.groups+"@idm.example.org")
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			codes[i] = w.Code
		}
		out[p] = codes
	}
	return out
}

// sameMatrix fails on any route without a row, any row without a route, and
// any status that moved; it prints the table it got for review.
func sameMatrix(t *testing.T, got, want map[string][5]int) {
	t.Helper()
	bad := false
	for p, codes := range got {
		if w, ok := want[p]; !ok || w != codes {
			t.Errorf("%s: got %v, want %v (row present: %v)", p, codes, w, ok)
			bad = true
		}
	}
	for p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("%s: in the table but not registered", p)
			bad = true
		}
	}
	if bad {
		keys := make([]string, 0, len(got))
		for p := range got {
			keys = append(keys, p)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, p := range keys {
			c := got[p]
			fmt.Fprintf(&b, "\t%q: {%d, %d, %d, %d, %d},\n", p, c[0], c[1], c[2], c[3], c[4])
		}
		t.Logf("got:\n%s", b.String())
	}
}

// tradeRoutes is every /hub/api route on a trade instance (no extension), with
// its status for each of people.
var tradeRoutes = map[string][5]int{
	"/hub/api/fleet":                {403, 200, 200, 200, 200},
	"/hub/api/me":                   {200, 200, 200, 200, 200},
	"/hub/api/reconcile":            {403, 403, 403, 403, 200},
	"/hub/api/signals":              {403, 200, 200, 200, 200},
	"/hub/api/watch":                {403, 200, 200, 200, 200},
	"DELETE /hub/api/bots/{name}":   {403, 403, 403, 403, 503},
	"DELETE /hub/api/specs/{name}":  {403, 403, 403, 403, 503},
	"GET /hub/api/bots":             {403, 200, 200, 200, 200},
	"GET /hub/api/replays":          {403, 200, 200, 200, 200},
	"GET /hub/api/replays/{id}":     {404, 404, 404, 404, 404},
	"GET /hub/api/specs":            {403, 200, 200, 200, 200},
	"GET /hub/api/specs/draft/{id}": {404, 404, 404, 404, 404},
	"GET /hub/api/specs/{name}":     {403, 404, 404, 404, 404},
	"GET /hub/api/strategies":       {403, 200, 200, 200, 200},
	"GET /hub/api/watchlist":        {403, 200, 200, 200, 200},
	"POST /hub/api/replays":         {403, 403, 403, 403, 503},
	"POST /hub/api/specs/check":     {403, 403, 403, 403, 400},
	"POST /hub/api/specs/draft":     {403, 403, 403, 403, 503},
	"PUT /hub/api/bots/{name}":      {403, 403, 403, 403, 503},
	"PUT /hub/api/specs/{name}":     {403, 403, 403, 403, 503},
	"PUT /hub/api/watchlist":        {403, 403, 403, 403, 503},
}

func TestTradeRoleMatrix(t *testing.T) {
	reg := t.TempDir()
	os.MkdirAll(filepath.Join(reg, "bots"), 0o755)
	os.WriteFile(filepath.Join(reg, "bots", "ma.yaml"), []byte("name: ma\nowner: dan\nstrategy: S\n"), 0o600)
	h := &hub{cfg: config{RegistryDir: reg, SecretsDir: t.TempDir(), StateDir: t.TempDir(), BotURL: "http://127.0.0.1:1/%s"}}
	if err := h.reload(); err != nil {
		t.Fatal(err)
	}
	sameMatrix(t, matrix(t, h, map[string]string{"name": "ma", "id": "x1"}, nil), tradeRoutes)
}

func me(t *testing.T, h *hub, user, groups string) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/hub/api/me", nil)
	r.Header.Set("X-Auth-Request-User", user+"@idm.example.org")
	r.Header.Set("X-Auth-Request-Groups", groups+"@idm.example.org")
	w := httptest.NewRecorder()
	h.routes(notify.Discard{}).ServeHTTP(w, r)
	return strings.TrimSpace(w.Body.String())
}

func TestMe(t *testing.T) {
	trade := &hub{cfg: config{RegistryDir: t.TempDir()}}
	if got := me(t, trade, "carol", "trade_viewers"); got != `{"role":"viewer","user":"carol","version":"dev"}` {
		t.Fatalf("trade /me must not change: %s", got)
	}
}
