package botapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wickfleet/internal/drafter"
	"wickfleet/internal/registry"
)

// spec is the EmaCross testdata spec under another class name.
func spec(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../examples/registry/strategies/EmaCross.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["name"] = name
	out, _ := json.Marshal(m)
	return string(out)
}

func TestSpecCheck(t *testing.T) {
	a, st := setup(t)
	w := do(t, a, "POST", "/hub/api/specs/check", "alice", "trade_operators", spec(t, "Breakout"), true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "class Breakout(IStrategy)") || !strings.Contains(w.Body.String(), "Buy when") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := do(t, a, "POST", "/hub/api/specs/check", "alice", "trade_operators", `{"version":1,"name":"bad name"}`, true); w.Code != 400 {
		t.Fatalf("invalid %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/specs/check", "alice", "trade_viewers", spec(t, "Breakout"), true); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	if len(st.writes) != 0 {
		t.Fatal("check must not write")
	}
}

func TestSpecPut(t *testing.T) {
	a, st := setup(t)
	body := spec(t, "Breakout")
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "alice", "trade_operators", body, false); w.Code != 403 {
		t.Fatalf("no csrf %d", w.Code)
	}
	if w := do(t, a, "PUT", "/hub/api/specs/Other", "alice", "trade_operators", body, true); w.Code != 400 {
		t.Fatalf("path must match name %d", w.Code)
	}
	// An author in the body is ignored; the caller becomes the author.
	forged := strings.Replace(body, `"version":1`, `"version":1,"author":"carol"`, 1)
	w := do(t, a, "PUT", "/hub/api/specs/Breakout", "alice", "trade_operators", forged, true)
	if w.Code != 202 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	py, js := st.writes["strategies/Breakout.py"], st.writes["strategies/Breakout.json"]
	if !strings.HasPrefix(py, generatedMark) || !strings.Contains(py, "class Breakout(IStrategy)") {
		t.Fatalf("py %q", py)
	}
	if !strings.Contains(js, `"author": "alice"`) || len(st.msgs) != 1 || !strings.Contains(st.msgs[0], "alice adds strategy Breakout") {
		t.Fatalf("json %s msgs %v", js, st.msgs)
	}
	// Someone else may not change it; the author and admins may.
	changed := strings.Replace(body, `"stoploss":-0.15`, `"stoploss":-0.1`, 1)
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "bob", "trade_operators", changed, true); w.Code != 403 {
		t.Fatalf("bob %d", w.Code)
	}
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "alice", "trade_operators", changed, true); w.Code != 202 {
		t.Fatalf("author %d %s", w.Code, w.Body)
	}
	if !strings.Contains(st.writes["strategies/Breakout.json"], `"author": "alice"`) || !strings.Contains(st.writes["strategies/Breakout.py"], "stoploss = -0.1") {
		t.Fatal("update must keep the author and regenerate the code")
	}
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "carol", "trade_admins", changed, true); w.Code != 200 {
		t.Fatalf("admin, same content -> unchanged, got %d", w.Code)
	}
	// A hand-written strategy of that name is never overwritten.
	st.writes["strategies/Handmade.py"] = "class Handmade: pass\n"
	if w := do(t, a, "PUT", "/hub/api/specs/Handmade", "alice", "trade_operators", spec(t, "Handmade"), true); w.Code != 409 {
		t.Fatalf("hand-written %d", w.Code)
	}
}

func TestSpecInUse(t *testing.T) {
	a, st := setup(t)
	do(t, a, "PUT", "/hub/api/specs/Breakout", "alice", "trade_operators", spec(t, "Breakout"), true)
	a.Bots = func() []registry.Bot {
		return []registry.Bot{{Name: "bobs", Owner: "bob", StrategyFile: "Breakout.py"}}
	}
	changed := strings.Replace(spec(t, "Breakout"), `"stoploss":-0.2`, `"stoploss":-0.15`, 1)
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "alice", "trade_operators", changed, true); w.Code != 403 || !strings.Contains(w.Body.String(), "bobs") {
		t.Fatalf("bob's bot runs it: %d %s", w.Code, w.Body)
	}
	if w := do(t, a, "DELETE", "/hub/api/specs/Breakout", "carol", "trade_admins", "", true); w.Code != 409 {
		t.Fatalf("delete in use %d", w.Code)
	}
	a.Bots = func() []registry.Bot { return nil }
	if w := do(t, a, "DELETE", "/hub/api/specs/Breakout", "alice", "trade_operators", "", true); w.Code != 202 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if st.writes["strategies/Breakout.py"] != "<deleted>" || st.writes["strategies/Breakout.json"] != "<deleted>" {
		t.Fatalf("both files must go: %v", st.writes)
	}
	if w := do(t, a, "GET", "/hub/api/specs/..%2F..%2Fetc", "alice", "trade_viewers", "", false); w.Code != 404 {
		t.Fatalf("traversal %d", w.Code)
	}
}

// A managed bot's strategy changes through its controller alone: no admin rewrites
// or deletes it, a running bot's spec or a reserved author's not yet in the checkout.
func TestManagedSpecsChangeByControllerOnly(t *testing.T) {
	a, st := setup(t)
	a.AdminCreatesOnly, a.ReservedAuthor = true, "ctl"
	a.Bots = func() []registry.Bot {
		return []registry.Bot{{Name: "zoo-rsi", Owner: "lin", Managed: "zoo", Strategy: "Founder", StrategyFile: "Founder.py"}}
	}
	st.writes["strategies/Founder.json"] = strings.Replace(spec(t, "Founder"), `"version":1`, `"version":1,"author":"dan"`, 1)
	st.writes["strategies/CtlKid.json"] = strings.Replace(spec(t, "CtlKid"), `"version":1`, `"version":1,"author":"ctl"`, 1)
	for _, name := range []string{"Founder", "CtlKid"} {
		changed := strings.Replace(spec(t, name), `"stoploss":-0.2`, `"stoploss":-0.01`, 1)
		for _, who := range []string{"dan", "lin"} {
			if w := do(t, a, "PUT", "/hub/api/specs/"+name, who, "trade_admins", changed, true); w.Code != 403 {
				t.Errorf("%s rewrites %s: %d %s", who, name, w.Code, w.Body)
			}
			if w := do(t, a, "DELETE", "/hub/api/specs/"+name, who, "trade_admins", "", true); w.Code != 403 {
				t.Errorf("%s deletes %s: %d %s", who, name, w.Code, w.Body)
			}
		}
	}
	if len(st.msgs) != 0 {
		t.Fatalf("committed %v", st.msgs)
	}
	if w := do(t, a, "PUT", "/hub/api/specs/Breakout", "dan", "trade_admins", spec(t, "Breakout"), true); w.Code != 202 {
		t.Fatalf("an admin still adds a spec: %d %s", w.Code, w.Body)
	}
}

func TestSpecListAndGet(t *testing.T) {
	a, _ := setup(t)
	os.WriteFile(filepath.Join(a.RegistryDir, "strategies", "Breakout.json"),
		[]byte(strings.Replace(spec(t, "Breakout"), `"version":1`, `"version":1,"author":"alice"`, 1)), 0o644)
	w := do(t, a, "GET", "/hub/api/specs", "bob", "trade_operators", "", false)
	var out struct {
		Specs     []SpecView `json:"specs"`
		CanCreate bool       `json:"can_create"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Specs) != 1 || out.Specs[0].Name != "Breakout" || out.Specs[0].Editable || !out.CanCreate {
		t.Fatalf("%+v", out)
	}
	w = do(t, a, "GET", "/hub/api/specs/Breakout", "alice", "trade_viewers", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"describe":"Breakout trades 1d`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestModelSpecNeedsFreqaiBot(t *testing.T) {
	a, _ := setup(t)
	raw, _ := os.ReadFile("../strategy/testdata/FreqaiExample.json")
	os.WriteFile(filepath.Join(a.RegistryDir, "strategies", "FreqaiExample.json"), raw, 0o644)
	os.WriteFile(filepath.Join(a.RegistryDir, "strategies", "FreqaiExample.py"), []byte("x"), 0o644)
	plain := `{"strategy":"FreqaiExample","strategy_file":"FreqaiExample.py","pairs":["BTC/USDT"]}`
	if w := do(t, a, "PUT", "/hub/api/bots/fq", "alice", "trade_operators", plain, true); w.Code != 400 || !strings.Contains(w.Body.String(), "FreqAI") {
		t.Fatalf("a plain bot on a model spec: %d %s", w.Code, w.Body)
	}
}

type fakeDrafter struct {
	block   chan struct{}
	calls   atomic.Int32
	started chan struct{}
}

func (f *fakeDrafter) Draft(_ context.Context, sentence string) (drafter.Result, error) {
	f.calls.Add(1)
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	if strings.Contains(sentence, "fail") {
		return drafter.Result{}, drafter.ErrNoDraft
	}
	return drafter.Result{Describe: "RsiDip trades 1h candles", Provider: "fake", Attempts: 1}, nil
}

func pollDraft(t *testing.T, a API, user, group, id string) DraftJob {
	t.Helper()
	for i := 0; i < 200; i++ {
		w := do(t, a, "GET", "/hub/api/specs/draft/"+id, user, group, "", false)
		var j DraftJob
		json.Unmarshal(w.Body.Bytes(), &j)
		if w.Code != 200 || j.State != "running" {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("draft never finished")
	return DraftJob{}
}

func TestSpecDraft(t *testing.T) {
	a, st := setup(t)
	body := `{"sentence":"hourly rsi dip buyer please"}`
	if w := do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_operators", body, true); w.Code != 503 {
		t.Fatalf("no drafter %d", w.Code)
	}
	fd := &fakeDrafter{}
	a.Drafts = NewDrafts(fd)
	if w := do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_viewers", body, true); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_operators", body, false); w.Code != 403 {
		t.Fatalf("no csrf %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_operators", `{"sentence":"short"}`, true); w.Code != 400 {
		t.Fatalf("short %d", w.Code)
	}
	// Answers at once with an id; the result comes by polling.
	fd.block, fd.started = make(chan struct{}), make(chan struct{})
	w := do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_operators", body, true)
	var started DraftJob
	json.Unmarshal(w.Body.Bytes(), &started)
	if w.Code != 202 || started.State != "running" || started.ID == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	<-fd.started
	if w := do(t, a, "POST", "/hub/api/specs/draft", "bob", "trade_operators", body, true); w.Code != 429 {
		t.Fatalf("second while running %d", w.Code)
	}
	if w := do(t, a, "GET", "/hub/api/specs/draft/"+started.ID, "bob", "trade_operators", "", false); w.Code != 404 {
		t.Fatalf("bob sees alice's draft %d", w.Code)
	}
	close(fd.block)
	j := pollDraft(t, a, "alice", "trade_operators", started.ID)
	if j.State != "done" || j.Result == nil || j.Result.Provider != "fake" {
		t.Fatalf("%+v", j)
	}
	fd.block, fd.started = nil, nil
	w = do(t, a, "POST", "/hub/api/specs/draft", "alice", "trade_operators", `{"sentence":"this will fail now"}`, true)
	json.Unmarshal(w.Body.Bytes(), &started)
	if j := pollDraft(t, a, "carol", "trade_admins", started.ID); j.State != "failed" || !strings.Contains(j.Error, "no valid strategy") {
		t.Fatalf("failed draft, seen by an admin: %+v", j)
	}
	if len(st.writes) != 0 {
		t.Fatal("a draft must not write")
	}
}
