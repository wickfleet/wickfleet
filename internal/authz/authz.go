// Package authz decides per request whether a Kanidm identity may reach a
// bot, and hands Traefik the bot's API key. It runs as ForwardAuth behind
// oauth2-proxy, whose X-Auth-Request-* headers carry the identity.
package authz

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"wickfleet/internal/registry"
)

// Role is ordered: a higher role can do everything a lower one can.
type Role int

const (
	None Role = iota
	Viewer
	Operator
	Admin
)

func (r Role) String() string {
	return [...]string{"none", "viewer", "operator", "admin"}[r]
}

// Groups maps Kanidm group names (local part, no @domain) to roles. The hub
// replaces it at startup when WICKFLEET_ROLE_GROUPS is set.
var Groups = map[string]Role{
	"trade_viewers":   Viewer,
	"trade_operators": Operator,
	"trade_admins":    Admin,
}

// ParseGroups reads "group=role,..." with roles viewer, operator and admin.
func ParseGroups(s string) (map[string]Role, error) {
	roles := map[string]Role{"viewer": Viewer, "operator": Operator, "admin": Admin}
	out := map[string]Role{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		g, r, _ := strings.Cut(p, "=")
		g = localPart(g)
		role, ok := roles[strings.TrimSpace(r)]
		if g == "" || !ok {
			return nil, fmt.Errorf("%q: want group=viewer, operator or admin", p)
		}
		if _, dup := out[g]; dup {
			return nil, fmt.Errorf("group %s listed twice", g)
		}
		out[g] = role
	}
	if len(out) == 0 {
		return nil, errors.New("no groups")
	}
	return out, nil
}

// Identity is who is asking, as oauth2-proxy saw it.
type Identity struct {
	User string // local part of the Kanidm SPN
	Role Role
}

func localPart(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i]
	}
	return s
}

// FromHeaders reads the identity oauth2-proxy set. Traefik replaces these
// headers with the auth response, so a client cannot supply them; that holds
// only for headers in its authResponseHeaders, which must list every one read
// here. The user is the preferred username: X-Auth-Request-User is the OIDC
// sub, a UUID on Kanidm.
func FromHeaders(h http.Header) Identity {
	user := h.Get("X-Auth-Request-Preferred-Username")
	if user == "" {
		user = h.Get("X-Auth-Request-User")
	}
	id := Identity{User: localPart(user)}
	for _, g := range strings.Split(h.Get("X-Auth-Request-Groups"), ",") {
		if r := Groups[localPart(g)]; r > id.Role {
			id.Role = r
		}
	}
	if id.User == "" {
		id.Role = None
	}
	return id
}

// readOnly reports whether a bot API call changes nothing. Token login and
// refresh are POSTs but only mint the bot's own JWT.
func readOnly(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		return path == "/api/v1/token/login" || path == "/api/v1/token/refresh"
	}
	return false
}

// Allow is the whole policy. Admins reach every bot; others only their own,
// viewers read-only. Managed bots belong to their controller: admins read them.
func Allow(id Identity, bot registry.Bot, method, path string) bool {
	switch {
	case bot.Managed != "":
		return id.Role == Admin && readOnly(method, path)
	case id.Role == Admin:
		return true
	case id.Role < Viewer || id.User != localPart(bot.Owner):
		return false
	case readOnly(method, path):
		return true
	default:
		return id.Role >= Operator
	}
}

// SplitBotPath turns /bots/<name>/rest into name and /rest.
func SplitBotPath(p string) (name, rest string, ok bool) {
	tail, found := strings.CutPrefix(p, "/bots/")
	if !found {
		return "", "", false
	}
	name, rest, _ = strings.Cut(tail, "/")
	if !registry.ValidName(name) {
		return "", "", false
	}
	return name, "/" + rest, true
}

// Handler serves Traefik ForwardAuth.
type Handler struct {
	Bots   func() []registry.Bot
	Secret func(bot string) (string, error)
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := FromHeaders(r.Header)
	if id.Role == None {
		http.Error(w, "no wickfleet role", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Wickfleet-User", id.User)
	w.Header().Set("X-Wickfleet-Role", id.Role.String())

	u, err := url.ParseRequestURI(r.Header.Get("X-Forwarded-Uri"))
	if err != nil {
		http.Error(w, "bad forwarded uri", http.StatusBadRequest)
		return
	}
	name, rest, isBot := SplitBotPath(u.Path)
	if !isBot {
		// UI and hub pages: any role. The hub API checks per bot itself.
		w.WriteHeader(http.StatusOK)
		return
	}
	bot, found := registry.Find(h.Bots(), name)
	if !found {
		http.Error(w, "no such bot", http.StatusNotFound)
		return
	}
	method := r.Header.Get("X-Forwarded-Method")
	if !Allow(id, bot, method, rest) {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	// A Bearer token is the bot's own JWT, minted through this check. Traefik
	// drops an auth response header the hub leaves out, so echo it back.
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") && len(auth) > len("Bearer ") {
		w.Header().Set("Authorization", auth)
	} else {
		pw, err := h.Secret(bot.Name)
		if err != nil {
			http.Error(w, "bot key unavailable", http.StatusBadGateway)
			return
		}
		cred := base64.StdEncoding.EncodeToString([]byte(bot.Name + ":" + pw))
		w.Header().Set("Authorization", "Basic "+cred)
	}
	w.WriteHeader(http.StatusOK)
}

// OpenPrefixes lets non-admins reach only /hub/api/me and the given route prefixes, judged
// by the pattern api would serve: no encoded path passes for another, new ones stay closed.
func OpenPrefixes(api *http.ServeMux, prefixes ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, p := api.Handler(r)
		if _, rest, ok := strings.Cut(p, " "); ok {
			p = rest // drop the method
		}
		open := p == "/hub/api/me"
		for _, area := range prefixes {
			open = open || p == area || strings.HasPrefix(p, area+"/")
		}
		if !open && FromHeaders(r.Header).Role != Admin {
			http.Error(w, "not on this instance", http.StatusForbidden)
			return
		}
		api.ServeHTTP(w, r)
	})
}

// KeyHeader carries a secret Traefik sets on the way to the hub. With it,
// a container on the internal network cannot forge an identity.
const KeyHeader = "X-Wickfleet-Authz-Key"

// RequireKey rejects requests without the key. An empty key turns it off.
func RequireKey(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	want := []byte(key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(KeyHeader)), want) != 1 {
			http.Error(w, "not from the proxy", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
