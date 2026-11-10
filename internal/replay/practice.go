package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wickfleet/internal/registry"
)

// The practice lane (B5): own queue, slot and candle store, no exchange lock and
// the lowest CPU weight, so births and forward records never wait for it.
const (
	practiceCPU     = 2
	practiceTimeout = 15 * time.Minute
	practiceLead    = 30 // days downloaded before a window, for indicator startup
	practiceFormat  = "json"
)

func practice(kind string) bool { return kind == "practice" || kind == "practice-fetch" }

// candles is the store a job reads: the shared one, or the practice lane's own.
func (r *Runner) candles(j *Job) string {
	if practice(j.Kind) {
		return r.practiceDir(j.Exchange)
	}
	return filepath.Join(r.Cfg.CandleDir, j.Exchange)
}

func (r *Runner) practiceDir(exchange string) string {
	return filepath.Join(r.Cfg.CandleDir, "practice", exchange)
}

// practiceBot keeps the practice store in JSON, which the hub reads for charts.
func practiceBot(b registry.Bot) registry.Bot {
	extra := map[string]any{}
	for k, v := range b.ConfigExtra {
		extra[k] = v
	}
	extra["dataformat_ohlcv"] = practiceFormat
	b.ConfigExtra = extra
	return b
}

// SubmitPractice queues a backtest of b with the given code over [from, to]
// on the practice lane, after topping up the practice store.
func (r *Runner) SubmitPractice(owner string, b registry.Bot, src []byte, from, to string) (*Job, error) {
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
	case t.After(r.now().UTC().Truncate(24 * time.Hour)):
		return nil, errors.New("to cannot be in the future")
	case len(src) == 0:
		return nil, errors.New("a practice run needs its strategy code")
	}
	if _, ok := r.Cfg.Images[b.Image]; !ok {
		return nil, fmt.Errorf("image %q is not allowed", b.Image)
	}
	tfs, err := Timeframes(b, src)
	if err != nil {
		return nil, err
	}
	b = practiceBot(b)
	j := &Job{Kind: "practice", Owner: owner, Bot: b.Name, Exchange: b.Exchange, Pairs: Pairs(b),
		Strategy: b.Strategy, From: from, To: to, bot: b, tfs: tfs, src: src}
	return j, r.enqueue(j, r.practiceQ)
}

// FetchPractice queues a top-up of the practice store for pairs and tfs from
// the window starting `from`, for charts before anyone has practised.
func (r *Runner) FetchPractice(exchange, image string, pairs, tfs []string, from string) (*Job, error) {
	if _, err := time.Parse("2006-01-02", from); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	b := practiceBot(registry.Bot{Name: "practice", Exchange: exchange, Pairs: pairs, Image: image,
		StakeCurrency: quote(pairs), Wallet: 1000, MaxOpenTrades: 1})
	j := &Job{Kind: "practice-fetch", Owner: "hub", Exchange: exchange, Pairs: pairs, From: from, bot: b, tfs: tfs}
	return j, r.enqueue(j, r.practiceQ)
}

// PracticePending reports whether a practice job of kind is queued or running.
func (r *Runner) PracticePending(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.jobs {
		if j.Kind == kind && j.State != Done && j.State != Failed {
			return true
		}
	}
	return false
}

// practiceSteps tops up the practice store and backtests. The lane's one slot
// makes it the store's only writer.
func (r *Runner) practiceSteps(ctx context.Context, j *Job, slot chan struct{}) error {
	r.set(j, func(j *Job) { j.Note = "waiting for a free practice slot" })
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	from, _ := time.Parse("2006-01-02", j.From)
	r.set(j, func(j *Job) { j.State, j.Started, j.Note = Downloading, r.now(), "updating practice candles" })
	if err := r.container(ctx, j, "dl", downloadCmd(j.Exchange, from.AddDate(0, 0, -practiceLead), j.tfs, false)); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if j.Kind == "practice-fetch" {
		return nil
	}
	to, _ := time.Parse("2006-01-02", j.To)
	r.set(j, func(j *Job) { j.State, j.Note = Testing, "" })
	if err := r.container(ctx, j, "bt", backtestCmd(j.bot, from, to)); err != nil {
		return fmt.Errorf("backtest: %w", err)
	}
	sum, _, err := r.saveResult(j)
	if err != nil {
		return err
	}
	r.set(j, func(j *Job) { j.Summary = &sum })
	return nil
}

// Candle is one OHLCV row; Time is the candle's open in Unix seconds.
type Candle struct {
	Time                           int64
	Open, High, Low, Close, Volume float64
}

// MarshalJSON keeps a candle as [time, open, high, low, close, volume]: a
// 90-day 15m chart is 8,640 of them, on a phone.
func (c Candle) MarshalJSON() ([]byte, error) {
	return json.Marshal([6]any{c.Time, c.Open, c.High, c.Low, c.Close, c.Volume})
}

// PracticeCandles reads one pair and timeframe from the practice store, oldest first.
func (r *Runner) PracticeCandles(exchange, pair, tf string) ([]Candle, error) {
	name := strings.NewReplacer("/", "_", ":", "_").Replace(pair) + "-" + tf + "." + practiceFormat
	raw, err := os.ReadFile(filepath.Join(r.practiceDir(exchange), name))
	if err != nil {
		return nil, err
	}
	return parseCandles(raw)
}

// parseCandles reads freqtrade's JSON store: rows of [ms, o, h, l, c, v].
func parseCandles(raw []byte) ([]Candle, error) {
	var rows [][]float64
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("practice candles: %w", err)
	}
	out := make([]Candle, 0, len(rows))
	for _, row := range rows {
		if len(row) != 6 {
			return nil, fmt.Errorf("practice candles: a row of %d values, want 6", len(row))
		}
		for _, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errors.New("practice candles: a value is not a number")
			}
		}
		out = append(out, Candle{Time: int64(row[0]) / 1000, Open: row[1], High: row[2], Low: row[3], Close: row[4], Volume: row[5]})
	}
	return out, nil
}
