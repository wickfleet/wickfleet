package drafter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"wickfleet/internal/strategy"
)

// Case is one eval sentence and what a good draft must contain.
type Case struct {
	Sentence  string   `json:"sentence"`
	Timeframe string   `json:"timeframe"`
	Kinds     []string `json:"kinds"`
	Ops       []string `json:"ops"`
}

// Score is one model over all cases.
type Score struct {
	Provider string
	Valid    int // drafts the validator accepted
	Correct  int // valid and every expectation met
	Cases    int
	Median   time.Duration
}

// LoadCases reads an eval file.
func LoadCases(path string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cs []Case
	return cs, json.Unmarshal(raw, &cs)
}

// Eval drafts every case with one provider; log gets one line per case.
func Eval(ctx context.Context, p Provider, cases []Case, log io.Writer) Score {
	sc := Score{Provider: p.Name(), Cases: len(cases)}
	var times []time.Duration
	for i, c := range cases {
		start := time.Now()
		r, err := Drafter{Providers: []Provider{p}}.Draft(ctx, c.Sentence)
		took := time.Since(start)
		times = append(times, took)
		if err != nil {
			fmt.Fprintf(log, "  %2d invalid  %5.1fs  %v\n", i+1, took.Seconds(), err)
			continue
		}
		sc.Valid++
		miss := c.missing(r)
		if len(miss) == 0 {
			sc.Correct++
		}
		fmt.Fprintf(log, "  %2d valid    %5.1fs  attempts %d  missing %v\n", i+1, took.Seconds(), r.Attempts, miss)
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	if len(times) > 0 {
		sc.Median = times[len(times)/2]
	}
	return sc
}

func (c Case) missing(r Result) []string {
	var miss []string
	if c.Timeframe != "" && r.Spec.Timeframe != c.Timeframe {
		miss = append(miss, "timeframe "+c.Timeframe)
	}
	kinds := map[string]bool{}
	for _, in := range r.Spec.Indicators {
		kinds[in.Kind] = true
	}
	for _, k := range c.Kinds {
		if !kinds[k] {
			miss = append(miss, "kind "+k)
		}
	}
	ops := map[string]bool{}
	for _, conds := range [][]strategy.Condition{r.Spec.Entry.All, r.Spec.Entry.Any, r.Spec.Exit.All, r.Spec.Exit.Any} {
		for _, cd := range conds {
			ops[cd.Op] = true
		}
	}
	for _, o := range c.Ops {
		if !ops[o] {
			miss = append(miss, "op "+o)
		}
	}
	return miss
}
