// Package risk is the live cross-bot risk view: capital at stake, open
// trades, exposure per pair and per coin, drawdown against the worst so far,
// stoploss distance and pair overlap. Read from the bots' REST APIs only.
package risk

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"wickfleet/internal/authz"
	"wickfleet/internal/freqtrade"
	"wickfleet/internal/httpx"
	"wickfleet/internal/registry"
)

// Source reads one bot.
type Source interface {
	Status(context.Context) ([]freqtrade.OpenTrade, error)
	Profit(context.Context) (freqtrade.Profit, error)
	Balance(context.Context) (freqtrade.Balance, error)
}

// Bot is one bot's live risk. Money fields are nil for viewers and for a bot
// that did not answer. A failing bot carries Error, never zeros.
type Bot struct {
	Name           string   `json:"name"`
	Owner          string   `json:"owner"`
	Dry            bool     `json:"dry"`
	Stake          string   `json:"stake"`
	OpenTrades     int      `json:"open_trades"`
	MaxOpenTrades  int      `json:"max_open_trades"`
	AtStake        *float64 `json:"at_stake,omitempty"` // stake of the open trades
	Value          *float64 `json:"value,omitempty"`    // wallet total
	DrawdownPct    float64  `json:"drawdown_pct"`       // from the last peak, closed trades
	MaxDrawdownPct float64  `json:"max_drawdown_pct"`   // worst so far, closed trades
	OfWorstPct     *float64 `json:"of_worst_pct,omitempty"`
	Error          string   `json:"error,omitempty"`

	bot registry.Bot
}

// Position is one open trade.
type Position struct {
	Bot         string   `json:"bot"`
	Owner       string   `json:"owner"`
	Dry         bool     `json:"dry"`
	Stake       string   `json:"stake"`
	Pair        string   `json:"pair"`
	Coin        string   `json:"coin"`
	Amount      *float64 `json:"amount,omitempty"`
	AtStake     *float64 `json:"at_stake,omitempty"`
	Value       *float64 `json:"value,omitempty"` // amount at the current rate
	ProfitPct   float64  `json:"profit_pct"`
	StopDistPct *float64 `json:"stop_dist_pct,omitempty"` // negative: the stop is below the price
	OpenDate    string   `json:"open_date,omitempty"`
}

// Totals sums one kind (dry or live) in one stake currency.
type Totals struct {
	Dry        bool     `json:"dry"`
	Stake      string   `json:"stake"`
	Bots       int      `json:"bots"`
	OpenTrades int      `json:"open_trades"`
	AtStake    *float64 `json:"at_stake"`
	Value      *float64 `json:"value"`
}

// Pair is the exposure to one pair, dry and live apart.
type Pair struct {
	Pair     string   `json:"pair"`
	Dry      bool     `json:"dry"`
	Stake    string   `json:"stake"`
	Bots     []string `json:"bots"`
	Trades   int      `json:"trades"`
	AtStake  *float64 `json:"at_stake"`
	Value    *float64 `json:"value"`
	SharePct *float64 `json:"share_pct"` // of everything at stake in this currency and kind
}

// Coin is the exposure to one base coin across pairs and stake currencies.
// The amount sums, money stays per currency.
type Coin struct {
	Coin    string             `json:"coin"`
	Dry     bool               `json:"dry"`
	Bots    []string           `json:"bots"`
	Trades  int                `json:"trades"`
	Amount  float64            `json:"amount"`
	AtStake map[string]float64 `json:"at_stake"`
	Value   map[string]float64 `json:"value"`
}

// Overlap is a configured pair that more than one bot trades.
type Overlap struct {
	Pair string   `json:"pair"`
	Bots []string `json:"bots"`
	Open []string `json:"open"` // bots holding it now
}

// Report is what /hub/api/risk returns.
type Report struct {
	At        time.Time  `json:"at"`
	Redacted  bool       `json:"redacted"`
	Bots      []Bot      `json:"bots"`
	Positions []Position `json:"positions"`
	Totals    []Totals   `json:"totals,omitempty"`
	Pairs     []Pair     `json:"pairs,omitempty"`
	Coins     []Coin     `json:"coins,omitempty"`
	Overlap   []Overlap  `json:"overlap"`
}

// Snapshot is every bot read once. Build cuts a view from it.
type Snapshot struct {
	At        time.Time
	Bots      []Bot
	Positions []Position
}

func f(v float64) *float64 { return &v }

// read gets one bot. Any failing call fails the bot as a whole.
func read(ctx context.Context, b registry.Bot, src Source) (Bot, []Position) {
	out := Bot{Name: b.Name, Owner: b.Owner, Dry: b.Dry(), Stake: b.StakeCurrency, MaxOpenTrades: b.MaxOpenTrades, bot: b}
	open, err := src.Status(ctx)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	p, err := src.Profit(ctx)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	bal, err := src.Balance(ctx)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	if bal.Stake != "" {
		out.Stake = bal.Stake
	}
	out.Value = f(bal.Total)
	out.DrawdownPct, out.MaxDrawdownPct = p.CurrentDrawdown*100, p.MaxDrawdown*100
	if p.MaxDrawdown > 0 {
		out.OfWorstPct = f(p.CurrentDrawdown / p.MaxDrawdown * 100)
	}
	var at float64
	pos := make([]Position, 0, len(open))
	for _, t := range open {
		at += t.StakeAmount
		ps := Position{Bot: b.Name, Owner: b.Owner, Dry: out.Dry, Stake: out.Stake, Pair: t.Pair, Coin: t.Base,
			Amount: f(t.Amount), AtStake: f(t.StakeAmount), Value: f(t.Amount * t.CurrentRate),
			ProfitPct: t.ProfitRatio * 100, OpenDate: t.OpenDate}
		switch {
		case t.StopDistRatio != nil:
			ps.StopDistPct = f(*t.StopDistRatio * 100)
		case t.StopLossAbs != nil && t.CurrentRate > 0:
			ps.StopDistPct = f((*t.StopLossAbs - t.CurrentRate) / t.CurrentRate * 100)
		}
		pos = append(pos, ps)
	}
	out.OpenTrades, out.AtStake = len(open), f(at)
	return out, pos
}

// Collect reads all bots in parallel.
func Collect(ctx context.Context, bots []registry.Bot, src func(registry.Bot) Source) Snapshot {
	rows := make([]Bot, len(bots))
	pos := make([][]Position, len(bots))
	var wg sync.WaitGroup
	for i, b := range bots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i], pos[i] = read(ctx, b, src(b))
		}()
	}
	wg.Wait()
	s := Snapshot{At: time.Now().UTC(), Bots: rows}
	for _, p := range pos {
		s.Positions = append(s.Positions, p...)
	}
	return s
}

// Build keeps the bots show allows and sums them. Redacted drops every money
// figure and the exposure lists that are made of money.
func Build(s Snapshot, show func(registry.Bot) bool, redacted bool) Report {
	r := Report{At: s.At, Redacted: redacted, Bots: []Bot{}, Positions: []Position{}, Overlap: []Overlap{}}
	seen := map[string]bool{}
	for _, b := range s.Bots {
		if show(b.bot) {
			r.Bots = append(r.Bots, b)
			seen[b.Name] = true
		}
	}
	for _, p := range s.Positions {
		if seen[p.Bot] {
			r.Positions = append(r.Positions, p)
		}
	}
	sort.Slice(r.Bots, func(i, j int) bool { return r.Bots[i].Name < r.Bots[j].Name })
	sort.SliceStable(r.Positions, func(i, j int) bool { return r.Positions[i].Bot < r.Positions[j].Bot })
	r.Overlap = overlap(r.Bots, r.Positions)
	if redacted {
		for i := range r.Bots {
			r.Bots[i].AtStake, r.Bots[i].Value = nil, nil
		}
		for i := range r.Positions {
			r.Positions[i].Amount, r.Positions[i].AtStake, r.Positions[i].Value = nil, nil, nil
		}
		return r
	}
	r.Totals, r.Pairs, r.Coins = sums(r.Bots, r.Positions)
	return r
}

type kindKey struct {
	dry   bool
	stake string
}

func sums(bots []Bot, pos []Position) ([]Totals, []Pair, []Coin) {
	tot := map[kindKey]*Totals{}
	for _, b := range bots {
		if b.Error != "" {
			continue
		}
		k := kindKey{b.Dry, b.Stake}
		t := tot[k]
		if t == nil {
			t = &Totals{Dry: b.Dry, Stake: b.Stake, AtStake: f(0), Value: f(0)}
			tot[k] = t
		}
		t.Bots++
		t.OpenTrades += b.OpenTrades
		*t.AtStake += *b.AtStake
		*t.Value += *b.Value
	}
	type pairKey struct {
		pair string
		dry  bool
	}
	pairs := map[pairKey]*Pair{}
	type coinKey struct {
		coin string
		dry  bool
	}
	coins := map[coinKey]*Coin{}
	for _, p := range pos {
		pk := pairKey{p.Pair, p.Dry}
		pe := pairs[pk]
		if pe == nil {
			pe = &Pair{Pair: p.Pair, Dry: p.Dry, Stake: p.Stake, AtStake: f(0), Value: f(0)}
			pairs[pk] = pe
		}
		pe.Bots = addName(pe.Bots, p.Bot)
		pe.Trades++
		*pe.AtStake += *p.AtStake
		*pe.Value += *p.Value
		ck := coinKey{p.Coin, p.Dry}
		ce := coins[ck]
		if ce == nil {
			ce = &Coin{Coin: p.Coin, Dry: p.Dry, AtStake: map[string]float64{}, Value: map[string]float64{}}
			coins[ck] = ce
		}
		ce.Bots = addName(ce.Bots, p.Bot)
		ce.Trades++
		ce.Amount += *p.Amount
		ce.AtStake[p.Stake] += *p.AtStake
		ce.Value[p.Stake] += *p.Value
	}
	outT := make([]Totals, 0, len(tot))
	for _, t := range tot {
		outT = append(outT, *t)
	}
	sort.Slice(outT, func(i, j int) bool {
		if outT[i].Dry != outT[j].Dry {
			return !outT[i].Dry
		}
		return outT[i].Stake < outT[j].Stake
	})
	outP := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		if t := tot[kindKey{p.Dry, p.Stake}]; t != nil && *t.AtStake > 0 {
			p.SharePct = f(*p.AtStake / *t.AtStake * 100)
		}
		outP = append(outP, *p)
	}
	sort.Slice(outP, func(i, j int) bool { return *outP[i].AtStake > *outP[j].AtStake })
	outC := make([]Coin, 0, len(coins))
	for _, c := range coins {
		outC = append(outC, *c)
	}
	sort.Slice(outC, func(i, j int) bool {
		if outC[i].Trades != outC[j].Trades {
			return outC[i].Trades > outC[j].Trades
		}
		return outC[i].Coin < outC[j].Coin
	})
	return outT, outP, outC
}

// overlap lists configured pairs on two or more bots.
func overlap(bots []Bot, pos []Position) []Overlap {
	by := map[string][]string{}
	for _, b := range bots {
		for _, p := range b.bot.Pairs {
			by[p] = addName(by[p], b.Name)
		}
	}
	open := map[string][]string{}
	for _, p := range pos {
		open[p.Pair] = addName(open[p.Pair], p.Bot)
	}
	out := []Overlap{}
	for p, names := range by {
		if len(names) > 1 {
			o := open[p]
			if o == nil {
				o = []string{}
			}
			out = append(out, Overlap{Pair: p, Bots: names, Open: o})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Bots) != len(out[j].Bots) {
			return len(out[i].Bots) > len(out[j].Bots)
		}
		return out[i].Pair < out[j].Pair
	})
	return out
}

func addName(names []string, n string) []string {
	for _, x := range names {
		if x == n {
			return names
		}
	}
	names = append(names, n)
	sort.Strings(names)
	return names
}

// API serves GET /hub/api/risk from one cached read of the whole fleet.
type API struct {
	Bots     func() []registry.Bot
	Source   func(registry.Bot) Source
	CacheFor time.Duration

	mu    sync.Mutex
	snap  Snapshot
	taken time.Time
}

// Routes registers the risk view.
func (a *API) Routes(mux httpx.Mux) {
	mux.HandleFunc("GET /hub/api/risk", a.get)
}

func (a *API) snapshot(ctx context.Context) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.taken.IsZero() && time.Since(a.taken) < a.CacheFor {
		return a.snap
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a.snap, a.taken = Collect(ctx, a.Bots(), a.Source), time.Now()
	return a.snap
}

// Admins see the fleet, others their own bots. Viewers see no money.
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	id := authz.FromHeaders(r.Header)
	if id.Role == authz.None {
		httpx.Fail(w, http.StatusForbidden, "no wickfleet role")
		return
	}
	show := func(b registry.Bot) bool { return id.Role == authz.Admin || b.Owner == id.User }
	httpx.JSON(w, http.StatusOK, Build(a.snapshot(r.Context()), show, id.Role < authz.Operator))
}
