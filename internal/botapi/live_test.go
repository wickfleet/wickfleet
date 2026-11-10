package botapi

import (
	"encoding/json"
	"strings"
	"testing"

	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

const liveBody = `{"strategy":"EmaCross","strategy_file":"EmaCross.py","pairs":["BTC/USDT"],"stake_currency":"USDT","dry_run":false}`

func liveHash(t *testing.T, a API) string {
	t.Helper()
	fr := &fakeReplays{passed: map[string]bool{}}
	a.Replays = fr
	w := do(t, a, "PUT", "/hub/api/bots/live", "alice", "trade_operators", liveBody, true)
	var out struct {
		Settings string `json:"settings"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Settings == "" {
		t.Fatalf("no settings hash: %d %s", w.Code, w.Body)
	}
	return out.Settings
}

func TestLiveRefusedOnADryInstance(t *testing.T) {
	a, st := setup(t)
	a.Replays = &fakeReplays{passed: map[string]bool{}}
	w := do(t, a, "PUT", "/hub/api/bots/live", "alice", "trade_operators", liveBody, true)
	if w.Code < 400 || len(st.writes) != 0 || !strings.Contains(w.Body.String(), "live trading is not allowed") {
		t.Fatalf("dry instance: %d %s, writes %v", w.Code, w.Body, st.writes)
	}
}

func TestLiveNeedsReplay(t *testing.T) {
	a, st := setup(t)
	a.Live = true
	w := do(t, a, "PUT", "/hub/api/bots/live", "alice", "trade_operators", liveBody, true)
	if w.Code != 412 || len(st.writes) != 0 {
		t.Fatalf("live without replay: %d %s", w.Code, w.Body)
	}
}

func TestLiveNeedsAPassingVerdict(t *testing.T) {
	a, st := setup(t)
	a.Live = true
	hash := liveHash(t, a)
	key := "alice|" + hash
	cases := []struct {
		name   string
		passed bool
		robust *replay.Robustness
		want   int
	}{
		{"bias checks only, no verdict", true, nil, 412},
		{"verdict fails", true, &replay.Robustness{Pass: false, Reasons: []string{"loses vs hold"}}, 412},
		{"verdict passes", true, &replay.Robustness{Pass: true}, 202},
	}
	for _, c := range cases {
		fr := &fakeReplays{passed: map[string]bool{key: c.passed}, robust: map[string]*replay.Robustness{key: c.robust}}
		a.Replays = fr
		w := do(t, a, "PUT", "/hub/api/bots/live", "alice", "trade_operators", liveBody, true)
		if w.Code != c.want {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	if !strings.Contains(st.writes["bots/live.yaml"], "dry_run: false") {
		t.Fatalf("live bot file: %q", st.writes["bots/live.yaml"])
	}
}

func TestDryBotUnchangedOnALiveInstance(t *testing.T) {
	a, _ := setup(t)
	a.Live = true
	body := `{"strategy":"EmaCross","strategy_file":"EmaCross.py","pairs":["BTC/USDT"],"stake_currency":"USDT"}`
	if w := do(t, a, "PUT", "/hub/api/bots/trend", "alice", "trade_operators", body, true); w.Code != 202 {
		t.Fatalf("dry edit on a live instance: %d %s", w.Code, w.Body)
	}
}

func TestListingCarriesNoKeys(t *testing.T) {
	a, _ := setup(t)
	f := false
	a.Bots = func() []registry.Bot { return []registry.Bot{{Name: "live", Owner: "alice", DryRun: &f}} }
	w := do(t, a, "GET", "/hub/api/bots", "alice", "trade_operators", "", false)
	if w.Code != 200 || strings.Contains(strings.ToLower(w.Body.String()), "secret") || strings.Contains(w.Body.String(), `"key"`) {
		t.Fatalf("listing: %d %s", w.Code, w.Body)
	}
}
