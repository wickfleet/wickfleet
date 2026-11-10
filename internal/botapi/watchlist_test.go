package botapi

import (
	"encoding/json"
	"strings"
	"testing"

	"wickfleet/internal/registry"
)

func TestWatchlistGet(t *testing.T) {
	a, _ := setup(t)
	a.Watchlist = func() registry.Watchlist {
		return registry.Watchlist{Symbols: []registry.Watch{{Symbol: "XIC.TO", Note: "core"}}}
	}
	if w := do(t, a, "GET", "/hub/api/watchlist", "", "", "", false); w.Code != 403 {
		t.Fatalf("anonymous %d", w.Code)
	}
	for group, editable := range map[string]bool{"trade_viewers": false, "trade_operators": false, "trade_admins": true} {
		w := do(t, a, "GET", "/hub/api/watchlist", "alice", group, "", false)
		var out struct {
			Symbols  []registry.Watch `json:"symbols"`
			Editable bool             `json:"editable"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", group, w.Code, w.Body)
		}
		if len(out.Symbols) != 1 || out.Symbols[0].Note != "core" || out.Editable != editable {
			t.Fatalf("%s: %+v", group, out)
		}
	}
}

func TestWatchlistPut(t *testing.T) {
	a, st := setup(t)
	body := `{"symbols":[{"symbol":" xic.to ","note":"core "},{"symbol":"BTC-USD"}]}`
	if w := do(t, a, "PUT", "/hub/api/watchlist", "carol", "trade_admins", body, false); w.Code != 403 {
		t.Fatalf("no csrf %d", w.Code)
	}
	if w := do(t, a, "PUT", "/hub/api/watchlist", "alice", "trade_operators", body, true); w.Code != 403 {
		t.Fatalf("operator %d", w.Code)
	}
	if len(st.writes) != 0 {
		t.Fatalf("refused writes committed: %v", st.writes)
	}
	w := do(t, a, "PUT", "/hub/api/watchlist", "carol", "trade_admins", body, true)
	if w.Code != 202 {
		t.Fatalf("admin %d %s", w.Code, w.Body)
	}
	got, err := registry.ParseWatchlist([]byte(st.writes[registry.WatchFile]))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Names(), ",") != "XIC.TO,BTC-USD" || got.Symbols[0].Note != "core" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(st.msgs[0], "carol sets the watchlist (2 symbols)") {
		t.Fatalf("msg %q", st.msgs[0])
	}
	if w := do(t, a, "PUT", "/hub/api/watchlist", "carol", "trade_admins", body, true); w.Code != 200 {
		t.Fatalf("same list must be unchanged, got %d", w.Code)
	}
	for _, bad := range []string{
		`{"symbols":[{"symbol":"../x"}]}`,
		`{"symbols":[{"symbol":"XIC.TO"},{"symbol":"xic.to"}]}`,
		`{"symbols":[],"extra":1}`,
	} {
		if w := do(t, a, "PUT", "/hub/api/watchlist", "carol", "trade_admins", bad, true); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := do(t, a, "PUT", "/hub/api/watchlist", "carol", "trade_admins", `{"symbols":[]}`, true); w.Code != 202 {
		t.Fatalf("empty list %d %s", w.Code, w.Body)
	}
}
