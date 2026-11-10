// Package login is the hub's built-in login (WICKFLEET_AUTH=builtin): local
// users with a role in users.json, a session cookie, a login rate limit and a
// one-time setup token for the first admin. It stands in for oauth2-proxy by
// setting the X-Auth-Request-* headers the rest of the hub already reads.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wickfleet/internal/authz"
	"wickfleet/internal/httpx"
)

// Cookie is the session cookie's name.
const Cookie = "wickfleet_session"

// Groups replaces authz.Groups in builtin mode: the only groups Wrap sets.
var Groups = map[string]authz.Role{
	"wickfleet-viewer":   authz.Viewer,
	"wickfleet-operator": authz.Operator,
	"wickfleet-admin":    authz.Admin,
}

// Limits: failed logins per user and source address, and per source alone, in Window.
const (
	Window      = 15 * time.Minute
	MaxPerUser  = 5
	MaxPerIP    = 20
	SetupMaxAge = time.Hour
)

//go:embed page
var page embed.FS

// Service holds the users file, the sessions and the failure counts.
type Service struct {
	Users string        // users.json
	Setup string        // the setup token file, see SetupToken
	TTL   time.Duration // session lifetime from login
	Now   func() time.Time
	Log   func(format string, args ...any)

	mu       sync.Mutex
	cache    map[string]User
	cacheMod time.Time
	cacheLen int64
	sessions map[string]session // by sha256 of the cookie value
	fails    map[string][]time.Time
	hashing  chan struct{}
}

type session struct {
	user    string
	expires time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
		return
	}
	log.Printf(format, args...)
}

// users is users.json, read again when its size or mtime changes, so the
// user CLI takes effect without a restart.
func (s *Service) users() (map[string]User, error) {
	st, err := os.Stat(s.Users)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cache, s.cacheMod, s.cacheLen = map[string]User{}, time.Time{}, -1
		return s.cache, nil
	case err != nil:
		return nil, err
	}
	if s.cache != nil && st.ModTime().Equal(s.cacheMod) && st.Size() == s.cacheLen {
		return s.cache, nil
	}
	u, err := loadUsers(s.Users)
	if err != nil {
		return nil, err
	}
	s.cache, s.cacheMod, s.cacheLen = u, st.ModTime(), st.Size()
	return u, nil
}

func tokenID(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// current is the signed-in user of r, if any. A write that the browser marks
// cross-site carries no identity: /authz judges the forwarded method.
func (s *Service) current(r *http.Request) (User, bool) {
	c, err := r.Cookie(Cookie)
	if err != nil || c.Value == "" {
		return User{}, false
	}
	method := r.Method
	if r.URL.Path == "/authz" {
		method = r.Header.Get("X-Forwarded-Method")
	}
	if !safeMethod(method) && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return User{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tokenID(c.Value)
	sess, ok := s.sessions[id]
	if !ok {
		return User{}, false
	}
	if !s.now().Before(sess.expires) {
		delete(s.sessions, id)
		return User{}, false
	}
	users, err := s.users()
	if err != nil {
		s.logf("login: %v", err)
		return User{}, false
	}
	u, ok := users[sess.user]
	return u, ok
}

// Wrap drops every X-Auth-Request-* header a client sent, then sets them from
// the session, so the rest of the hub sees only a signed-in identity.
func (s *Service) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Auth-Request-") {
				delete(r.Header, k)
			}
		}
		if u, ok := s.current(r); ok {
			r.Header.Set("X-Auth-Request-Preferred-Username", u.Name)
			r.Header.Set("X-Auth-Request-Groups", "wickfleet-"+u.Role)
		}
		next.ServeHTTP(w, r)
	})
}

// ToLogin sends a browser that opens a page without a session to the login
// page and back; any other unauthenticated request gets the ForwardAuth answer.
func ToLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authz.FromHeaders(r.Header).Role == authz.None && r.Header.Get("X-Forwarded-Method") == http.MethodGet &&
			strings.Contains(r.Header.Get("Accept"), "text/html") {
			rd := "/"
			if u, err := url.ParseRequestURI(r.Header.Get("X-Forwarded-Uri")); err == nil && localPath(u.RequestURI()) {
				rd = u.RequestURI()
			}
			http.Redirect(w, r, "/hub/login?rd="+url.QueryEscape(rd), http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localPath is a path on this origin: one slash, never // or /\ (another host to a browser).
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "/\\")
}

// clientIP is the address the edge proxy saw (the last X-Forwarded-For hop), else the peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		if ip := strings.TrimSpace(hops[len(hops)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blocked reports how long the keys stay locked, 0 when free. Holds s.mu.
func (s *Service) blocked(now time.Time, keys map[string]int) time.Duration {
	if s.fails == nil {
		s.fails = map[string][]time.Time{}
	}
	var wait time.Duration
	for k, max := range keys {
		kept := s.fails[k][:0]
		for _, t := range s.fails[k] {
			if now.Sub(t) < Window {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(s.fails, k)
			continue
		}
		s.fails[k] = kept
		if len(kept) >= max {
			if d := Window - now.Sub(kept[len(kept)-max]); d > wait {
				wait = d
			}
		}
	}
	return wait
}

func (s *Service) fail(now time.Time, keys map[string]int) {
	if len(s.fails) > 100_000 { // a flood of names; forget them rather than grow
		s.fails = map[string][]time.Time{}
	}
	for k := range keys {
		s.fails[k] = append(s.fails[k], now)
	}
}

// verify runs at most two argon2id checks at once, 19 MiB each.
func (s *Service) verify(hash, pw string) bool {
	s.mu.Lock()
	if s.hashing == nil {
		s.hashing = make(chan struct{}, 2)
	}
	sem := s.hashing
	s.mu.Unlock()
	sem <- struct{}{}
	defer func() { <-sem }()
	return Verify(hash, pw)
}

// startSession sets the cookie for user.
func (s *Service) startSession(w http.ResponseWriter, user string) error {
	tok, err := randomToken()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	now := s.now()
	for id, sess := range s.sessions {
		if !now.Before(sess.expires) {
			delete(s.sessions, id)
		}
	}
	s.sessions[tokenID(tok)] = session{user: user, expires: now.Add(s.TTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: Cookie, Value: tok, Path: "/", MaxAge: int(s.TTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return nil
}

// decode checks the CSRF header and reads a small strict JSON body.
func decode(w http.ResponseWriter, r *http.Request, in any) bool {
	if r.Header.Get(httpx.CSRFHeader) != "1" {
		httpx.Fail(w, http.StatusForbidden, "missing "+httpx.CSRFHeader)
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		httpx.Fail(w, http.StatusUnsupportedMediaType, "JSON only")
		return false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad body")
		return false
	}
	return true
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
	httpx.Fail(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ User, Password string }
	if !decode(w, r, &in) {
		return
	}
	ip := clientIP(r)
	keys := map[string]int{"ip|" + ip: MaxPerIP, "user|" + ip + "|" + in.User: MaxPerUser}
	now := s.now()
	s.mu.Lock()
	wait := s.blocked(now, keys)
	users, err := s.users()
	s.mu.Unlock()
	if wait > 0 {
		tooMany(w, wait)
		return
	}
	if err != nil {
		s.logf("login: %v", err)
		httpx.Fail(w, http.StatusInternalServerError, "users file unreadable")
		return
	}
	u, known := users[in.User]
	hash := dummy
	if known {
		hash = u.Hash
	}
	if ok := s.verify(hash, in.Password); !ok || !known || len(in.Password) > MaxPassword {
		s.mu.Lock()
		s.fail(now, keys)
		s.mu.Unlock()
		s.logf("login: failed for %q from %s", in.User, ip)
		httpx.Fail(w, http.StatusUnauthorized, "wrong user or password")
		return
	}
	s.mu.Lock()
	delete(s.fails, "user|"+ip+"|"+in.User)
	s.mu.Unlock()
	if err := s.startSession(w, u.Name); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "no session")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"user": u.Name, "role": u.Role})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(httpx.CSRFHeader) != "1" {
		httpx.Fail(w, http.StatusForbidden, "missing "+httpx.CSRFHeader)
		return
	}
	if c, err := r.Cookie(Cookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, tokenID(c.Value))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: Cookie, Value: "", Path: "/", MaxAge: -1,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(r)
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "not signed in")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"user": u.Name, "role": u.Role})
}

// state tells the page whether the first admin is still to be set up.
func (s *Service) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	users, err := s.users()
	s.mu.Unlock()
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "users file unreadable")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]bool{"setup": len(users) == 0})
}

// setup creates the first admin with the token from SetupToken, once.
func (s *Service) setup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token, User, Password string }
	if !decode(w, r, &in) {
		return
	}
	code, msg := s.firstAdmin(clientIP(r), in.Token, in.User, in.Password)
	if code == http.StatusTooManyRequests {
		tooMany(w, Window)
		return
	}
	if code != http.StatusOK {
		httpx.Fail(w, code, msg)
		return
	}
	if err := s.startSession(w, in.User); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "no session")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"user": in.User, "role": "admin"})
}

// firstAdmin checks the setup token and writes users.json with one admin.
func (s *Service) firstAdmin(ip, token, user, pw string) (int, string) {
	keys := map[string]int{"ip|" + ip: MaxPerIP, "setup|" + ip: MaxPerUser}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blocked(now, keys) > 0 {
		return http.StatusTooManyRequests, ""
	}
	users, err := s.users()
	if err != nil {
		return http.StatusInternalServerError, "users file unreadable"
	}
	if len(users) > 0 {
		return http.StatusConflict, "already set up"
	}
	st, err := os.Stat(s.Setup)
	if err != nil {
		return http.StatusForbidden, "no setup token: run wickfleet-hub setup-token"
	}
	if now.Sub(st.ModTime()) > SetupMaxAge {
		os.Remove(s.Setup)
		return http.StatusForbidden, "setup token expired: run wickfleet-hub setup-token again"
	}
	raw, err := os.ReadFile(s.Setup)
	if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(string(raw))), []byte(tokenID(token))) != 1 {
		s.fail(now, keys)
		return http.StatusForbidden, "wrong setup token"
	}
	if err := checkUser(user, "admin"); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	if err := checkPassword(pw); err != nil {
		return http.StatusBadRequest, err.Error()
	}
	hash, err := Hash(pw)
	if err == nil {
		err = saveUsers(s.Users, map[string]User{user: {Name: user, Role: "admin", Hash: hash}})
	}
	if err != nil {
		s.logf("login setup: %v", err)
		return http.StatusInternalServerError, "could not save the user"
	}
	os.Remove(s.Setup)
	s.logf("login: first admin %q set up from %s", user, ip)
	return http.StatusOK, ""
}

// pageHeaders lock the login page down: own scripts only, no frames, no caching.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func serve(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := page.ReadFile("page/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		pageHeaders(w)
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	}
}

// Routes registers the login page and /hub/auth/*.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /hub/login", serve("login.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /hub/login.js", serve("login.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /hub/login.css", serve("login.css", "text/css; charset=utf-8"))
	mux.HandleFunc("POST /hub/auth/login", s.login)
	mux.HandleFunc("POST /hub/auth/logout", s.logout)
	mux.HandleFunc("POST /hub/auth/setup", s.setup)
	mux.HandleFunc("GET /hub/auth/me", s.me)
	mux.HandleFunc("GET /hub/auth/state", s.state)
}
