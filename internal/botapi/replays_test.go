package botapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

type fakeReplays struct {
	got    []registry.Bot
	busy   bool
	jobs   []replay.Job
	passed map[string]bool // owner + "|" + settings
	robust map[string]*replay.Robustness
}

func (f *fakeReplays) Passed(owner, settings string, _ time.Duration) (replay.Job, bool) {
	return replay.Job{Robust: f.robust[owner+"|"+settings]}, f.passed[owner+"|"+settings]
}

func (f *fakeReplays) Submit(owner string, b registry.Bot, from, to string) (*replay.Job, error) {
	if f.busy {
		return nil, replay.ErrBusy
	}
	if from >= to {
		return nil, errBadRange
	}
	f.got = append(f.got, b)
	j := replay.Job{ID: "r1", Kind: "replay", Owner: owner, Bot: b.Name, State: replay.Queued}
	f.jobs = append(f.jobs, j)
	return &j, nil
}
func (f *fakeReplays) List() []replay.Job { return f.jobs }
func (f *fakeReplays) Get(id string) (replay.Job, bool) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, true
		}
	}
	return replay.Job{}, false
}
func (f *fakeReplays) Trades(string) ([]replay.Trade, error) {
	return []replay.Trade{{Pair: "BTC/USDT", Fee: 1.5}}, nil
}

type rangeErr struct{}

func (rangeErr) Error() string { return "from must be before to" }

var errBadRange = rangeErr{}

func TestReplayPost(t *testing.T) {
	a, _ := setup(t)
	fr := &fakeReplays{}
	a.Replays = fr
	body := `{"bot":"trend","from":"2025-10-18","to":"2026-10-18"}`
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", body, false); w.Code != 403 {
		t.Fatalf("no csrf %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_viewers", body, true); w.Code != 403 {
		t.Fatalf("viewer %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/replays", "carol", "trade_operators", body, true); w.Code != 404 {
		t.Fatalf("someone else's bot must look missing, got %d", w.Code)
	}
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("own bot %d %s", w.Code, w.Body)
	}
	if w := do(t, a, "POST", "/hub/api/replays", "carol", "trade_admins", body, true); w.Code != 202 {
		t.Fatalf("admin on any bot %d", w.Code)
	}
	draft := `{"draft":{"owner":"mallory","strategy":"EmaCross","strategy_file":"EmaCross.py","pairs":["ETH/USDT"],"stake_currency":"USDT"},"from":"2025-10-18","to":"2026-10-18"}`
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", draft, true); w.Code != 202 {
		t.Fatalf("draft %d %s", w.Code, w.Body)
	}
	d := fr.got[len(fr.got)-1]
	if d.Owner != "alice" || d.Exchange != "kraken" || d.Image != "freqtrade" || d.Pairs[0] != "ETH/USDT" {
		t.Fatalf("draft must take defaults and the caller as owner: %+v", d)
	}
	for _, bad := range []string{
		`{"from":"2025-10-18","to":"2026-10-18"}`,
		`{"bot":"trend","draft":{"strategy":"EmaCross"},"from":"2025-10-18","to":"2026-10-18"}`,
		`{"draft":{"strategy":"EmaCross","strategy_file":"Nope.py"},"from":"2025-10-18","to":"2026-10-18"}`,
		`{"bot":"trend","from":"2026-10-18","to":"2025-10-18"}`,
		`{"bot":"trend","from":"2025-10-18","to":"2026-10-18","x":1}`,
	} {
		if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", bad, true); w.Code != 400 {
			t.Errorf("%s: %d %s", bad, w.Code, w.Body)
		}
	}
	fr.busy = true
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", body, true); w.Code != 429 {
		t.Fatalf("busy %d", w.Code)
	}
}

func TestReplayVisibility(t *testing.T) {
	a, _ := setup(t)
	fr := &fakeReplays{jobs: []replay.Job{
		{ID: "r1", Kind: "replay", Owner: "alice", State: replay.Done},
		{ID: "r2", Kind: "replay", Owner: "bob", State: replay.Done},
		{ID: "f1", Kind: "fetch", Owner: "hub", State: replay.Done},
	}}
	a.Replays = fr
	list := func(user, group string) string {
		w := do(t, a, "GET", "/hub/api/replays", user, group, "", false)
		var out struct {
			Jobs []replay.Job `json:"jobs"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		var ids []string
		for _, j := range out.Jobs {
			ids = append(ids, j.ID)
		}
		return strings.Join(ids, ",")
	}
	if got := list("alice", "trade_viewers"); got != "r1" {
		t.Fatalf("alice sees %s", got)
	}
	if got := list("carol", "trade_admins"); got != "r1,r2,f1" {
		t.Fatalf("admin sees %s", got)
	}
	a.ReplaySince, a.ReplaySinceBy = "2020-01-01", map[string]string{"kraken": "2025-01-01"}
	if w := do(t, a, "GET", "/hub/api/replays", "alice", "trade_viewers", "", false); !strings.Contains(w.Body.String(), `"since":"2020-01-01","since_by_exchange":{"kraken":"2025-01-01"}`) {
		t.Fatalf("start dates: %s", w.Body)
	}
	if w := do(t, a, "GET", "/hub/api/replays/r2", "alice", "trade_viewers", "", false); w.Code != 404 {
		t.Fatalf("bob's replay for alice %d", w.Code)
	}
	w := do(t, a, "GET", "/hub/api/replays/r1", "alice", "trade_viewers", "", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"trades":[{"pair":"BTC/USDT"`) {
		t.Fatalf("own done replay with trades: %d %s", w.Code, w.Body)
	}
	a.Replays = nil
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", `{"bot":"trend","from":"a","to":"b"}`, true); w.Code != 503 {
		t.Fatalf("off %d", w.Code)
	}
}

func TestNewBotNeedsAReplay(t *testing.T) {
	a, st := setup(t)
	fr := &fakeReplays{passed: map[string]bool{}}
	a.Replays = fr
	body := `{"strategy":"EmaCross","strategy_file":"EmaCross.py","pairs":["BTC/USDT"],"stake_currency":"USDT"}`
	w := do(t, a, "PUT", "/hub/api/bots/fresh", "alice", "trade_operators", body, true)
	if w.Code != 412 || len(st.writes) != 0 {
		t.Fatalf("no replay: %d %s, writes %v", w.Code, w.Body, st.writes)
	}
	var out struct {
		Settings string `json:"settings"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	// The draft replay of the same fields resolves to the same fingerprint.
	draft := `{"draft":` + body + `,"from":"2025-10-18","to":"2026-10-18"}`
	if w := do(t, a, "POST", "/hub/api/replays", "alice", "trade_operators", draft, true); w.Code != 202 {
		t.Fatalf("draft %d", w.Code)
	}
	if got := replay.SettingsHash(fr.got[0]); got != out.Settings {
		t.Fatalf("draft fingerprint %s, PUT wants %s", got, out.Settings)
	}
	fr.passed["alice|"+out.Settings] = true
	if w := do(t, a, "PUT", "/hub/api/bots/fresh", "bob", "trade_operators", body, true); w.Code != 412 {
		t.Fatalf("alice's replay must not let bob save: %d", w.Code)
	}
	if w := do(t, a, "PUT", "/hub/api/bots/fresh", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("after the replay %d %s", w.Code, w.Body)
	}
	// Changing an existing bot is not gated.
	if w := do(t, a, "PUT", "/hub/api/bots/trend", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("edit %d %s", w.Code, w.Body)
	}
}
