package signals

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var tor, _ = time.LoadLocation("America/Toronto")

// series builds one bar per trading-ish day; closes[i] is month i's level.
func series(start time.Time, closes []float64) []Bar {
	var bars []Bar
	for i, c := range closes {
		m := start.AddDate(0, i, 0)
		for d := 1; d <= 25; d += 6 {
			bars = append(bars, Bar{Day: time.Date(m.Year(), m.Month(), d, 21, 0, 0, 0, time.UTC), Close: c})
		}
	}
	return bars
}

func TestMonthly(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	closes := []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 20, 999}
	bars := series(start, closes)
	now := time.Date(2025, 11, 15, 12, 0, 0, 0, tor) // November is open, ignored
	s, err := Monthly(bars, now, tor)
	if err != nil {
		t.Fatal(err)
	}
	if s.Month != "2025-10" || !s.Hold || s.SMA != 11 || s.Close != 20 {
		t.Fatalf("got %+v", s)
	}
	closes[9] = 5
	s, _ = Monthly(series(start, closes), now, tor)
	if s.Hold {
		t.Fatalf("below average must be OUT: %+v", s)
	}
	if _, err := Monthly(series(start, closes[:5]), now, tor); err == nil {
		t.Fatal("too few months must error")
	}
}

func TestCurrentMonthNeverCounts(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := series(start, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 50})
	s, err := Monthly(bars, time.Date(2025, 11, 30, 23, 0, 0, 0, tor), tor)
	if err != nil || s.Month != "2025-10" || s.Hold {
		t.Fatalf("a spike in the open month leaked in: %+v %v", s, err)
	}
}

type fakeSrc map[string][]Bar

func (f fakeSrc) Daily(_ context.Context, s string) ([]Bar, error) {
	if b, ok := f[s]; ok {
		return b, nil
	}
	return nil, errors.New("unknown symbol")
}

func TestEvaluateAndSummary(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	up := series(start, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 20})
	down := series(start, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 5})
	now := time.Date(2025, 11, 3, 8, 0, 0, 0, tor)
	sigs := Evaluate(context.Background(), fakeSrc{"XIU.TO": down, "XEQT.TO": up}, []string{"XIU.TO", "XEQT.TO", "BAD"}, now, tor)
	if len(sigs) != 3 || sigs[0].Symbol != "BAD" || sigs[0].Error == "" {
		t.Fatalf("%+v", sigs)
	}
	msg := Summary(sigs, map[string]bool{"XIU.TO": true, "XEQT.TO": true}, Mode{})
	for _, want := range []string{"2025-10 close", "XEQT.TO: HOLD, +81.8%", "XIU.TO: OUT, -47.4% vs average  <- SELL", "BAD: no signal"} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "XEQT.TO: HOLD, +81.8% vs average  <-") {
		t.Error("unchanged symbol must not be flagged")
	}
}

func TestYahoo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v8/finance/chart/XIU.TO") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"chart":{"result":[{"timestamp":[1700000000,1700086400,1700172800],
		  "indicators":{"adjclose":[{"adjclose":[30.5,null,31.0]}]}}]}}`)
	}))
	defer srv.Close()
	bars, err := Yahoo{Base: srv.URL}.Daily(context.Background(), "XIU.TO")
	if err != nil || len(bars) != 2 || bars[1].Close != 31.0 {
		t.Fatalf("%v %+v", err, bars)
	}
	if _, err := (Yahoo{Base: srv.URL}).Daily(context.Background(), "NOPE"); err == nil {
		t.Fatal("404 must error")
	}
}

func TestSummaryTaxable(t *testing.T) {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	up := series(start, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 20})
	down := series(start, []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 5})
	now := time.Date(2025, 11, 3, 8, 0, 0, 0, tor)
	sigs := Evaluate(context.Background(), fakeSrc{"XIC.TO": down, "VFV.TO": up}, []string{"XIC.TO", "VFV.TO"}, now, tor)
	msg := Summary(sigs, map[string]bool{"XIC.TO": true, "VFV.TO": false}, Mode{Taxable: true, Parking: "CLF"})
	for _, want := range []string{"Taxable account", "XIC.TO: OUT, -47.4% vs average  <- OUT: stop buying, new money to CLF; sell only at a loss", "VFV.TO: HOLD, +81.8% vs average  <- back IN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "SELL at the next open") {
		t.Error("taxable mode must never say SELL")
	}
}
