package risk

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wickfleet/internal/freqtrade"
	"wickfleet/internal/registry"
)

type fake struct {
	open   []freqtrade.OpenTrade
	profit freqtrade.Profit
	bal    freqtrade.Balance
	err    error
}

func (f fake) Status(context.Context) ([]freqtrade.OpenTrade, error) { return f.open, f.err }
func (f fake) Profit(context.Context) (freqtrade.Profit, error)      { return f.profit, f.err }
func (f fake) Balance(context.Context) (freqtrade.Balance, error)    { return f.bal, f.err }

func fp(v float64) *float64 { return &v }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

var live = false

// fleet: alice runs two dry USDT bots and one dry CAD bot that all hold BTC,
// bob one live USDT bot holding BTC too and one bot that does not answer.
func fleet() ([]registry.Bot, map[string]fake) {
	bots := []registry.Bot{
		{Name: "a1", Owner: "alice", StakeCurrency: "USDT", MaxOpenTrades: 3, Pairs: []string{"BTC/USDT", "ETH/USDT"}},
		{Name: "a2", Owner: "alice", StakeCurrency: "USDT", MaxOpenTrades: 2, Pairs: []string{"BTC/USDT", "SOL/USDT"}},
		{Name: "a3", Owner: "alice", StakeCurrency: "CAD", MaxOpenTrades: 1, Pairs: []string{"BTC/CAD"}},
		{Name: "b1", Owner: "bob", StakeCurrency: "USDT", MaxOpenTrades: 1, Pairs: []string{"BTC/USDT"}, DryRun: &live},
		{Name: "b2", Owner: "bob", StakeCurrency: "USDT", Pairs: []string{"ETH/USDT"}},
	}
	btc := func(stake string, amount, atStake, rate float64, dist *float64) freqtrade.OpenTrade {
		return freqtrade.OpenTrade{Pair: "BTC/" + stake, Base: "BTC", Quote: stake, Amount: amount, StakeAmount: atStake,
			CurrentRate: rate, StopLossAbs: fp(rate * 0.9), StopDistRatio: dist}
	}
	src := map[string]fake{
		"a1": {open: []freqtrade.OpenTrade{btc("USDT", 0.01, 600, 61000, fp(-0.1)),
			{Pair: "ETH/USDT", Base: "ETH", Quote: "USDT", Amount: 0.1, StakeAmount: 300, CurrentRate: 3100, StopDistRatio: fp(-0.05)}},
			profit: freqtrade.Profit{MaxDrawdown: 0.2, CurrentDrawdown: 0.05}, bal: freqtrade.Balance{Total: 1010, Stake: "USDT"}},
		// No distance from freqtrade: derived from the stop and the current rate.
		"a2": {open: []freqtrade.OpenTrade{btc("USDT", 0.02, 1200, 61000, nil)},
			profit: freqtrade.Profit{MaxDrawdown: 0, CurrentDrawdown: 0}, bal: freqtrade.Balance{Total: 990, Stake: "USDT"}},
		"a3": {open: []freqtrade.OpenTrade{btc("CAD", 0.005, 400, 84000, fp(-0.08))},
			profit: freqtrade.Profit{MaxDrawdown: 0.1, CurrentDrawdown: 0.1}, bal: freqtrade.Balance{Total: 1000, Stake: "CAD"}},
		"b1": {open: []freqtrade.OpenTrade{btc("USDT", 0.03, 1800, 61000, fp(-0.02))},
			profit: freqtrade.Profit{MaxDrawdown: 0.3, CurrentDrawdown: 0.15}, bal: freqtrade.Balance{Total: 2100, Stake: "USDT"}},
		"b2": {err: errors.New("profit: HTTP 502")},
	}
	return bots, src
}

func snapshot(t *testing.T) Snapshot {
	t.Helper()
	bots, src := fleet()
	return Collect(context.Background(), bots, func(b registry.Bot) Source { return src[b.Name] })
}

func TestBuildAggregates(t *testing.T) {
	r := Build(snapshot(t), func(registry.Bot) bool { return true }, false)
	byName := map[string]Bot{}
	for _, b := range r.Bots {
		byName[b.Name] = b
	}
	if b := byName["b2"]; b.Error == "" || b.AtStake != nil {
		t.Fatalf("a failing bot is reported as failing, never as zero: %+v", b)
	}
	if b := byName["a1"]; b.OpenTrades != 2 || *b.AtStake != 900 || b.DrawdownPct != 5 || b.MaxDrawdownPct != 20 || *b.OfWorstPct != 25 {
		t.Fatalf("a1 %+v", b)
	}
	if b := byName["a2"]; b.OfWorstPct != nil {
		t.Fatalf("no historical drawdown: share of worst must be unknown, got %v", *b.OfWorstPct)
	}
	// Dry and live never in one sum, currencies apart.
	want := map[string][2]float64{"dry USDT": {3, 2100}, "dry CAD": {1, 400}, "live USDT": {1, 1800}}
	if len(r.Totals) != len(want) {
		t.Fatalf("totals %+v", r.Totals)
	}
	for _, tot := range r.Totals {
		k := map[bool]string{true: "dry ", false: "live "}[tot.Dry] + tot.Stake
		if w, ok := want[k]; !ok || float64(tot.OpenTrades) != w[0] || *tot.AtStake != w[1] {
			t.Errorf("%s: %+v, want %v", k, tot, w)
		}
	}
	// BTC on three dry bots in two currencies is one line, the live bot's BTC is another.
	var dryBTC *Coin
	for i, c := range r.Coins {
		if c.Coin == "BTC" && c.Dry {
			if dryBTC != nil {
				t.Fatal("dry BTC listed twice")
			}
			dryBTC = &r.Coins[i]
		}
	}
	if dryBTC == nil || !near(dryBTC.Amount, 0.035) || strings.Join(dryBTC.Bots, ",") != "a1,a2,a3" ||
		dryBTC.AtStake["USDT"] != 1800 || dryBTC.AtStake["CAD"] != 400 || !near(dryBTC.Value["USDT"], 1830) {
		t.Fatalf("dry BTC %+v", dryBTC)
	}
	var pairBTC *Pair
	for i, p := range r.Pairs {
		if p.Pair == "BTC/USDT" && p.Dry {
			pairBTC = &r.Pairs[i]
		}
	}
	// 1800 of the 2100 dry USDT at stake sits in BTC/USDT.
	if pairBTC == nil || pairBTC.Trades != 2 || !near(*pairBTC.SharePct, 1800.0/2100*100) {
		t.Fatalf("pair %+v", pairBTC)
	}
	stops := map[string]float64{}
	for _, p := range r.Positions {
		if p.StopDistPct == nil {
			t.Fatalf("no stop distance on %+v", p)
		}
		stops[p.Bot+" "+p.Pair] = *p.StopDistPct
	}
	if !near(stops["a1 BTC/USDT"], -10) || !near(stops["a2 BTC/USDT"], -10) || !near(stops["b1 BTC/USDT"], -2) {
		t.Fatalf("stops %v", stops)
	}
	// Overlap is from the configured pairs: BTC/USDT on a1, a2, b1 and ETH/USDT on a1 and b2.
	ov := map[string]string{}
	for _, o := range r.Overlap {
		ov[o.Pair] = strings.Join(o.Bots, ",") + " open:" + strings.Join(o.Open, ",")
	}
	if ov["BTC/USDT"] != "a1,a2,b1 open:a1,a2,b1" || ov["ETH/USDT"] != "a1,b2 open:a1" || len(ov) != 2 {
		t.Fatalf("overlap %v", ov)
	}
}

func TestRedactedHasNoMoney(t *testing.T) {
	r := Build(snapshot(t), func(b registry.Bot) bool { return b.Owner == "alice" }, true)
	raw, _ := json.Marshal(r)
	s := string(raw)
	for _, k := range []string{`"at_stake"`, `"value"`, `"amount"`, `"totals"`, `"pairs"`, `"coins"`} {
		if strings.Contains(s, k) {
			t.Errorf("redacted report carries %s: %s", k, s)
		}
	}
	if !r.Redacted || len(r.Bots) != 3 || len(r.Positions) != 4 || r.Bots[0].DrawdownPct == 0 && r.Bots[2].DrawdownPct == 0 {
		t.Fatalf("redacted keeps counts and drawdown: %+v", r)
	}
	// Control: the same view unredacted carries the money.
	if raw, _ := json.Marshal(Build(snapshot(t), func(b registry.Bot) bool { return b.Owner == "alice" }, false)); !strings.Contains(string(raw), `"at_stake"`) {
		t.Fatal("control: unredacted report has no at_stake, the check above proves nothing")
	}
}

// get calls the API as user with groups. An empty user sends no identity.
func get(t *testing.T, a *API, user, groups string) (int, Report) {
	t.Helper()
	mux := http.NewServeMux()
	a.Routes(mux)
	r := httptest.NewRequest("GET", "/hub/api/risk", nil)
	if user != "" {
		r.Header.Set("X-Auth-Request-User", user)
		r.Header.Set("X-Auth-Request-Groups", groups)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var rep Report
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	return w.Code, rep
}

func TestAPIRoles(t *testing.T) {
	bots, src := fleet()
	var calls atomic.Int32
	a := &API{Bots: func() []registry.Bot { return bots }, CacheFor: time.Minute,
		Source: func(b registry.Bot) Source { calls.Add(1); return src[b.Name] }}
	names := func(r Report) string {
		var out []string
		for _, b := range r.Bots {
			out = append(out, b.Name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		user, groups string
		code         int
		bots         string
		redacted     bool
	}{
		{"", "", 403, "", false},
		{"root", "trade_admins", 200, "a1,a2,a3,b1,b2", false},
		{"alice", "trade_operators", 200, "a1,a2,a3", false},
		{"bob", "trade_viewers", 200, "b1,b2", true},
		{"carol", "trade_operators", 200, "", false},
	} {
		code, r := get(t, a, c.user, c.groups)
		if code != c.code || names(r) != c.bots || r.Redacted != c.redacted {
			t.Errorf("%s %s: %d %q redacted=%v, want %d %q %v", c.user, c.groups, code, names(r), r.Redacted, c.code, c.bots, c.redacted)
		}
		if c.redacted && (r.Totals != nil || r.Bots[0].AtStake != nil) {
			t.Errorf("%s: viewer sees money %+v", c.user, r)
		}
	}
	if int(calls.Load()) != len(bots) {
		t.Fatalf("one collection serves every caller within CacheFor: %d source calls, want %d", calls.Load(), len(bots))
	}
}
