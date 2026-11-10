// Package strategy turns a small, checked JSON spec into a freqtrade
// strategy. Only whitelisted indicators and conditions exist, every name is
// matched against a strict pattern and numbers are formatted here, so the
// generated Python never carries text a user typed.
package strategy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Version is the spec format this package reads.
const Version = 1

// Spec is one strategy: spot, long only, one timeframe.
type Spec struct {
	Version     int                `json:"version"`
	Name        string             `json:"name"`
	Author      string             `json:"author,omitempty"` // set by the hub, never from a request
	Description string             `json:"description,omitempty"`
	Timeframe   string             `json:"timeframe"`
	Stoploss    float64            `json:"stoploss"`           // e.g. -0.20
	ROI         map[string]float64 `json:"roi,omitempty"`      // minutes -> ratio; empty = signal exits only
	Trailing    *Trailing          `json:"trailing,omitempty"` //
	Indicators  []Indicator        `json:"indicators"`
	Entry       Group              `json:"entry"`
	Exit        Group              `json:"exit"`

	// Model turns on FreqAI: operands prediction and do_predict exist, and
	// the bot needs a freqai config. Its windows live there, so StartupCandles
	// is required with a model.
	Model          *Model      `json:"model,omitempty"`
	StartupCandles int         `json:"startup_candles,omitempty"`
	EntryGuard     *EntryGuard `json:"entry_guard,omitempty"`
}

// Model picks a fixed feature set and target; nothing about them is free text.
type Model struct {
	Features string `json:"features"` // freqai_example
	Target   string `json:"target"`   // mean_return
}

// EntryGuard refuses an entry filled too far above the last close.
type EntryGuard struct {
	MaxAboveClose float64 `json:"max_above_close"`
}

// Trailing is freqtrade's trailing stop.
type Trailing struct {
	Positive          float64 `json:"positive"`
	Offset            float64 `json:"offset,omitempty"`
	OnlyOffsetReached bool    `json:"only_offset_reached,omitempty"`
}

// Indicator is one computed column. Kinds and their parameters:
//
//	sma ema tema highest lowest  source, period, shift
//	rsi atr adx                  period (rsi also source)
//	bollinger                    source, period, stddev   -> .upper .middle .lower
//	macd                         source, fast, slow, signal -> .macd .signal .hist
//	stoch                        period, smooth_k, smooth_d -> .k .d
type Indicator struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Source  string  `json:"source,omitempty"`
	Period  int     `json:"period,omitempty"`
	Shift   int     `json:"shift,omitempty"`
	StdDev  float64 `json:"stddev,omitempty"`
	Fast    int     `json:"fast,omitempty"`
	Slow    int     `json:"slow,omitempty"`
	Signal  int     `json:"signal,omitempty"`
	SmoothK int     `json:"smooth_k,omitempty"`
	SmoothD int     `json:"smooth_d,omitempty"`
}

// Group is one level of AND (all) or OR (any) over conditions.
type Group struct {
	All []Condition `json:"all,omitempty"`
	Any []Condition `json:"any,omitempty"`
}

// Condition compares two operands, or watches one rise or fall over Bars.
// An operand is a price column, an indicator id (field after a dot) or a number.
type Condition struct {
	Left  Operand `json:"left"`
	Op    string  `json:"op"`
	Right Operand `json:"right,omitzero"`
	Bars  int     `json:"bars,omitempty"`
}

// Operand is either a reference or a number; JSON gives one or the other.
type Operand struct {
	Ref string
	Num *float64
}

func (o Operand) IsZero() bool { return o.Ref == "" && o.Num == nil }

func (o Operand) MarshalJSON() ([]byte, error) {
	if o.Num != nil {
		return json.Marshal(*o.Num)
	}
	return json.Marshal(o.Ref)
}

func (o *Operand) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err == nil {
		o.Num = &f
		return nil
	}
	return json.Unmarshal(b, &o.Ref)
}

var (
	nameRE   = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,47}$`)
	idRE     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,23}$`)
	tfRE     = regexp.MustCompile(`^(1|3|5|15|30)m$|^(1|2|4|6|8|12)h$|^1d$|^1w$`)
	minRE    = regexp.MustCompile(`^[0-9]{1,6}$`)
	descBad  = regexp.MustCompile(`[^A-Za-z0-9 ,.;:()%/+<>=_-]`)
	authorRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
)

// Price columns every candle has.
var prices = map[string]bool{"open": true, "high": true, "low": true, "close": true, "volume": true}

// fields a multi-output indicator exposes.
var fields = map[string][]string{
	"bollinger": {"upper", "middle", "lower"},
	"macd":      {"macd", "signal", "hist"},
	"stoch":     {"k", "d"},
}

var ops = map[string]bool{">": true, "<": true, ">=": true, "<=": true, "==": true,
	"crosses_above": true, "crosses_below": true, "rising": true, "falling": true}

const (
	maxIndicators = 20
	maxConditions = 10
	maxPeriod     = 500
)

// ValidName reports whether s is usable as a strategy class and file name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Parse decodes strictly and validates.
func Parse(raw []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	return s, s.Validate()
}

// Validate checks every field; the generator relies on it.
func (s Spec) Validate() error {
	switch {
	case s.Version != Version:
		return fmt.Errorf("version must be %d", Version)
	case s.Author != "" && !authorRE.MatchString(s.Author):
		return errors.New("author: up to 64 of A-Z a-z 0-9 . _ @ -")
	case !nameRE.MatchString(s.Name):
		return fmt.Errorf("name %q: a class name, letter first, up to 48 letters and digits", s.Name)
	case len(s.Description) > 200 || descBad.MatchString(s.Description):
		return errors.New("description: up to 200 plain characters, no quotes or line breaks")
	case !tfRE.MatchString(s.Timeframe):
		return fmt.Errorf("timeframe %q not supported", s.Timeframe)
	case !(s.Stoploss < 0 && s.Stoploss >= -0.99):
		return errors.New("stoploss must be between -0.99 and 0 (e.g. -0.10)")
	case len(s.Indicators) > maxIndicators:
		return fmt.Errorf("at most %d indicators", maxIndicators)
	}
	for k, v := range s.ROI {
		if !minRE.MatchString(k) || !finite(v) || v < 0 || v > 1000 {
			return fmt.Errorf("roi %q: %v", k, v)
		}
	}
	if t := s.Trailing; t != nil {
		if !finite(t.Positive) || t.Positive <= 0 || t.Positive > 1 || !finite(t.Offset) || t.Offset < 0 || t.Offset > 1 ||
			(t.Offset != 0 && t.Offset <= t.Positive) {
			return errors.New("trailing: positive in (0,1], offset 0 or above positive")
		}
	}
	refs := map[string]bool{}
	for p := range prices {
		refs[p] = true
	}
	if m := s.Model; m != nil {
		if m.Features != "freqai_example" || m.Target != "mean_return" {
			return errors.New("model: features must be freqai_example and target mean_return")
		}
		if s.StartupCandles < 1 || s.StartupCandles > 1000 {
			return errors.New("startup_candles (1..1000) is required with a model")
		}
		refs["prediction"], refs["do_predict"] = true, true
	} else if s.StartupCandles < 0 || s.StartupCandles > 1000 {
		return errors.New("startup_candles must be 0..1000")
	}
	if g := s.EntryGuard; g != nil && !(finite(g.MaxAboveClose) && g.MaxAboveClose > 0 && g.MaxAboveClose <= 0.05) {
		return errors.New("entry_guard.max_above_close must be in (0, 0.05]")
	}
	for _, in := range s.Indicators {
		if err := in.validate(); err != nil {
			return err
		}
		if refs[in.ID] || hasRefPrefix(refs, in.ID) {
			return fmt.Errorf("indicator id %q used twice or clashes with a price", in.ID)
		}
		if fs, ok := fields[in.Kind]; ok {
			for _, f := range fs {
				refs[in.ID+"."+f] = true
			}
		} else {
			refs[in.ID] = true
		}
	}
	for side, g := range map[string]Group{"entry": s.Entry, "exit": s.Exit} {
		if err := g.validate(side, refs); err != nil {
			return err
		}
	}
	return nil
}

func hasRefPrefix(refs map[string]bool, id string) bool {
	for r := range refs {
		if strings.HasPrefix(r, id+".") {
			return true
		}
	}
	return false
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func (in Indicator) validate() error {
	bad := func(msg string) error { return fmt.Errorf("indicator %q (%s): %s", in.ID, in.Kind, msg) }
	if !idRE.MatchString(in.ID) {
		return fmt.Errorf("indicator id %q: lower case, letter first, up to 24 of a-z 0-9 _", in.ID)
	}
	period := func(p int, what string) error {
		if p < 1 || p > maxPeriod {
			return bad(fmt.Sprintf("%s must be 1..%d", what, maxPeriod))
		}
		return nil
	}
	src := func() error {
		if !prices[in.Source] {
			return bad("source must be open, high, low, close or volume")
		}
		return nil
	}
	if in.Shift < 0 || in.Shift > 50 {
		return bad("shift must be 0..50")
	}
	// Each kind accepts only its own parameters; a stray one is a mistake, not ignored.
	used := map[string]bool{}
	switch in.Kind {
	case "sma", "ema", "tema", "highest", "lowest":
		used = map[string]bool{"source": true, "period": true, "shift": true}
		if err := src(); err != nil {
			return err
		}
		if err := period(in.Period, "period"); err != nil {
			return err
		}
	case "rsi":
		used = map[string]bool{"source": true, "period": true}
		if err := src(); err != nil {
			return err
		}
		if err := period(in.Period, "period"); err != nil {
			return err
		}
	case "atr", "adx":
		used = map[string]bool{"period": true}
		if err := period(in.Period, "period"); err != nil {
			return err
		}
	case "bollinger":
		used = map[string]bool{"source": true, "period": true, "stddev": true}
		if err := src(); err != nil {
			return err
		}
		if err := period(in.Period, "period"); err != nil {
			return err
		}
		if !finite(in.StdDev) || in.StdDev <= 0 || in.StdDev > 5 {
			return bad("stddev must be in (0, 5]")
		}
	case "macd":
		used = map[string]bool{"source": true, "fast": true, "slow": true, "signal": true}
		if err := src(); err != nil {
			return err
		}
		for _, p := range []struct {
			v int
			n string
		}{{in.Fast, "fast"}, {in.Slow, "slow"}, {in.Signal, "signal"}} {
			if err := period(p.v, p.n); err != nil {
				return err
			}
		}
		if in.Fast >= in.Slow {
			return bad("fast must be shorter than slow")
		}
	case "stoch":
		used = map[string]bool{"period": true, "smooth_k": true, "smooth_d": true}
		for _, p := range []struct {
			v int
			n string
		}{{in.Period, "period"}, {in.SmoothK, "smooth_k"}, {in.SmoothD, "smooth_d"}} {
			if err := period(p.v, p.n); err != nil {
				return err
			}
		}
	default:
		return bad("unknown kind; use sma ema tema highest lowest rsi atr adx bollinger macd stoch")
	}
	set := map[string]bool{"source": in.Source != "", "period": in.Period != 0, "shift": in.Shift != 0,
		"stddev": in.StdDev != 0, "fast": in.Fast != 0, "slow": in.Slow != 0, "signal": in.Signal != 0,
		"smooth_k": in.SmoothK != 0, "smooth_d": in.SmoothD != 0}
	for k, on := range set {
		if on && !used[k] {
			return bad(k + " does not apply to this kind")
		}
	}
	return nil
}

func (g Group) validate(side string, refs map[string]bool) error {
	conds := g.All
	if len(g.All) > 0 && len(g.Any) > 0 {
		return fmt.Errorf("%s: use all or any, not both", side)
	}
	if len(g.Any) > 0 {
		conds = g.Any
	}
	if len(conds) == 0 {
		return fmt.Errorf("%s: needs at least one condition", side)
	}
	if len(conds) > maxConditions {
		return fmt.Errorf("%s: at most %d conditions", side, maxConditions)
	}
	for i, c := range conds {
		where := fmt.Sprintf("%s condition %d", side, i+1)
		if !ops[c.Op] {
			return fmt.Errorf("%s: unknown op %q", where, c.Op)
		}
		if c.Left.Num != nil || !refs[c.Left.Ref] {
			return fmt.Errorf("%s: left must be a price or an indicator, got %s", where, c.Left.label())
		}
		switch c.Op {
		case "==":
			if c.Bars != 0 || c.Right.Num == nil || !finite(*c.Right.Num) {
				return fmt.Errorf("%s: == compares with a number only", where)
			}
			continue
		case "rising", "falling":
			if !c.Right.IsZero() || c.Bars < 1 || c.Bars > 50 {
				return fmt.Errorf("%s: %s takes bars 1..50 and no right side", where, c.Op)
			}
		default:
			if c.Bars != 0 {
				return fmt.Errorf("%s: bars only applies to rising and falling", where)
			}
			if c.Right.Num != nil {
				if !finite(*c.Right.Num) {
					return fmt.Errorf("%s: right is not a finite number", where)
				}
			} else if !refs[c.Right.Ref] {
				return fmt.Errorf("%s: right %s is not a price, an indicator or a number", where, c.Right.label())
			}
		}
	}
	return nil
}

func (o Operand) label() string {
	if o.Num != nil {
		return strconv.FormatFloat(*o.Num, 'g', -1, 64)
	}
	if o.Ref == "" {
		return "(nothing)"
	}
	return strconv.Quote(o.Ref)
}

// Startup is the candles freqtrade must load before the first signal: the
// longest lookback any indicator needs.
func (s Spec) Startup() int {
	n := s.StartupCandles
	for _, in := range s.Indicators {
		var need int
		switch in.Kind {
		case "macd":
			need = in.Slow + in.Signal
		case "stoch":
			need = in.Period + in.SmoothK + in.SmoothD
		case "ema", "tema", "adx":
			need = in.Period * 3 // these need settling time, not just the window
		default:
			need = in.Period
		}
		if need+in.Shift > n {
			n = need + in.Shift
		}
	}
	return n
}

// roiKeys sorted numerically, for stable output.
func (s Spec) roiKeys() []string {
	keys := make([]string, 0, len(s.ROI))
	for k := range s.ROI {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a < b
	})
	return keys
}
