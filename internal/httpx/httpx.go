// Package httpx holds what the hub's API handlers share: the JSON reply,
// the error shape, the CSRF header and the guarded JSON write.
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"wickfleet/internal/authz"
)

// CSRFHeader must be on every write. A custom header forces a CORS
// preflight, so another site cannot send one with the user's cookie.
const CSRFHeader = "X-Wickfleet-Request"

// Mux is where routes go; *http.ServeMux is one, a test recorder another.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// JSON writes v as the reply with status code.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Fail replies {"error": msg} with status code.
func Fail(w http.ResponseWriter, code int, msg string) {
	JSON(w, code, map[string]string{"error": msg})
}

// Guard checks the CSRF header, then the role (refused with deny under min),
// then decodes a strict JSON body of at most 8 KiB into in.
func Guard(w http.ResponseWriter, r *http.Request, min authz.Role, deny string, in any) (authz.Identity, bool) {
	if r.Header.Get(CSRFHeader) != "1" {
		Fail(w, http.StatusForbidden, "missing "+CSRFHeader)
		return authz.Identity{}, false
	}
	id := authz.FromHeaders(r.Header)
	if id.Role < min {
		Fail(w, http.StatusForbidden, deny)
		return id, false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		Fail(w, http.StatusUnsupportedMediaType, "JSON only")
		return id, false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(in); err != nil {
		Fail(w, http.StatusBadRequest, "bad body: "+err.Error())
		return id, false
	}
	return id, true
}
