// Package demo is the public read-only mode (WICKFLEET_AUTH=demo): no login,
// every request is the demo viewer, and nothing that changes state gets through.
package demo

import (
	"net/http"
	"strings"

	"wickfleet/internal/authz"
)

// User owns the demo bots: a viewer sees only bots whose owner is User.
const User = "demo"

const group = "wickfleet-demo"

// Groups replaces authz.Groups in demo mode: the demo group is the only role.
var Groups = map[string]authz.Role{group: authz.Viewer}

// Wrap drops any identity the client sent, sets the demo viewer and refuses
// every method that is not a read. Bot writes arrive as ForwardAuth GETs and
// are refused by the viewer policy in authz.
func Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Auth-Request-") {
				delete(r.Header, k)
			}
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			http.Error(w, "read-only demo", http.StatusForbidden)
			return
		}
		r.Header.Set("X-Auth-Request-Preferred-Username", User)
		r.Header.Set("X-Auth-Request-Groups", group)
		next.ServeHTTP(w, r)
	})
}
