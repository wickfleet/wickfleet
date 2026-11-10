package authz

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"wickfleet/internal/registry"
)

const dom = "@idm.example.org"

// Kanidm's sub is a UUID; owners and commit authors use the username.
func TestPreferredUsernameWins(t *testing.T) {
	h := http.Header{}
	h.Set("X-Auth-Request-User", "00000000-0000-4000-8000-000000000001")
	h.Set("X-Auth-Request-Preferred-Username", "alice@idm.example.org")
	h.Set("X-Auth-Request-Groups", "trade_operators"+dom)
	if id := FromHeaders(h); id.User != "alice" || id.Role != Operator {
		t.Fatalf("%+v", id)
	}
	h.Del("X-Auth-Request-Preferred-Username")
	if id := FromHeaders(h); id.User != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("without it, fall back to the user header: %+v", id)
	}
}

func TestFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Auth-Request-User", "alice"+dom)
	h.Set("X-Auth-Request-Groups", "idm_all_persons"+dom+",trade_viewers"+dom+",trade_operators"+dom)
	id := FromHeaders(h)
	if id.User != "alice" || id.Role != Operator {
		t.Fatalf("got %+v", id)
	}
	h.Del("X-Auth-Request-User")
	if FromHeaders(h).Role != None {
		t.Fatal("groups without a user must not grant a role")
	}
	h.Set("X-Auth-Request-User", "x")
	h.Set("X-Auth-Request-Groups", "trade_admins_evil"+dom+",xtrade_admins")
	if FromHeaders(h).Role != None {
		t.Fatal("look-alike group names must not grant a role")
	}
}

func TestParseGroups(t *testing.T) {
	g, err := ParseGroups(" game_players=viewer, game_admins" + dom + "=admin,")
	if err != nil || len(g) != 2 || g["game_players"] != Viewer || g["game_admins"] != Admin {
		t.Fatalf("%v %v", g, err)
	}
	for _, bad := range []string{"", " , ", "x=root", "=admin", "x", "x=viewer,x=admin"} {
		if _, err := ParseGroups(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	old := Groups
	defer func() { Groups = old }()
	Groups = g
	h := http.Header{}
	h.Set("X-Auth-Request-User", "alice"+dom)
	h.Set("X-Auth-Request-Groups", "trade_admins"+dom+",game_players"+dom)
	if id := FromHeaders(h); id.Role != Viewer {
		t.Fatalf("configured groups replace the defaults: %+v", id)
	}
}

func TestAllow(t *testing.T) {
	own := registry.Bot{Name: "ma", Owner: "alice"}
	cases := []struct {
		user   string
		role   Role
		method string
		path   string
		want   bool
	}{
		{"bob", Admin, "DELETE", "/api/v1/trades/1", true},
		{"alice", Viewer, "GET", "/api/v1/status", true},
		{"alice", Viewer, "POST", "/api/v1/token/login", true},
		{"alice", Viewer, "POST", "/api/v1/token/refresh", true},
		{"alice", Viewer, "POST", "/api/v1/forceexit", false},
		{"alice", Viewer, "DELETE", "/api/v1/trades/1", false},
		{"alice", Operator, "POST", "/api/v1/stop", true},
		{"bob", Viewer, "GET", "/api/v1/status", false},
		{"bob", Operator, "POST", "/api/v1/stop", false},
		{"alice", None, "GET", "/api/v1/status", false},
	}
	for _, c := range cases {
		got := Allow(Identity{User: c.user, Role: c.role}, own, c.method, c.path)
		if got != c.want {
			t.Errorf("%s %v %s %s: got %v want %v", c.user, c.role, c.method, c.path, got, c.want)
		}
	}
	// Managed bots: the owner gets nothing, an admin reads, nobody writes.
	agent := registry.Bot{Name: "rsi", Owner: "alice", Managed: "daniel"}
	for _, c := range []struct {
		user   string
		role   Role
		method string
		path   string
		want   bool
	}{
		{"alice", Operator, "GET", "/api/v1/status", false},
		{"alice", Viewer, "POST", "/api/v1/token/login", false},
		{"bob", Admin, "GET", "/api/v1/status", true},
		{"bob", Admin, "POST", "/api/v1/token/login", true},
		{"bob", Admin, "POST", "/api/v1/forceexit", false},
		{"bob", Admin, "DELETE", "/api/v1/trades/1", false},
	} {
		if got := Allow(Identity{User: c.user, Role: c.role}, agent, c.method, c.path); got != c.want {
			t.Errorf("managed bot, %s %v %s %s: got %v want %v", c.user, c.role, c.method, c.path, got, c.want)
		}
	}
}

func TestSplitBotPath(t *testing.T) {
	cases := map[string][3]string{
		"/bots/ma/api/v1/ping": {"ma", "/api/v1/ping", "ok"},
		"/bots/ma":             {"ma", "/", "ok"},
		"/bots/../etc":         {"", "", ""},
		"/hub/api/fleet":       {"", "", ""},
		"/botsx/ma/":           {"", "", ""},
	}
	for in, want := range cases {
		n, r, ok := SplitBotPath(in)
		if (want[2] == "ok") != ok || n != want[0] || r != want[1] {
			t.Errorf("%s: got %q %q %v", in, n, r, ok)
		}
	}
}

func newReq(user, groups, method, uri, auth string) *http.Request {
	r := httptest.NewRequest("GET", "/authz", nil)
	if user != "" {
		r.Header.Set("X-Auth-Request-User", user+dom)
	}
	r.Header.Set("X-Auth-Request-Groups", groups)
	r.Header.Set("X-Forwarded-Method", method)
	r.Header.Set("X-Forwarded-Uri", uri)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestHandler(t *testing.T) {
	h := Handler{
		Bots: func() []registry.Bot {
			return []registry.Bot{{Name: "ma", Owner: "alice"}, {Name: "dead", Owner: "alice"}}
		},
		Secret: func(b string) (string, error) {
			if b == "dead" {
				return "", errors.New("gone")
			}
			return "pw", nil
		},
	}
	cases := []struct {
		name, user, groups, method, uri, auth string
		code                                  int
		basic                                 bool
	}{
		{"no role", "alice", "idm_all_persons" + dom, "GET", "/", "", 403, false},
		{"ui page", "alice", "trade_viewers" + dom, "GET", "/", "", 200, false},
		{"bot read gets key", "alice", "trade_viewers" + dom, "GET", "/bots/ma/api/v1/status", "", 200, true},
		{"client basic replaced", "alice", "trade_viewers" + dom, "GET", "/bots/ma/api/v1/status", "Basic eDp4", 200, true},
		{"bearer kept", "alice", "trade_viewers" + dom, "GET", "/bots/ma/api/v1/status", "Bearer jwt", 200, false},
		{"empty bearer gets key", "alice", "trade_viewers" + dom, "GET", "/bots/ma/api/v1/status", "Bearer ", 200, true},
		{"viewer write", "alice", "trade_viewers" + dom, "POST", "/bots/ma/api/v1/forceexit", "", 403, false},
		{"other owner", "bob", "trade_operators" + dom, "GET", "/bots/ma/api/v1/status", "", 403, false},
		{"unknown bot", "alice", "trade_admins" + dom, "GET", "/bots/zz/api/v1/status", "", 404, false},
		{"key missing", "alice", "trade_admins" + dom, "GET", "/bots/dead/api/v1/status", "", 502, false},
		{"bad uri", "alice", "trade_admins" + dom, "GET", "::", "", 400, false},
		{"query kept apart", "alice", "trade_viewers" + dom, "GET", "/bots/ma/api/v1/message/ws?token=t", "", 200, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newReq(c.user, c.groups, c.method, c.uri, c.auth))
			if rec.Code != c.code {
				t.Fatalf("code %d want %d (%s)", rec.Code, c.code, rec.Body.String())
			}
			got := rec.Header().Get("Authorization")
			switch {
			case c.basic && got != "Basic bWE6cHc=":
				t.Fatalf("authorization %q", got)
			case !c.basic && c.code == 200 && c.auth != "" && got != c.auth:
				// Traefik deletes the request header when the response lacks it.
				t.Fatalf("bearer must be echoed, got %q", got)
			case !c.basic && (c.code != 200 || c.auth == "") && got != "":
				t.Fatalf("unexpected authorization %q", got)
			}
		})
	}
}

func TestRequireKey(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	cases := []struct {
		key, sent string
		code      int
	}{
		{"", "", 204},
		{"s3cret", "s3cret", 204},
		{"s3cret", "", 401},
		{"s3cret", "s3cre", 401},
		{"s3cret", "s3cretX", 401},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/authz", nil)
		if c.sent != "" {
			r.Header.Set(KeyHeader, c.sent)
		}
		w := httptest.NewRecorder()
		RequireKey(c.key, ok).ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("key %q sent %q: %d want %d", c.key, c.sent, w.Code, c.code)
		}
	}
}

func TestOpenPrefixes(t *testing.T) {
	api := http.NewServeMux()
	for _, p := range []string{"GET /hub/api/game", "/hub/api/game/{team}/{act}", "GET /hub/api/game/jobs/{id}",
		"/hub/api/me", "GET /hub/api/bots", "GET /hub/api/specs/{name}", "/hub/api/reconcile",
		"GET /hub/api/docs", "/hub/api/docs/{page}/try"} {
		api.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	}
	cases := []struct {
		path, groups string
		code         int
	}{
		{"/hub/api/game", "trade_viewers", 204},
		{"/hub/api/game/red/pin", "trade_viewers", 204},
		{"/hub/api/game/jobs/a1", "", 204}, // the handler refuses no role itself
		{"/hub/api/me", "", 204},
		{"/hub/api/bots", "trade_operators", 403},
		{"/hub/api/specs/RsiDip", "trade_viewers", 403},
		{"/hub/api/gamex", "trade_viewers", 403},
		{"/hub/api/game/../bots", "trade_viewers", 403},
		{"/hub/api/specs/..%2Fgame", "trade_viewers", 403},
		{"/hub/api/%67ame", "trade_viewers", 204},
		{"/hub/api/docs", "trade_viewers", 204},
		{"/hub/api/docs/rsi/try", "trade_viewers", 204},
		{"/hub/api/docsx", "trade_viewers", 403},
		{"/hub/api/docs/../bots", "trade_viewers", 403},
		{"/hub/api/specs/..%2Fdocs", "trade_viewers", 403},
		{"/hub/api/bots", "trade_admins", 204},
		{"/hub/api/reconcile", "trade_admins", 204},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Header.Set("X-Auth-Request-User", "alice"+dom)
		r.Header.Set("X-Auth-Request-Groups", c.groups+dom)
		w := httptest.NewRecorder()
		OpenPrefixes(api, "/hub/api/game", "/hub/api/docs").ServeHTTP(w, r)
		if w.Code != c.code {
			t.Errorf("%s as %s: %d want %d", c.path, c.groups, w.Code, c.code)
		}
	}
}
