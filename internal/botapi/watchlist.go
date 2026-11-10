package botapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"wickfleet/internal/authz"
	"wickfleet/internal/gitstore"
	"wickfleet/internal/registry"
)

// WatchRoutes registers /hub/api/watchlist. The list is shared by everyone
// and feeds the Telegram summary, so only admins change it.
func (a API) WatchRoutes(mux Mux) {
	mux.HandleFunc("GET /hub/api/watchlist", a.watchGet)
	mux.HandleFunc("PUT /hub/api/watchlist", a.watchPut)
}

func (a API) watchGet(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	wl := registry.Watchlist{}
	if a.Watchlist != nil {
		wl = a.Watchlist()
	}
	if wl.Symbols == nil {
		wl.Symbols = []registry.Watch{}
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"symbols":  wl.Symbols,
		"editable": id.Role == authz.Admin && a.Store != nil,
	})
}

func (a API) watchPut(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) != "1" {
		fail(w, http.StatusForbidden, "missing "+CSRFHeader)
		return
	}
	id := authz.FromHeaders(r.Header)
	if id.Role != authz.Admin {
		fail(w, http.StatusForbidden, "admins only")
		return
	}
	if a.Store == nil {
		fail(w, http.StatusServiceUnavailable, "registry writes are not configured")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, http.StatusUnsupportedMediaType, "JSON only")
		return
	}
	var in registry.Watchlist
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	for i := range in.Symbols {
		in.Symbols[i].Symbol = strings.ToUpper(strings.TrimSpace(in.Symbols[i].Symbol))
		in.Symbols[i].Note = strings.TrimSpace(in.Symbols[i].Note)
	}
	if err := in.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Symbols == nil {
		in.Symbols = []registry.Watch{}
	}
	data, err := yaml.Marshal(in)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	err = a.Store.Change(r.Context(), registry.WatchFile, data,
		fmt.Sprintf("feat: %s sets the watchlist (%d symbols)", id.User, len(in.Symbols)), author(id))
	if errors.Is(err, gitstore.ErrNoChange) {
		jsonOut(w, http.StatusOK, map[string]string{"result": "unchanged"})
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "commit failed")
		return
	}
	jsonOut(w, http.StatusAccepted, map[string]string{"result": "saved", "note": "signals update after the next sync, about a minute"})
}
