// Package compare backtests several bots over one window and lines their
// results up. Each bot is an ordinary replay job. A comparison only queues
// them, one at a time, and reads their results when they are done.
package compare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"wickfleet/internal/atomicfile"
	"wickfleet/internal/registry"
	"wickfleet/internal/replay"
)

// Replays is the slice of the replay runner a comparison needs.
type Replays interface {
	Submit(owner string, b registry.Bot, from, to string) (*replay.Job, error)
	Get(id string) (replay.Job, bool)
	Trades(id string) ([]replay.Trade, error)
}

// Comparison states. A row carries its replay's state, or waiting before it is queued.
const (
	Running = "running"
	Done    = "done"
	Failed  = "failed"
	Waiting = "waiting"
)

// Row is one bot of a comparison.
type Row struct {
	Bot      string   `json:"bot"`
	Owner    string   `json:"owner"`
	Exchange string   `json:"exchange"`
	Strategy string   `json:"strategy"`
	Dry      bool     `json:"dry"`
	Replay   string   `json:"replay,omitempty"`
	State    string   `json:"state"`
	Note     string   `json:"note,omitempty"`
	Error    string   `json:"error,omitempty"`
	Metrics  *Metrics `json:"metrics,omitempty"`
	Equity   []Point  `json:"equity,omitempty"`
}

func (r Row) finished() bool { return r.State == replay.Done || r.State == replay.Failed }

// Comparison is several bots replayed over the same window.
type Comparison struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Finished time.Time `json:"finished,omitzero"`
	Rows     []Row     `json:"bots"`
	Redacted bool      `json:"redacted,omitempty"` // money left out, for viewers

	bots []registry.Bot
}

// Redact drops every money figure: balances, absolute profit and equity in
// the stake currency. Percentages, ratios, counts and the return curve stay.
func (c Comparison) Redact() Comparison {
	rows := make([]Row, len(c.Rows))
	for i, r := range c.Rows {
		if r.Metrics != nil {
			m := *r.Metrics
			m.StartBalance, m.FinalBalance, m.ProfitAbs = nil, nil, nil
			r.Metrics = &m
		}
		if r.Equity != nil {
			eq := make([]Point, len(r.Equity))
			for k, p := range r.Equity {
				eq[k] = Point{Date: p.Date, ReturnPct: p.ReturnPct}
			}
			r.Equity = eq
		}
		rows[i] = r
	}
	c.Rows, c.Redacted = rows, true
	return c
}

// Brief is the comparison without its equity curves, for listings.
func (c Comparison) Brief() Comparison {
	rows := make([]Row, len(c.Rows))
	for i, r := range c.Rows {
		r.Equity = nil
		rows[i] = r
	}
	c.Rows = rows
	return c
}

// Service runs comparisons. Start it once, Submit from anywhere.
type Service struct {
	Replays   Replays // nil: replay is off, earlier comparisons are still listed
	Dir       string  // one <id>.json per comparison
	Poll      time.Duration
	MaxActive int // comparisons running at once
	MaxBots   int
	Keep      int // finished comparisons kept
	Now       func() time.Time
	Log       func(format string, args ...any)

	mu    sync.Mutex
	items map[string]*Comparison
	ctx   context.Context
	wg    sync.WaitGroup
}

// ErrBusy means too many comparisons are running.
var ErrBusy = errors.New("too many comparisons running, try again later")

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Start loads earlier comparisons. One that a restart interrupted is failed:
// its replays were failed by the replay runner's own restart.
func (s *Service) Start(ctx context.Context) error {
	if s.Poll <= 0 {
		s.Poll = 2 * time.Second
	}
	if s.MaxActive <= 0 {
		s.MaxActive = 2
	}
	if s.MaxBots <= 0 {
		s.MaxBots = 12
	}
	if s.Keep <= 0 {
		s.Keep = 50
	}
	s.ctx = ctx
	s.items = map[string]*Comparison{}
	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c Comparison
		if json.Unmarshal(raw, &c) != nil || c.ID == "" {
			continue
		}
		if c.State == Running {
			c.State, c.Error, c.Finished = Failed, "the hub restarted while this comparison ran", s.now()
			for i := range c.Rows {
				if !c.Rows[i].finished() {
					c.Rows[i].State, c.Rows[i].Note = replay.Failed, ""
				}
			}
			s.save(&c)
		}
		s.items[c.ID] = &c
	}
	return nil
}

// Wait returns once every comparison started has stopped.
func (s *Service) Wait() { s.wg.Wait() }

// Submit starts a comparison of bots over [from, to], dates as YYYY-MM-DD.
func (s *Service) Submit(owner string, bots []registry.Bot, from, to string) (*Comparison, error) {
	if s.Replays == nil {
		return nil, errors.New("replay is off on this instance")
	}
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	switch {
	case !f.Before(t):
		return nil, errors.New("from must be before to")
	case t.After(s.now().UTC().Truncate(24 * time.Hour)):
		return nil, errors.New("to cannot be in the future")
	case len(bots) < 2:
		return nil, errors.New("pick two or more bots")
	case len(bots) > s.MaxBots:
		return nil, fmt.Errorf("at most %d bots per comparison", s.MaxBots)
	}
	c := &Comparison{ID: newID(s.now()), Owner: owner, From: from, To: to, State: Running, Created: s.now(), bots: bots}
	for _, b := range bots {
		c.Rows = append(c.Rows, Row{Bot: b.Name, Owner: b.Owner, Exchange: b.Exchange, Strategy: b.Strategy, Dry: b.Dry(), State: Waiting})
	}
	s.mu.Lock()
	active := 0
	for _, o := range s.items {
		if o.State == Running {
			active++
		}
	}
	if active >= s.MaxActive {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.items[c.ID] = c
	s.mu.Unlock()
	s.save(c)
	cp := s.copyOf(c)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.run(c) }()
	return &cp, nil
}

func newID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "c" + t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Get returns a copy.
func (s *Service) Get(id string) (Comparison, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.items[id]
	if !ok {
		return Comparison{}, false
	}
	return s.copyLocked(c), true
}

// List returns copies, newest first.
func (s *Service) List() []Comparison {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Comparison, 0, len(s.items))
	for _, c := range s.items {
		out = append(out, s.copyLocked(c))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out
}

func (s *Service) copyOf(c *Comparison) Comparison {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyLocked(c)
}

func (s *Service) copyLocked(c *Comparison) Comparison {
	cp := *c
	cp.Rows = append([]Row(nil), c.Rows...)
	cp.bots = nil
	return cp
}

func (s *Service) update(c *Comparison, f func()) {
	s.mu.Lock()
	f()
	s.mu.Unlock()
	s.save(c)
}

func (s *Service) save(c *Comparison) {
	s.mu.Lock()
	raw, err := json.Marshal(c)
	s.mu.Unlock()
	if err != nil {
		return
	}
	if err := atomicfile.Write(filepath.Join(s.Dir, c.ID+".json"), raw, 0o640); err != nil {
		s.logf("comparison %s: %v", c.ID, err)
	}
}

// sleep waits one poll, false once the service is stopping.
func (s *Service) sleep() bool {
	select {
	case <-s.ctx.Done():
		return false
	case <-time.After(s.Poll):
		return true
	}
}

// run queues the bots one after another: the next only once the one before
// has left the queue, so a comparison holds at most one queue place and the
// replay runner's own limits (one replay at a time, one per exchange) apply.
func (s *Service) run(c *Comparison) {
	for i := range c.Rows {
		if !s.submit(c, i) {
			s.stop(c)
			return
		}
	}
	for {
		open := false
		for i := range c.Rows {
			s.refresh(c, i)
			open = open || !c.Rows[i].finished()
		}
		if !open {
			break
		}
		if !s.sleep() {
			s.stop(c)
			return
		}
	}
	s.update(c, func() { c.State, c.Finished = Done, s.now() })
	s.prune()
}

// submit queues row i and waits until its replay has left the queue.
func (s *Service) submit(c *Comparison, i int) bool {
	b := c.bots[i]
	for {
		j, err := s.Replays.Submit(c.Owner, b, c.From, c.To)
		if errors.Is(err, replay.ErrBusy) {
			s.update(c, func() { c.Rows[i].Note = "waiting for room in the replay queue" })
			if !s.sleep() {
				return false
			}
			continue
		}
		if err != nil {
			s.update(c, func() { c.Rows[i].State, c.Rows[i].Error, c.Rows[i].Note = replay.Failed, err.Error(), "" })
			return true
		}
		s.update(c, func() { c.Rows[i].Replay, c.Rows[i].State, c.Rows[i].Note = j.ID, j.State, "" })
		break
	}
	for {
		s.refresh(c, i)
		if c.Rows[i].State != replay.Queued {
			return true
		}
		if !s.sleep() {
			return false
		}
	}
}

// refresh copies the replay's state into row i and measures it once done.
func (s *Service) refresh(c *Comparison, i int) {
	row := c.Rows[i]
	if row.finished() || row.Replay == "" {
		return
	}
	j, ok := s.Replays.Get(row.Replay)
	if !ok {
		s.update(c, func() { c.Rows[i].State, c.Rows[i].Error = replay.Failed, "the replay is gone" })
		return
	}
	switch j.State {
	case replay.Done:
		trades, err := s.Replays.Trades(j.ID)
		if err != nil {
			s.update(c, func() { c.Rows[i].State, c.Rows[i].Error = replay.Failed, "replay trades: "+err.Error() })
			return
		}
		m := Measure(j, trades)
		var start float64
		if j.Summary != nil {
			start = j.Summary.StartBalance
		}
		eq := Equity(start, trades, c.From, c.To)
		s.update(c, func() { c.Rows[i].State, c.Rows[i].Note, c.Rows[i].Metrics, c.Rows[i].Equity = replay.Done, "", &m, eq })
	case replay.Failed:
		s.update(c, func() { c.Rows[i].State, c.Rows[i].Note, c.Rows[i].Error = replay.Failed, "", j.Error })
	default:
		if row.State != j.State || row.Note != j.Note {
			s.update(c, func() { c.Rows[i].State, c.Rows[i].Note = j.State, j.Note })
		}
	}
}

// stop marks a comparison the hub stopped in the middle of.
func (s *Service) stop(c *Comparison) {
	s.update(c, func() { c.State, c.Error, c.Finished = Failed, "the hub stopped while this comparison ran", s.now() })
}

// prune keeps the newest Keep finished comparisons.
func (s *Service) prune() {
	kept := 0
	for _, c := range s.List() {
		if c.State == Running {
			continue
		}
		if kept++; kept <= s.Keep {
			continue
		}
		s.mu.Lock()
		delete(s.items, c.ID)
		s.mu.Unlock()
		_ = os.Remove(filepath.Join(s.Dir, c.ID+".json"))
	}
}
