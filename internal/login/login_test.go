package login

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wickfleet/internal/authz"
)

type fixture struct {
	t   *testing.T
	s   *Service
	dir string
	now time.Time
	srv http.Handler
}

// newFixture serves the login routes, and on /whoami the identity the hub
// would see behind Wrap, as "user role".
func newFixture(t *testing.T) *fixture {
	t.Helper()
	old := authz.Groups
	authz.Groups = Groups
	t.Cleanup(func() { authz.Groups = old })
	f := &fixture{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	f.s = &Service{Users: filepath.Join(f.dir, "users.json"), Setup: filepath.Join(f.dir, "setup-token"),
		TTL: 12 * time.Hour, Now: func() time.Time { return f.now }, Log: func(string, ...any) {}}
	mux := http.NewServeMux()
	f.s.Routes(mux)
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		id := authz.FromHeaders(r.Header)
		fmt.Fprintf(w, "%s %s", id.User, id.Role)
	})
	f.srv = f.s.Wrap(mux)
	return f
}

func (f *fixture) addUser(name, role, pw string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	if rc := CLI(f.dir, []string{"user", "add", name, role}, strings.NewReader(pw+"\n"), &out, &errb); rc != 0 {
		f.t.Fatalf("user add %s: %d %s", name, rc, errb.String())
	}
}

type reply struct {
	code   int
	body   string
	cookie *http.Cookie
	header http.Header
}

func (f *fixture) do(method, path, body string, edit func(*http.Request)) reply {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:5000"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Wickfleet-Request", "1")
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	out := reply{code: w.Code, body: w.Body.String(), header: w.Header()}
	for _, c := range w.Result().Cookies() {
		if c.Name == Cookie {
			out.cookie = c
		}
	}
	return out
}

func (f *fixture) login(user, pw string) reply {
	f.t.Helper()
	b, _ := json.Marshal(map[string]string{"user": user, "password": pw})
	return f.do("POST", "/hub/auth/login", string(b), nil)
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

func (f *fixture) whoami(edit func(*http.Request)) string {
	f.t.Helper()
	return f.do("GET", "/whoami", "", edit).body
}

const pw = "correct horse battery"

func TestNoDefaultAdmin(t *testing.T) {
	f := newFixture(t)
	for _, try := range [][2]string{{"admin", "admin"}, {"admin", ""}, {"admin", "changeme"}, {"root", "root"}, {"", ""}} {
		if r := f.login(try[0], try[1]); r.code != 401 || r.cookie != nil {
			t.Fatalf("login %q/%q with no users: %d", try[0], try[1], r.code)
		}
		f.s.fails = nil // keep the rate limit out of this
	}
	if r := f.do("GET", "/hub/auth/state", "", nil); !strings.Contains(r.body, `"setup":true`) {
		t.Fatalf("state %s", r.body)
	}
	setup := `{"token":"","user":"admin","password":"` + pw + `"}`
	if r := f.do("POST", "/hub/auth/setup", setup, nil); r.code != 403 || !strings.Contains(r.body, "no setup token") {
		t.Fatalf("setup without a token file: %d %s", r.code, r.body)
	}
	if _, err := os.Stat(f.s.Users); !os.IsNotExist(err) {
		t.Fatalf("a refused setup wrote users.json: %v", err)
	}
	// Negative control: the same setup with a real token creates the admin.
	tok, err := SetupToken(f.s.Setup)
	if err != nil {
		t.Fatal(err)
	}
	r := f.do("POST", "/hub/auth/setup", `{"token":"`+tok+`","user":"admin","password":"`+pw+`"}`, nil)
	if r.code != 200 || r.cookie == nil {
		t.Fatalf("setup with the token: %d %s", r.code, r.body)
	}
	if got := f.whoami(withCookie(r.cookie)); got != "admin admin" {
		t.Fatalf("after setup %q", got)
	}
}

func TestSetupTokenOnceAndExpiring(t *testing.T) {
	f := newFixture(t)
	tok, _ := SetupToken(f.s.Setup)
	body := func(tok, user string) string {
		return `{"token":"` + tok + `","user":"` + user + `","password":"` + pw + `"}`
	}
	if r := f.do("POST", "/hub/auth/setup", body("0000", "admin"), nil); r.code != 403 {
		t.Fatalf("wrong token: %d", r.code)
	}
	if r := f.do("POST", "/hub/auth/setup", body(tok, "admin"), nil); r.code != 200 {
		t.Fatalf("right token: %d %s", r.code, r.body)
	}
	if _, err := os.Stat(f.s.Setup); !os.IsNotExist(err) {
		t.Fatal("the token file outlived its use")
	}
	if r := f.do("POST", "/hub/auth/setup", body(tok, "eve"), nil); r.code != 409 {
		t.Fatalf("second setup: %d", r.code)
	}
	var out, errb bytes.Buffer
	if rc := CLI(f.dir, []string{"setup-token"}, nil, &out, &errb); rc == 0 {
		t.Fatal("setup-token with users present")
	}

	g := newFixture(t)
	tok, _ = SetupToken(g.s.Setup)
	g.now = g.now.Add(2 * SetupMaxAge)
	os.Chtimes(g.s.Setup, g.now.Add(-2*SetupMaxAge), g.now.Add(-2*SetupMaxAge))
	if r := g.do("POST", "/hub/auth/setup", body(tok, "admin"), nil); r.code != 403 || !strings.Contains(r.body, "expired") {
		t.Fatalf("expired token: %d %s", r.code, r.body)
	}
}

func TestClientHeadersIgnored(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "viewer", pw)
	forged := func(r *http.Request) {
		r.Header.Set("X-Auth-Request-Preferred-Username", "mallory")
		r.Header.Set("X-Auth-Request-User", "mallory")
		r.Header.Set("X-Auth-Request-Groups", "wickfleet-admin,trade_admins")
		r.Header.Set("x-auth-request-email", "mallory@example.org")
	}
	if got := f.whoami(forged); got != " none" {
		t.Fatalf("forged headers without a session gave %q", got)
	}
	r := f.login("alice", pw)
	if got := f.whoami(func(r2 *http.Request) { forged(r2); r2.AddCookie(r.cookie) }); got != "alice viewer" {
		t.Fatalf("forged headers beside a session gave %q", got)
	}
	// Negative control: the plain session gives the same identity.
	if got := f.whoami(withCookie(r.cookie)); got != "alice viewer" {
		t.Fatalf("session gave %q", got)
	}
}

func TestCookieFlagsAndSessionExpiry(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "operator", pw)
	r := f.login("alice", pw)
	c := r.cookie
	if r.code != 200 || c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("login %d, cookie %+v", r.code, c)
	}
	f.now = f.now.Add(f.s.TTL - time.Second)
	if got := f.whoami(withCookie(c)); got != "alice operator" {
		t.Fatalf("before expiry %q", got)
	}
	f.now = f.now.Add(time.Second)
	if got := f.whoami(withCookie(c)); got != " none" {
		t.Fatalf("at expiry %q", got)
	}
	bogus := &http.Cookie{Name: Cookie, Value: strings.Repeat("ab", 32)}
	if got := f.whoami(withCookie(bogus)); got != " none" {
		t.Fatalf("made-up cookie %q", got)
	}
}

func TestLogoutNeedsCSRFAndEndsTheSession(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "viewer", pw)
	c := f.login("alice", pw).cookie
	noCSRF := func(r *http.Request) { r.Header.Del("X-Wickfleet-Request"); r.AddCookie(c) }
	if r := f.do("POST", "/hub/auth/logout", "{}", noCSRF); r.code != 403 {
		t.Fatalf("logout without the header: %d", r.code)
	}
	if got := f.whoami(withCookie(c)); got != "alice viewer" {
		t.Fatalf("a refused logout ended the session: %q", got)
	}
	if r := f.do("POST", "/hub/auth/logout", "{}", withCookie(c)); r.code != 200 || r.cookie == nil || r.cookie.MaxAge >= 0 {
		t.Fatalf("logout %d %+v", r.code, r.cookie)
	}
	if got := f.whoami(withCookie(c)); got != " none" {
		t.Fatalf("after logout %q", got)
	}
}

func TestLoginNeedsCSRF(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "viewer", pw)
	r := f.do("POST", "/hub/auth/login", `{"user":"alice","password":"`+pw+`"}`,
		func(r *http.Request) { r.Header.Del("X-Wickfleet-Request") })
	if r.code != 403 || r.cookie != nil {
		t.Fatalf("login without the header: %d", r.code)
	}
	if r := f.do("POST", "/hub/auth/login", `user=alice&password=x`,
		func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }); r.code != 415 {
		t.Fatalf("form post: %d", r.code)
	}
}

func TestCrossSiteWriteHasNoIdentity(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "admin", pw)
	c := f.login("alice", pw).cookie
	site := func(method, v string) func(*http.Request) {
		return func(r *http.Request) { r.Method = method; r.Header.Set("Sec-Fetch-Site", v); r.AddCookie(c) }
	}
	if got := f.whoami(site("POST", "cross-site")); got != " none" {
		t.Fatalf("cross-site POST %q", got)
	}
	if got := f.whoami(site("POST", "same-origin")); got != "alice admin" {
		t.Fatalf("same-origin POST %q", got)
	}
	if got := f.whoami(site("GET", "cross-site")); got != "alice admin" {
		t.Fatalf("cross-site GET %q", got)
	}
	// ForwardAuth: the bot write is the forwarded method, not the GET to /authz.
	fwd := func(r *http.Request) {
		r.URL.Path = "/authz"
		r.Header.Set("X-Forwarded-Method", "DELETE")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.AddCookie(c)
	}
	r := httptest.NewRequest("GET", "/authz", nil)
	fwd(r)
	if _, ok := f.s.current(r); ok {
		t.Fatal("cross-site bot write through /authz kept the identity")
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "viewer", pw)
	f.addUser("bob", "viewer", pw)
	// Negative control: fewer failures than the limit, then the right password works.
	for i := 0; i < MaxPerUser-1; i++ {
		f.login("alice", "wrong password!")
	}
	if r := f.login("alice", pw); r.code != 200 {
		t.Fatalf("under the limit: %d", r.code)
	}
	for i := 0; i < MaxPerUser; i++ {
		if r := f.login("alice", "wrong password!"); r.code != 401 {
			t.Fatalf("failure %d: %d", i+1, r.code)
		}
	}
	r := f.login("alice", pw)
	if r.code != 429 || r.cookie != nil || r.header.Get("Retry-After") == "" {
		t.Fatalf("right password while locked: %d", r.code)
	}
	if r := f.login("bob", pw); r.code != 200 {
		t.Fatalf("another user from the same address: %d", r.code)
	}
	f.now = f.now.Add(Window)
	if r := f.login("alice", pw); r.code != 200 {
		t.Fatalf("after the window: %d", r.code)
	}
	// One address spraying many names hits the per-address cap.
	for i := 0; i < MaxPerIP; i++ {
		f.login(fmt.Sprintf("u%d", i), "wrong password!")
	}
	if r := f.login("bob", pw); r.code != 429 {
		t.Fatalf("address over its cap: %d", r.code)
	}
	other := func(r *http.Request) { r.RemoteAddr = "198.51.100.7:4000" }
	if r := f.do("POST", "/hub/auth/login", `{"user":"bob","password":"`+pw+`"}`, other); r.code != 200 {
		t.Fatalf("another address: %d", r.code)
	}
}

func TestUserChangesApplyToSessions(t *testing.T) {
	f := newFixture(t)
	f.addUser("alice", "operator", pw)
	c := f.login("alice", pw).cookie
	f.addUser("alice", "viewer", pw)
	if got := f.whoami(withCookie(c)); got != "alice viewer" {
		t.Fatalf("after a role change %q", got)
	}
	var out, errb bytes.Buffer
	if rc := CLI(f.dir, []string{"user", "del", "alice"}, nil, &out, &errb); rc != 0 {
		t.Fatal(errb.String())
	}
	if got := f.whoami(withCookie(c)); got != " none" {
		t.Fatalf("after delete %q", got)
	}
}

func TestToLogin(t *testing.T) {
	deny := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no role", 403) })
	h := ToLogin(deny)
	try := func(method, accept, uri string, user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/authz", nil)
		r.Header.Set("X-Forwarded-Method", method)
		r.Header.Set("X-Forwarded-Uri", uri)
		r.Header.Set("Accept", accept)
		if user != "" {
			r.Header.Set("X-Auth-Request-Preferred-Username", user)
			r.Header.Set("X-Auth-Request-Groups", "trade_viewers")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := try("GET", "text/html,*/*", "/bots/ma/?x=1", ""); w.Code != 302 || w.Header().Get("Location") != "/hub/login?rd=%2Fbots%2Fma%2F%3Fx%3D1" {
		t.Fatalf("page: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := try("GET", "text/html", "//evil.example/", ""); w.Header().Get("Location") != "/hub/login?rd=%2F" {
		t.Fatalf("protocol-relative rd: %q", w.Header().Get("Location"))
	}
	if w := try("POST", "text/html", "/bots/ma/api/v1/forcebuy", ""); w.Code != 403 {
		t.Fatalf("write: %d", w.Code)
	}
	if w := try("GET", "application/json", "/bots/ma/api/v1/status", ""); w.Code != 403 {
		t.Fatalf("api call: %d", w.Code)
	}
	if w := try("GET", "text/html", "/", "alice"); w.Code != 403 {
		t.Fatalf("signed in, the next handler decides: %d", w.Code)
	}
}

func TestLoginPageHeaders(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/hub/login", "/hub/login.js", "/hub/login.css"} {
		r := f.do("GET", p, "", nil)
		csp := r.header.Get("Content-Security-Policy")
		if r.code != 200 || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: %d %q", p, r.code, csp)
		}
	}
}

func TestHashAndCLIChecks(t *testing.T) {
	h, err := Hash(pw)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("%q %v", h, err)
	}
	if !Verify(h, pw) || Verify(h, pw+"x") || Verify("$2y$10$notargon", pw) || Verify("", "") {
		t.Fatal("verify")
	}
	dir := t.TempDir()
	var out, errb bytes.Buffer
	for _, args := range [][]string{{"user", "add", "Alice", "viewer"}, {"user", "add", "alice", "root"}, {"user", "add", "alice", "admin"}} {
		pass := pw
		if args[3] == "admin" {
			pass = "short"
		}
		if rc := CLI(dir, args, strings.NewReader(pass+"\n"), &out, &errb); rc == 0 {
			t.Fatalf("%v accepted", args)
		}
	}
	if rc := CLI(dir, []string{"user", "add", "alice", "admin"}, strings.NewReader(pw+"\n"), &out, &errb); rc != 0 {
		t.Fatal(errb.String())
	}
	out.Reset()
	CLI(dir, []string{"user", "list"}, nil, &out, &errb)
	if out.String() != "alice\tadmin\n" {
		t.Fatalf("list %q", out.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	if strings.Contains(string(raw), pw) {
		t.Fatal("users.json holds the password")
	}
	if st, _ := os.Stat(filepath.Join(dir, "users.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("users.json mode %v", st.Mode())
	}
}
