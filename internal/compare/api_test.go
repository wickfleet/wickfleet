package compare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wickfleet/internal/authz"
	"wickfleet/internal/demo"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

var fleetBots = []registry.Bot{
	{Name: "a1", Owner: "alice", Strategy: "S"}, {Name: "a2", Owner: "alice", Strategy: "S"},
	{Name: "b1", Owner: "bob", Strategy: "S"}, {Name: "z1", Owner: "alice", Strategy: "S", Managed: "zoo"},
	{Name: "d1", Owner: demo.User, Strategy: "S"}, {Name: "d2", Owner: demo.User, Strategy: "S"},
}

// seeded writes three finished comparisons: alice's, a mixed one and the demo's.
func seeded(t *testing.T, r Replays) *Service {
	t.Helper()
	dir := t.TempDir()
	for id, owners := range map[string][]string{"c1": {"alice", "alice"}, "c2": {"alice", "bob"}, "c3": {demo.User, demo.User}} {
		c := Comparison{ID: id, Owner: "root", From: "2026-01-01", To: "2026-01-05", State: Done}
		for i, o := range owners {
			m := Measure(replay.Job{Summary: &replay.Summary{Stake: "USDT", StartBalance: 1000, FinalBalance: 1094.6, ProfitNet: 94.6, ProfitPct: 9.46, Trades: 5, Wins: 3}}, nil)
			c.Rows = append(c.Rows, Row{Bot: o + string(rune('1'+i)), Owner: o, State: "done", Metrics: &m,
				Equity: Equity(1000, []replay.Trade{{Close: "2026-01-03 00:00:00", ProfitNet: 94.6}}, "2026-01-01", "2026-01-05")})
		}
		raw, _ := json.Marshal(c)
		os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600)
	}
	s := &Service{Replays: r, Dir: dir}
	ctx := t.Context()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Wait)
	return s
}

type answer struct {
	code int
	body string
}

func call(h http.Handler, method, path, user, groups, body string) answer {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Wickfleet-Request", "1")
	if user != "" {
		r.Header.Set("X-Auth-Request-User", user)
		r.Header.Set("X-Auth-Request-Groups", groups)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return answer{w.Code, w.Body.String()}
}

func handler(a API) http.Handler {
	mux := http.NewServeMux()
	a.Routes(mux)
	return mux
}

func ids(body string) string {
	var out struct {
		Comparisons []Comparison `json:"comparisons"`
		CanRun      bool         `json:"can_run"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	var s []string
	for _, c := range out.Comparisons {
		s = append(s, c.ID)
		for _, r := range c.Rows {
			if r.Equity != nil {
				return "listing carries equity"
			}
		}
	}
	return strings.Join(s, ",") + map[bool]string{true: " run", false: ""}[out.CanRun]
}

func TestAPIRoles(t *testing.T) {
	const window = `"from":"2026-01-01","to":"2026-01-05"}`
	s := seeded(t, &blocking{})
	s.MaxActive = 1
	h := handler(API{Service: s, Bots: func() []registry.Bot { return fleetBots }})
	for _, c := range []struct {
		name, method, path, user, groups, body string
		code                                   int
		want                                   string // listed ids, or a body fragment
	}{
		{"none lists", "GET", "/hub/api/compare", "", "", "", 403, ""},
		{"admin lists all", "GET", "/hub/api/compare", "root", "trade_admins", "", 200, "c3,c2,c1 run"},
		{"owner lists own only", "GET", "/hub/api/compare", "alice", "trade_operators", "", 200, "c1 run"},
		{"viewer lists own, cannot run", "GET", "/hub/api/compare", "bob", "trade_viewers", "", 200, ""},
		{"owner reads own", "GET", "/hub/api/compare/c1", "alice", "trade_operators", "", 200, `"equity"`},
		{"owner cannot read mixed", "GET", "/hub/api/compare/c2", "alice", "trade_operators", "", 404, ""},
		{"admin reads mixed", "GET", "/hub/api/compare/c2", "root", "trade_admins", "", 200, `"bob"`},
		{"viewer cannot read others", "GET", "/hub/api/compare/c1", "bob", "trade_viewers", "", 404, ""},
		{"none reads", "GET", "/hub/api/compare/c1", "", "", "", 403, ""},
		{"none runs", "POST", "/hub/api/compare", "", "", `{"bots":["a1","a2"],` + window, 403, ""},
		{"viewer runs", "POST", "/hub/api/compare", "alice", "trade_viewers", `{"bots":["a1","a2"],` + window, 403, ""},
		{"owner runs own", "POST", "/hub/api/compare", "alice", "trade_operators", `{"bots":["a1","a2"],` + window, 202, `"running"`},
		{"owner runs another's", "POST", "/hub/api/compare", "alice", "trade_operators", `{"bots":["a1","b1"],` + window, 404, "b1"},
		{"admin runs any", "POST", "/hub/api/compare", "root", "trade_admins", `{"bots":["a1","b1"],` + window, 429, "too many"},
		{"one bot", "POST", "/hub/api/compare", "alice", "trade_operators", `{"bots":["a1","a1"],` + window, 400, "two or more"},
		{"both bots and all", "POST", "/hub/api/compare", "alice", "trade_operators", `{"bots":["a1"],"all":true,` + window, 400, "either"},
		{"unknown field", "POST", "/hub/api/compare", "alice", "trade_operators", `{"x":1,` + window, 400, "unknown field"},
	} {
		got := call(h, c.method, c.path, c.user, c.groups, c.body)
		ok := got.code == c.code
		if c.method == "GET" && c.path == "/hub/api/compare" && got.code == 200 {
			ok = ok && ids(got.body) == c.want
		} else if c.want != "" {
			ok = ok && strings.Contains(got.body, c.want)
		}
		if !ok {
			t.Errorf("%s: %d %s, want %d %q", c.name, got.code, strings.TrimSpace(got.body), c.code, c.want)
		}
	}
}

func TestAllMeansTheCallersBots(t *testing.T) {
	fr := &blocking{}
	s := seeded(t, fr)
	s.MaxActive = 5
	h := handler(API{Service: s, Bots: func() []registry.Bot { return fleetBots }})
	if got := call(h, "POST", "/hub/api/compare", "alice", "trade_operators", `{"all":true,"from":"2026-01-01","to":"2026-01-05"}`); got.code != 202 {
		t.Fatalf("%d %s", got.code, got.body)
	}
	var id string
	for _, c := range s.List() {
		if c.State == Running {
			id = c.ID
		}
	}
	c, _ := s.Get(id)
	var names []string
	for _, r := range c.Rows {
		names = append(names, r.Bot)
	}
	// a managed bot and other owners' bots are left out.
	if strings.Join(names, ",") != "a1,a2" {
		t.Fatalf("all for alice: %v", names)
	}
}

func TestDemoReadsLastComparisonOnly(t *testing.T) {
	old := authz.Groups
	authz.Groups = demo.Groups
	t.Cleanup(func() { authz.Groups = old })
	h := demo.Wrap(handler(API{Service: seeded(t, &blocking{}), Bots: func() []registry.Bot { return fleetBots }, Demo: true}))
	list := call(h, "GET", "/hub/api/compare", "root", "trade_admins", "")
	if list.code != 200 || ids(list.body) != "c3" || !strings.Contains(list.body, `"demo":true`) {
		t.Fatalf("demo list with forged admin headers: %d %s", list.code, list.body)
	}
	if got := call(h, "GET", "/hub/api/compare/c3", "", "", ""); got.code != 200 {
		t.Fatalf("demo reads its comparison: %d", got.code)
	}
	if got := call(h, "GET", "/hub/api/compare/c1", "", "", ""); got.code != 404 {
		t.Fatalf("demo reads alice's comparison: %d", got.code)
	}
	if got := call(h, "POST", "/hub/api/compare", "root", "trade_admins", `{"bots":["d1","d2"],"from":"2026-01-01","to":"2026-01-05"}`); got.code != 403 {
		t.Fatalf("demo run: %d, want 403", got.code)
	}
	// Without the wrapper the API itself still refuses in demo mode: a second lock.
	authz.Groups = old
	inner := handler(API{Service: seeded(t, &blocking{}), Bots: func() []registry.Bot { return fleetBots }, Demo: true})
	if got := call(inner, "POST", "/hub/api/compare", "root", "trade_admins", `{"bots":["a1","b1"],"from":"2026-01-01","to":"2026-01-05"}`); got.code != 403 {
		t.Fatalf("demo API run as admin: %d, want 403", got.code)
	}
}

func TestOff(t *testing.T) {
	h := handler(API{Bots: func() []registry.Bot { return fleetBots }})
	if got := call(h, "GET", "/hub/api/compare", "root", "trade_admins", ""); got.code != 200 || !strings.Contains(got.body, `"enabled":false`) {
		t.Fatalf("%d %s", got.code, got.body)
	}
	if got := call(h, "POST", "/hub/api/compare", "root", "trade_admins", `{"bots":["a1","b1"],"from":"2026-01-01","to":"2026-01-05"}`); got.code != 503 {
		t.Fatalf("off: %d", got.code)
	}
}

// money lists every money figure a body carries: balances, absolute profit and
// equity values. Percentages, ratios and counts are not money.
func money(t *testing.T, body string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	var out []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if k == "start_balance" || k == "final_balance" || k == "profit_abs" {
					out = append(out, path+"."+k)
				}
				if k == "equity" {
					if _, isNum := e.(float64); isNum {
						out = append(out, path+"."+k)
					}
				}
				walk(path+"."+k, e)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		}
	}
	walk("", v)
	return out
}

// TestViewersGetNoMoney: viewers and the demo see percentages, ratios and
// counts, never balances, absolute profit or equity in currency. Owners and
// admins still do, which proves the check can fire.
func TestViewersGetNoMoney(t *testing.T) {
	h := handler(API{Service: seeded(t, &blocking{}), Bots: func() []registry.Bot { return fleetBots }})
	for _, c := range []struct {
		name, path, user, groups string
		money                    bool
	}{
		{"admin reads", "/hub/api/compare/c1", "root", "trade_admins", true},
		{"owner reads", "/hub/api/compare/c1", "alice", "trade_operators", true},
		{"owner lists", "/hub/api/compare", "alice", "trade_operators", true},
		{"viewer reads", "/hub/api/compare/c1", "alice", "trade_viewers", false},
		{"viewer lists", "/hub/api/compare", "alice", "trade_viewers", false},
	} {
		got := call(h, "GET", c.path, c.user, c.groups, "")
		if got.code != 200 {
			t.Fatalf("%s: %d %s", c.name, got.code, got.body)
		}
		m := money(t, got.body)
		if (len(m) > 0) != c.money {
			t.Errorf("%s: money fields %v, want money=%v", c.name, m, c.money)
		}
		if !c.money && (!strings.Contains(got.body, `"profit_pct":9.46`) || !strings.Contains(got.body, `"redacted":true`)) {
			t.Errorf("%s: redaction must keep percentages and say so: %s", c.name, got.body)
		}
	}
	// The viewer's curve is still there, as return % on the same dates.
	var v Comparison
	_ = json.Unmarshal([]byte(call(h, "GET", "/hub/api/compare/c1", "alice", "trade_viewers", "").body), &v)
	if eq := v.Rows[0].Equity; len(eq) != 5 || eq[4].ReturnPct != 9.46 || eq[4].Equity != nil {
		t.Errorf("viewer curve %+v", eq)
	}

	old := authz.Groups
	authz.Groups = demo.Groups
	t.Cleanup(func() { authz.Groups = old })
	dh := demo.Wrap(handler(API{Service: seeded(t, &blocking{}), Bots: func() []registry.Bot { return fleetBots }, Demo: true}))
	for _, p := range []string{"/hub/api/compare", "/hub/api/compare/c3"} {
		got := call(dh, "GET", p, "root", "trade_admins", "")
		if m := money(t, got.body); got.code != 200 || len(m) > 0 || !strings.Contains(got.body, `"profit_pct":9.46`) {
			t.Errorf("demo %s: %d money %v", p, got.code, m)
		}
	}
}
