package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wickfleet/internal/gitstore"
	"wickfleet/internal/registry"
)

const dom = "@idm.example.org"

type fakeStore struct {
	writes map[string]string
	msgs   []string
}

func (f *fakeStore) Change(_ context.Context, rel string, data []byte, msg, author string) error {
	if data == nil {
		if _, ok := f.writes[rel]; !ok && rel != "bots/trend.yaml" {
			return gitstore.ErrNoChange
		}
		f.writes[rel] = "<deleted>"
	} else {
		if f.writes[rel] == string(data) {
			return gitstore.ErrNoChange
		}
		f.writes[rel] = string(data)
	}
	f.msgs = append(f.msgs, msg+" | "+author)
	return nil
}

func (f *fakeStore) ChangeAll(ctx context.Context, files map[string][]byte, msg, author string) error {
	changed := false
	for rel, data := range files {
		if err := f.Change(ctx, rel, data, msg, author); err == nil {
			changed = true
			f.msgs = f.msgs[:len(f.msgs)-1]
		} else if !errors.Is(err, gitstore.ErrNoChange) {
			return err
		}
	}
	if !changed {
		return gitstore.ErrNoChange
	}
	f.msgs = append(f.msgs, msg+" | "+author)
	return nil
}

func (f *fakeStore) Read(_ context.Context, rel string) ([]byte, error) {
	if v, ok := f.writes[rel]; ok && v != "<deleted>" {
		return []byte(v), nil
	}
	if rel == "bots/trend.yaml" {
		return []byte("name: trend\nowner: alice\nstrategy: EmaCross\nfreqaimodel: LightGBMRegressor\nconfig_extra:\n  timeframe: 1h\n"), nil
	}
	return nil, os.ErrNotExist
}

func setup(t *testing.T) (API, *fakeStore) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "strategies"), 0o755)
	os.WriteFile(filepath.Join(dir, "strategies", "EmaCross.py"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "defaults.yaml"), []byte("exchange: kraken\nimage: freqtrade\nwallet: 1000\n"), 0o644)
	st := &fakeStore{writes: map[string]string{}}
	return API{
		Bots: func() []registry.Bot {
			return []registry.Bot{{Name: "trend", Owner: "alice"}, {Name: "bobs", Owner: "bob"}}
		},
		RegistryDir: dir, Images: []string{"freqtrade", "freqtrade_freqai"}, Store: st,
	}, st
}

func do(t *testing.T, a API, method, path, user, group, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	a.Routes(mux)
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		r.Header.Set("X-Auth-Request-User", user+dom)
		r.Header.Set("X-Auth-Request-Groups", group+dom)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		r.Header.Set(CSRFHeader, "1")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestList(t *testing.T) {
	a, _ := setup(t)
	var got struct {
		Bots []View `json:"bots"`
	}
	w := do(t, a, "GET", "/hub/api/bots", "alice", "trade_viewers", "", false)
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Bots) != 1 || got.Bots[0].Name != "trend" || got.Bots[0].Editable || got.Bots[0].URL != "/bots/trend" {
		t.Fatalf("viewer list %s", w.Body)
	}
	w = do(t, a, "GET", "/hub/api/bots", "carol", "trade_admins", "", false)
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Bots) != 2 || !got.Bots[1].Editable {
		t.Fatalf("admin list %s", w.Body)
	}
	if w := do(t, a, "GET", "/hub/api/bots", "eve", "idm_all_persons", "", false); w.Code != 403 {
		t.Fatalf("no role %d", w.Code)
	}
	w = do(t, a, "GET", "/hub/api/strategies", "alice", "trade_viewers", "", false)
	if !strings.Contains(w.Body.String(), "EmaCross.py") {
		t.Fatalf("strategies %s", w.Body)
	}
}

const good = `{"strategy":"EmaCross","strategy_file":"EmaCross.py","pairs":["BTC/USDT"]}`

func TestPutRules(t *testing.T) {
	cases := []struct {
		name, user, group, path, body string
		csrf                          bool
		code                          int
		want                          string
	}{
		{"no csrf header", "alice", "trade_operators", "/hub/api/bots/new1", good, false, 403, "X-Wickfleet-Request"},
		{"viewer", "alice", "trade_viewers", "/hub/api/bots/new1", good, true, 403, "operators"},
		{"operator creates", "alice", "trade_operators", "/hub/api/bots/new1", good, true, 202, "creates"},
		{"operator edits own", "alice", "trade_operators", "/hub/api/bots/trend", good, true, 202, "changes"},
		{"operator edits other", "alice", "trade_operators", "/hub/api/bots/bobs", good, true, 403, "not your bot"},
		{"admin edits other", "carol", "trade_admins", "/hub/api/bots/bobs", good, true, 202, "changes"},
		{"bad name", "alice", "trade_operators", "/hub/api/bots/Bad_Name", good, true, 400, "bad bot name"},
		{"unknown field", "alice", "trade_operators", "/hub/api/bots/new2", `{"strategy":"S","api_key":"x"}`, true, 400, "unknown field"},
		{"live via body", "alice", "trade_operators", "/hub/api/bots/new2", `{"strategy":"S","dry_run":false}`, true, 400, "live trading is not allowed"},
		{"bad image", "alice", "trade_operators", "/hub/api/bots/new2", `{"strategy":"S","image":"alpine"}`, true, 400, "not allowed"},
		{"missing strategy file", "alice", "trade_operators", "/hub/api/bots/new2", `{"strategy":"S","strategy_file":"Nope.py"}`, true, 400, "not in the registry"},
		{"bad class", "alice", "trade_operators", "/hub/api/bots/new2", `{"strategy":"S; rm -rf"}`, true, 400, "bad strategy class"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := setup(t)
			w := do(t, a, "PUT", c.path, c.user, c.group, c.body, c.csrf)
			if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestPutWritesOwnerAndMessage(t *testing.T) {
	a, st := setup(t)
	// An operator cannot hand a bot to someone else.
	body := `{"owner":"bob","strategy":"EmaCross","strategy_file":"EmaCross.py"}`
	if w := do(t, a, "PUT", "/hub/api/bots/new1", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	file := st.writes["bots/new1.yaml"]
	if !strings.Contains(file, "name: new1") || !strings.Contains(file, "owner: alice") || strings.Contains(file, "bob") {
		t.Fatalf("file %q", file)
	}
	if st.msgs[0] != "feat: alice creates bot new1 | alice <alice@wickfleet.invalid>" {
		t.Fatalf("msg %q", st.msgs[0])
	}
	if w := do(t, a, "PUT", "/hub/api/bots/new1", "alice", "trade_operators", body, true); !strings.Contains(w.Body.String(), "unchanged") {
		t.Fatalf("same body twice must be unchanged: %s", w.Body)
	}
}

func TestDelete(t *testing.T) {
	a, st := setup(t)
	if w := do(t, a, "DELETE", "/hub/api/bots/bobs", "alice", "trade_operators", "", true); w.Code != 403 {
		t.Fatalf("other's bot %d", w.Code)
	}
	if w := do(t, a, "DELETE", "/hub/api/bots/trend", "alice", "trade_operators", "", false); w.Code != 403 {
		t.Fatalf("no csrf %d", w.Code)
	}
	if w := do(t, a, "DELETE", "/hub/api/bots/nope", "carol", "trade_admins", "", true); w.Code != 404 {
		t.Fatalf("missing %d", w.Code)
	}
	if w := do(t, a, "DELETE", "/hub/api/bots/trend", "alice", "trade_operators", "", true); w.Code != 202 || st.writes["bots/trend.yaml"] != "<deleted>" {
		t.Fatalf("delete %d %v", w.Code, st.writes)
	}
}

func TestNoStoreConfigured(t *testing.T) {
	a, _ := setup(t)
	a.Store = nil
	if w := do(t, a, "PUT", "/hub/api/bots/x1", "alice", "trade_operators", good, true); w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}

func TestPutKeepsWhatTheUICannotExpress(t *testing.T) {
	a, st := setup(t)
	body := `{"strategy":"EmaCross","strategy_file":"EmaCross.py","wallet":2000}`
	if w := do(t, a, "PUT", "/hub/api/bots/trend", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	f := st.writes["bots/trend.yaml"]
	for _, want := range []string{"freqaimodel: LightGBMRegressor", "timeframe: 1h", "wallet: 2000", "owner: alice"} {
		if !strings.Contains(f, want) {
			t.Errorf("lost %q:\n%s", want, f)
		}
	}
}

func TestManagedBotsAreReadOnly(t *testing.T) {
	a, st := setup(t)
	a.Bots = func() []registry.Bot {
		return []registry.Bot{{Name: "rsi", Owner: "alice", Managed: "daniel"}, {Name: "trend", Owner: "alice"}}
	}
	for _, who := range []string{"trade_operators", "trade_admins"} {
		if w := do(t, a, "PUT", "/hub/api/bots/rsi", "alice", who, good, true); w.Code != 403 || !strings.Contains(w.Body.String(), "controller") {
			t.Errorf("%s changes an agent: %d %s", who, w.Code, w.Body)
		}
		if w := do(t, a, "DELETE", "/hub/api/bots/rsi", "alice", who, "", true); w.Code != 403 {
			t.Errorf("%s deletes an agent: %d %s", who, w.Code, w.Body)
		}
	}
	if len(st.writes) != 0 {
		t.Fatalf("committed %v", st.writes)
	}
	if w := do(t, a, "PUT", "/hub/api/bots/new1", "alice", "trade_operators", good, true); w.Code != 202 {
		t.Fatalf("without the limit an operator still creates: %d %s", w.Code, w.Body)
	}
	a.AdminCreatesOnly = true
	if w := do(t, a, "PUT", "/hub/api/bots/new2", "alice", "trade_operators", good, true); w.Code != 403 || !strings.Contains(w.Body.String(), "admins") {
		t.Fatalf("operator creates under the limit: %d %s", w.Code, w.Body)
	}
	if w := do(t, a, "PUT", "/hub/api/bots/new2", "carol", "trade_admins", good, true); w.Code != 202 {
		t.Fatalf("admin creates under the limit: %d %s", w.Code, w.Body)
	}
	if w := do(t, a, "PUT", "/hub/api/bots/trend", "alice", "trade_operators", good, true); w.Code != 202 {
		t.Fatalf("an owner still edits a plain bot under the limit: %d %s", w.Code, w.Body)
	}
}

// A managed bot is committed before the checkout syncs it: the head file decides.
func TestFreshAgentIsNotABot(t *testing.T) {
	a, st := setup(t)
	a.AdminCreatesOnly, a.Bots = true, func() []registry.Bot { return nil }
	born := "name: kid-new\nowner: ctl\nstrategy: EmaCross\nstrategy_file: EmaCross.py\narena: zoo\n"
	st.writes["bots/kid-new.yaml"] = born
	if w := do(t, a, "PUT", "/hub/api/bots/kid-new", "dan", "trade_admins", good, true); w.Code != 403 || !strings.Contains(w.Body.String(), "controller") {
		t.Fatalf("admin overwrites a fresh agent: %d %s", w.Code, w.Body)
	}
	if st.writes["bots/kid-new.yaml"] != born || len(st.msgs) != 0 {
		t.Fatalf("committed %v", st.msgs)
	}
}
