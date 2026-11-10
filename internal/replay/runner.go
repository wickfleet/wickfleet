// Package replay backtests a bot over any date range in short-lived
// freqtrade containers, on a shared candle store the hub keeps filled.
package replay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wickfleet/internal/botconfig"
	"wickfleet/internal/docker"
	"wickfleet/internal/hostdir"
	"wickfleet/internal/registry"
)

// Engine is the Docker slice a job needs.
type Engine interface {
	Create(ctx context.Context, name string, s docker.Spec) (string, error)
	Pull(ctx context.Context, ref string) error
	Start(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (docker.State, error)
	Logs(ctx context.Context, id string, tail int) (string, error)
	Labeled(ctx context.Context, value string) ([]docker.Container, error)
}

// Config is instance-wide. Paths are host paths the hub sees at the same place.
type Config struct {
	Images       map[string]string // image key -> pinned reference
	Strategies   string            // registry checkout's strategies/
	CandleDir    string            // <exchange>/ per exchange, shared by every job
	JobDir       string            // <job id>/ per job
	Network      string            // needs egress: downloads and market metadata
	BotUser      string
	Since        time.Time            // the store holds candles from here on
	SinceBy      map[string]time.Time // per exchange, lower case; overrides Since
	Memory       int64                // per job container; FreqAI jobs get FreqaiMem
	FreqaiMem    int64                //
	Timeout      time.Duration        // per replay container
	FetchTimeout time.Duration        // per store fetch; a first Kraken fill takes hours
	MaxQueued    int                  // per lane
	KeepJobs     int                  // per kind
	PracticeMem  int64                // per practice container; 0 = Memory
	Rules        Rules                // walk-forward and Monte Carlo pass rules; zero = defaults
}

// Job states.
const (
	Queued      = "queued"
	Downloading = "downloading"
	Testing     = "testing"
	Done        = "done"
	Failed      = "failed"
)

// Job is one replay, store fetch or practice run.
type Job struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"` // replay | fetch | practice | practice-fetch
	Owner    string      `json:"owner"`
	Bot      string      `json:"bot"`
	Exchange string      `json:"exchange"`
	Pairs    []string    `json:"pairs"`
	Strategy string      `json:"strategy,omitempty"`
	From     string      `json:"from,omitempty"`
	To       string      `json:"to,omitempty"`
	State    string      `json:"state"`
	Note     string      `json:"note,omitempty"`
	Error    string      `json:"error,omitempty"`
	Created  time.Time   `json:"created"`
	Started  time.Time   `json:"started,omitzero"`
	Finished time.Time   `json:"finished,omitzero"`
	Summary  *Summary    `json:"summary,omitempty"`
	Bias     *Bias       `json:"bias,omitempty"`
	Robust   *Robustness `json:"robust,omitempty"`
	Settings string      `json:"settings,omitempty"` // SettingsHash of the replayed bot

	bot registry.Bot
	tfs []string
	src []byte // strategy code given at submit; nil = the registry's file
}

// Runner queues and runs jobs. Start it once; Submit and Fetch from anywhere.
type Runner struct {
	Cfg    Config
	Docker Engine
	Log    func(format string, args ...any)
	Now    func() time.Time

	mu        sync.Mutex
	jobs      map[string]*Job
	replayQ   chan *Job
	fetchQ    chan *Job
	practiceQ chan *Job
	exMu      map[string]*sync.Mutex
	wg        sync.WaitGroup // lanes and running jobs, for Wait
}

// lane runs one job at a time of its kind; slot is its token.
type lane struct {
	q    chan *Job
	slot chan struct{}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Start loads earlier jobs, clears containers a restart orphaned and runs the
// replay, fetch and practice lanes until ctx ends.
func (r *Runner) Start(ctx context.Context) error {
	if err := r.init(ctx); err != nil {
		return err
	}
	for _, q := range []chan *Job{r.replayQ, r.fetchQ, r.practiceQ} {
		r.wg.Add(1)
		go func() { defer r.wg.Done(); r.dispatch(ctx, lane{q, make(chan struct{}, 1)}) }()
	}
	return nil
}

// Wait returns once Start's ctx has ended and every job it started is done.
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) init(ctx context.Context) error {
	if r.Cfg.MaxQueued <= 0 {
		r.Cfg.MaxQueued = 5
	}
	r.jobs = map[string]*Job{}
	r.replayQ = make(chan *Job, 64)
	r.fetchQ = make(chan *Job, 64)
	r.practiceQ = make(chan *Job, 64)
	r.exMu = map[string]*sync.Mutex{}
	if err := hostdir.Make(r.Cfg.JobDir); err != nil {
		return err
	}
	if err := hostdir.Make(r.Cfg.CandleDir); err != nil {
		return err
	}
	r.load()
	if cs, err := r.Docker.Labeled(ctx, "replay"); err == nil {
		for _, c := range cs {
			_ = r.Docker.Remove(ctx, c.ID)
		}
	}
	return nil
}

func (r *Runner) load() {
	dirs, _ := filepath.Glob(filepath.Join(r.Cfg.JobDir, "*", "job.json"))
	for _, f := range dirs {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var j Job
		if json.Unmarshal(raw, &j) != nil || j.ID == "" {
			continue
		}
		if j.State != Done && j.State != Failed {
			j.State, j.Error, j.Finished, j.Note = Failed, "the hub restarted while this job ran", r.now(), ""
			r.save(&j)
		}
		r.jobs[j.ID] = &j
	}
}

// ErrBusy means the lane's queue is full.
var ErrBusy = errors.New("too many jobs queued, try again later")

// Submit queues a replay of b over [from, to], dates as YYYY-MM-DD.
func (r *Runner) Submit(owner string, b registry.Bot, from, to string) (*Job, error) {
	return r.SubmitSource(owner, b, nil, from, to)
}

// SubmitSource is Submit with the strategy code given, for a bot whose file
// is not in the registry yet (a managed bot before its first commit).
func (r *Runner) SubmitSource(owner string, b registry.Bot, src []byte, from, to string) (*Job, error) {
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	today := r.now().UTC().Truncate(24 * time.Hour)
	switch {
	case !f.Before(t):
		return nil, errors.New("from must be before to")
	case f.Before(r.Cfg.SinceFor(b.Exchange)):
		return nil, fmt.Errorf("the %s candle store starts %s", b.Exchange, r.Cfg.SinceFor(b.Exchange).Format("2006-01-02"))
	case t.After(today):
		return nil, errors.New("to cannot be in the future")
	}
	if _, ok := r.Cfg.Images[b.Image]; !ok {
		return nil, fmt.Errorf("image %q is not allowed", b.Image)
	}
	given := src != nil
	if !given {
		if src, err = r.strategySource(b); err != nil {
			return nil, err
		}
	}
	tfs, err := Timeframes(b, src)
	if err != nil {
		return nil, err
	}
	j := &Job{Kind: "replay", Owner: owner, Bot: b.Name, Exchange: b.Exchange, Pairs: Pairs(b),
		Strategy: b.Strategy, From: from, To: to, Settings: SettingsHash(b), bot: b, tfs: tfs}
	if given {
		j.src = src
	}
	return j, r.enqueue(j, r.replayQ)
}

// SettingsHash fingerprints what a bot trades with; name and owner do not
// change a backtest, so a draft and the saved bot match.
func SettingsHash(b registry.Bot) string {
	b.Name, b.Owner = "", ""
	raw, _ := json.Marshal(b)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// Passed returns owner's newest finished replay of these settings within d.
func (r *Runner) Passed(owner, settings string, d time.Duration) (Job, bool) {
	cut := r.now().Add(-d)
	for _, j := range r.List() {
		if j.Kind == "replay" && j.Owner == owner && j.Settings == settings && j.State == Done && j.Finished.After(cut) && !j.Bias.Blocks() {
			return j, true
		}
	}
	return Job{}, false
}

// Fetch queues a store refresh for one exchange.
func (r *Runner) Fetch(exchange, image string, pairs, tfs []string) (*Job, error) {
	b := registry.Bot{Name: "fetch", Exchange: exchange, Pairs: pairs, Image: image, StakeCurrency: quote(pairs), Wallet: 1000, MaxOpenTrades: 1}
	j := &Job{Kind: "fetch", Owner: "hub", Bot: "", Exchange: exchange, Pairs: pairs, bot: b, tfs: tfs}
	return j, r.enqueue(j, r.fetchQ)
}

// FetchPending reports whether a store fetch for exchange is queued or running.
func (r *Runner) FetchPending(exchange string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.Kind == "fetch" && j.Exchange == exchange && j.State != Done && j.State != Failed {
			return true
		}
	}
	return false
}

func quote(pairs []string) string {
	for _, p := range pairs {
		if _, q, ok := strings.Cut(p, "/"); ok {
			return q
		}
	}
	return "USDT"
}

func (r *Runner) strategySource(b registry.Bot) ([]byte, error) {
	if b.StrategyFile == "" {
		return nil, fmt.Errorf("bot %s has no strategy_file; replay needs the code", b.Name)
	}
	src, err := os.ReadFile(filepath.Join(r.Cfg.Strategies, b.StrategyFile))
	if err != nil {
		return nil, fmt.Errorf("strategy file %s: %w", b.StrategyFile, err)
	}
	return src, nil
}

func (r *Runner) enqueue(j *Job, q chan *Job) error {
	j.ID = newID(r.now())
	j.State, j.Created = Queued, r.now()
	if err := hostdir.Make(filepath.Join(r.Cfg.JobDir, j.ID)); err != nil {
		return err
	}
	r.mu.Lock()
	waiting := 0
	for _, o := range r.jobs {
		if o.Kind == j.Kind && o.State == Queued {
			waiting++
		}
	}
	if waiting >= r.Cfg.MaxQueued {
		r.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(r.Cfg.JobDir, j.ID))
		return ErrBusy
	}
	r.jobs[j.ID] = j
	r.mu.Unlock()
	q <- j
	r.save(j)
	r.prune()
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newID(t time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "r" + t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Get returns a copy of the job.
func (r *Runner) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List returns copies, newest first.
func (r *Runner) List() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out
}

// Trades reads a finished replay's trades from its result file.
func (r *Runner) Trades(id string) ([]Trade, error) {
	raw, err := os.ReadFile(filepath.Join(r.Cfg.JobDir, filepath.Base(id), "trades.json"))
	if err != nil {
		return nil, err
	}
	var out []Trade
	return out, json.Unmarshal(raw, &out)
}

func (r *Runner) set(j *Job, f func(*Job)) {
	r.mu.Lock()
	f(j)
	r.mu.Unlock()
	r.save(j)
}

func (r *Runner) save(j *Job) {
	r.mu.Lock()
	raw, err := json.MarshalIndent(j, "", "  ")
	r.mu.Unlock()
	if err != nil {
		return
	}
	p := filepath.Join(r.Cfg.JobDir, j.ID, "job.json")
	if err := os.WriteFile(p+".tmp", raw, 0o640); err == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// prune keeps the newest KeepJobs finished jobs of each kind, so practice
// runs never push out a forward record its caller has not read yet.
func (r *Runner) prune() {
	if r.Cfg.KeepJobs <= 0 {
		return
	}
	jobs := r.List()
	kept := map[string]int{}
	for _, j := range jobs {
		if j.State != Done && j.State != Failed {
			continue
		}
		if kept[j.Kind]++; kept[j.Kind] <= r.Cfg.KeepJobs {
			continue
		}
		r.mu.Lock()
		delete(r.jobs, j.ID)
		r.mu.Unlock()
		_ = os.RemoveAll(filepath.Join(r.Cfg.JobDir, j.ID))
	}
}

// dispatch starts every job at once; each waits for its exchange, then for
// the lane's slot, so a job stuck on one exchange never holds the lane.
func (r *Runner) dispatch(ctx context.Context, l lane) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-l.q:
			r.wg.Add(1)
			go func() { defer r.wg.Done(); r.run(ctx, j, l.slot) }()
		}
	}
}

func (r *Runner) exLock(exchange string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.exMu[exchange]
	if !ok {
		m = &sync.Mutex{}
		r.exMu[exchange] = m
	}
	return m
}

func (r *Runner) run(ctx context.Context, j *Job, slot chan struct{}) {
	err := r.runSteps(ctx, j, slot)
	r.set(j, func(j *Job) {
		j.Finished, j.Note = r.now(), ""
		if err != nil {
			j.State, j.Error = Failed, err.Error()
		} else {
			j.State = Done
		}
	})
	if err != nil {
		r.logf("replay %s (%s %s): %v", j.ID, j.Kind, j.Exchange, err)
	}
}

func (r *Runner) runSteps(ctx context.Context, j *Job, slot chan struct{}) error {
	dir := filepath.Join(r.Cfg.JobDir, j.ID)
	for _, d := range []string{filepath.Join(dir, "strategies"), filepath.Join(dir, "out"), r.candles(j)} {
		if err := hostdir.Make(d); err != nil {
			return err
		}
	}
	// The API never starts in a backtest, but the schema wants real-looking keys.
	// A replay is always paper money, even for a live bot.
	paper := j.bot
	paper.DryRun = nil
	cfg, err := botconfig.Render(paper, botconfig.Secrets{APIPassword: randHex(16), JWTSecret: randHex(32)}, botconfig.Options{})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o640); err != nil {
		return err
	}
	if j.Kind == "replay" || j.Kind == "practice" {
		src := j.src
		if src == nil {
			if src, err = r.strategySource(j.bot); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "strategies", j.bot.StrategyFile), src, 0o640); err != nil {
			return err
		}
	}
	if practice(j.Kind) {
		return r.practiceSteps(ctx, j, slot)
	}

	lock := r.exLock(j.Exchange)
	r.set(j, func(j *Job) { j.Note = "waiting for the " + j.Exchange + " candle store" })
	lock.Lock()
	defer lock.Unlock()
	r.set(j, func(j *Job) { j.Note = "waiting for a free slot" })
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	r.set(j, func(j *Job) {
		j.State, j.Started, j.Note = Downloading, r.now(), "updating candles"
		if needsTrades(j.Exchange) {
			j.Note = "updating candles from trades (Kraken: slow the first time)"
		}
	})
	since := r.Cfg.SinceFor(j.Exchange)
	if r.storeStartsAfter(j.Exchange, since, j.Pairs, j.tfs) {
		r.set(j, func(j *Job) { j.Note = "filling in candle history from " + since.Format("2006-01-02") })
		if err := r.container(ctx, j, "dlp", downloadCmd(j.Exchange, since, j.tfs, true)); err != nil {
			return fmt.Errorf("download history: %w", err)
		}
		r.markStore(j.Exchange, since, j.Pairs, j.tfs)
	}
	if err := r.container(ctx, j, "dl", downloadCmd(j.Exchange, since, j.tfs, false)); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if j.Kind == "fetch" {
		return nil
	}
	from, _ := time.Parse("2006-01-02", j.From)
	to, _ := time.Parse("2006-01-02", j.To)
	r.set(j, func(j *Job) { j.State, j.Note = Testing, "" })
	if err := r.container(ctx, j, "bt", backtestCmd(j.bot, from, to)); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	sum, trades, err := r.saveResult(j)
	if err != nil {
		return err
	}
	rob := Assess(sum, trades, from, to, r.Cfg.Rules, j.ID)
	r.set(j, func(j *Job) { j.Summary, j.Robust = &sum, rob; j.Note = "checking for lookahead and recursive bias" })
	b := r.biasChecks(ctx, j, from, to)
	r.set(j, func(j *Job) { j.Bias = b })
	return nil
}

// saveResult reads the backtest's result and keeps its trades in trades.json.
func (r *Runner) saveResult(j *Job) (Summary, []Trade, error) {
	zips, _ := filepath.Glob(filepath.Join(r.Cfg.JobDir, j.ID, "out", "backtest-result-*.zip"))
	if len(zips) == 0 {
		return Summary{}, nil, errors.New("backtest wrote no result")
	}
	sort.Strings(zips)
	sum, trades, err := ParseResult(zips[len(zips)-1], j.bot.Strategy)
	if err != nil {
		return Summary{}, nil, err
	}
	raw, _ := json.Marshal(trades)
	return sum, trades, os.WriteFile(filepath.Join(r.Cfg.JobDir, j.ID, "trades.json"), raw, 0o640)
}

// biasChecks runs freqtrade's lookahead and recursive analysis on the same
// data. A broken analysis makes the verdict inconclusive, never clean.
func (r *Runner) biasChecks(ctx context.Context, j *Job, from, to time.Time) *Bias {
	if j.bot.FreqaiModel != "" {
		return &Bias{Lookahead: "skipped", Recursive: "skipped", Note: "FreqAI: the analysis would retrain the model per signal"}
	}
	b := &Bias{}
	csv := filepath.Join(r.Cfg.JobDir, j.ID, "out", "lookahead.csv")
	if _, err := r.step(ctx, j, "la", lookaheadCmd(j.bot, from, to)); err != nil {
		b.Lookahead, b.Note = "inconclusive", "lookahead analysis failed: "+err.Error()
	} else if st, n, ind, err := ParseLookahead(csv); err != nil {
		b.Lookahead, b.Note = "inconclusive", "lookahead analysis wrote no result"
	} else {
		b.Lookahead, b.Signals, b.Indicators = st, n, ind
		if st == "inconclusive" {
			b.Note = "too few trades in this range to judge lookahead bias; replay a longer range"
		}
	}
	logs, err := r.step(ctx, j, "ra", recursiveCmd(j.bot, from, to))
	if err != nil {
		b.Recursive = "variance"
		b.Note = strings.TrimPrefix(b.Note+"; recursive analysis failed: "+err.Error(), "; ")
		return b
	}
	var note string
	b.Recursive, note = ParseRecursive(logs)
	if note != "" {
		b.Note = strings.TrimPrefix(b.Note+"; "+note, "; ")
	}
	return b
}

// container runs one step to completion and removes it.
func (r *Runner) container(ctx context.Context, j *Job, step string, cmd []string) error {
	_, err := r.step(ctx, j, step, cmd)
	return err
}

// step is container that also hands back the step's log tail.
func (r *Runner) step(ctx context.Context, j *Job, step string, cmd []string) (string, error) {
	img := r.Cfg.Images[j.bot.Image]
	mem := r.Cfg.Memory
	if j.bot.FreqaiModel != "" && r.Cfg.FreqaiMem > 0 {
		mem = r.Cfg.FreqaiMem
	}
	if practice(j.Kind) && r.Cfg.PracticeMem > 0 {
		mem = r.Cfg.PracticeMem
	}
	spec := docker.Spec{
		Image: img, Cmd: cmd, User: r.Cfg.BotUser,
		Labels: map[string]string{docker.ManagedLabel: "replay", "wickfleet.replay": j.ID},
		HostConfig: docker.HostConfig{
			Binds: []string{
				filepath.Join(r.Cfg.JobDir, j.ID) + ":" + jobMount,
				r.candles(j) + ":" + candleMount + "/" + j.Exchange,
			},
			Memory:        mem,
			RestartPolicy: docker.RestartPolicy{Name: "no"},
			NetworkMode:   r.Cfg.Network,
			CapDrop:       []string{"ALL"},
			SecurityOpt:   []string{"no-new-privileges:true"},
		},
	}
	if practice(j.Kind) {
		spec.HostConfig.CPUShares = practiceCPU
	}
	timeout := r.Cfg.Timeout
	if j.Kind == "fetch" && r.Cfg.FetchTimeout > 0 {
		timeout = r.Cfg.FetchTimeout
	}
	if practice(j.Kind) {
		timeout = practiceTimeout
	}
	if timeout <= 0 {
		timeout = 3 * time.Hour
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id, err := docker.CreatePulling(cctx, r.Docker, "wickfleet-replay-"+j.ID+"-"+step, spec)
	if err != nil {
		return "", err
	}
	// The container goes whatever happens; a fresh context outlives a timeout.
	defer func() { _ = r.Docker.Remove(context.Background(), id) }()
	if err := r.Docker.Start(cctx, id); err != nil {
		return "", err
	}
	for {
		st, err := r.Docker.Inspect(cctx, id)
		if err != nil {
			if cctx.Err() != nil {
				return "", fmt.Errorf("gave up after %s", timeout)
			}
			return "", err
		}
		if !st.Running && st.Status != "created" {
			logs, _ := r.Docker.Logs(context.Background(), id, 400)
			switch {
			case st.OOMKill:
				return logs, fmt.Errorf("out of memory (limit %d MB)", mem>>20)
			case st.ExitCode != 0:
				return logs, fmt.Errorf("exit %d: %s", st.ExitCode, lastError(logs))
			case hasError(logs):
				// freqtrade exits 0 on fatal config errors.
				return logs, errors.New(lastError(logs))
			}
			return logs, nil
		}
		select {
		case <-cctx.Done():
			return "", fmt.Errorf("gave up after %s", timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

func hasError(logs string) bool { return strings.Contains(logs, " - ERROR - ") }

// lastError picks freqtrade's last ERROR line, else the last line.
func lastError(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if _, msg, ok := strings.Cut(lines[i], " - ERROR - "); ok {
			return strings.TrimSpace(msg)
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return "no output"
}

// SinceFor is where the store of one exchange starts.
func (c Config) SinceFor(exchange string) time.Time {
	if t, ok := c.SinceBy[strings.ToLower(exchange)]; ok {
		return t
	}
	return c.Since
}

// ParseSince reads "2020-01-01" or "2020-01-01,kraken=2025-01-01".
func ParseSince(s string) (time.Time, map[string]time.Time, error) {
	var def time.Time
	by := map[string]time.Time{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, day, isExchange := strings.Cut(part, "=")
		if !isExchange {
			day = name
		}
		t, err := time.Parse("2006-01-02", strings.TrimSpace(day))
		if err != nil {
			return time.Time{}, nil, fmt.Errorf("%q: %w", part, err)
		}
		if isExchange {
			by[strings.ToLower(strings.TrimSpace(name))] = t
		} else {
			def = t
		}
	}
	if def.IsZero() {
		return time.Time{}, nil, errors.New("a default start date is required")
	}
	return def, by, nil
}

// storeMarker records the earliest day one pair and timeframe was filled
// from; a prepend only fetches the job's own pairs and timeframes.
func (r *Runner) storeMarker(exchange, pair, tf string) string {
	name := ".wickfleet-since-" + strings.NewReplacer("/", "_", ":", "_").Replace(pair) + "-" + tf
	return filepath.Join(r.Cfg.CandleDir, exchange, name)
}

// storeStartsAfter: any pair and timeframe without a marker, or with one
// later than since.
func (r *Runner) storeStartsAfter(exchange string, since time.Time, pairs, tfs []string) bool {
	for _, p := range pairs {
		for _, tf := range tfs {
			raw, err := os.ReadFile(r.storeMarker(exchange, p, tf))
			if err != nil {
				return true
			}
			if t, err := time.Parse("2006-01-02", strings.TrimSpace(string(raw))); err != nil || t.After(since) {
				return true
			}
		}
	}
	return false
}

func (r *Runner) markStore(exchange string, since time.Time, pairs, tfs []string) {
	for _, p := range pairs {
		for _, tf := range tfs {
			if err := os.WriteFile(r.storeMarker(exchange, p, tf), []byte(since.Format("2006-01-02")+"\n"), 0o664); err != nil {
				r.logf("candle store %s: marker not written: %v", exchange, err)
				return
			}
		}
	}
}
