package strategy

import (
	"encoding/json"
	"math"
	"os"
	"slices"
	"testing"
)

type goldenSet struct {
	Candles [][6]float64          `json:"candles"`
	Columns map[string][]*float64 `json:"columns"`
}

func golden(t *testing.T) map[string]goldenSet {
	t.Helper()
	raw, err := os.ReadFile("testdata/columns.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var sets map[string]goldenSet
	if err := json.Unmarshal(raw, &sets); err != nil {
		t.Fatal(err)
	}
	return sets
}

func bars(rows [][6]float64) []Bar {
	out := make([]Bar, len(rows))
	for i, r := range rows {
		out[i] = Bar{Time: int64(r[0]) / 1000, Open: r[1], High: r[2], Low: r[3], Close: r[4], Volume: r[5]}
	}
	return out
}

// The golden file is freqtrade 2026.9 (TA-Lib 0.8.1, pandas 3.0.6) running AllKinds
// on candles with a gap, a flat stretch and prices down to 5e-7 (make strategy-golden).
func TestColumnsMatchFreqtrade(t *testing.T) {
	s := load(t, "AllKinds")
	for name, set := range golden(t) {
		b := bars(set.Candles)
		f := Fill(b, b[0].Time, 3600)
		want := set.Columns
		if len(f.Time) != len(want["time"]) || len(f.Time) == len(b) && name == "btc" {
			t.Fatalf("%s: %d rows from %d bars, freqtrade has %d", name, len(f.Time), len(b), len(want["time"]))
		}
		for i := range f.Time {
			if f.Time[i] != int64(*want["time"][i]) {
				t.Fatalf("%s row %d: time %d, want %v", name, i, f.Time[i], *want["time"][i])
			}
		}
		cols := s.Columns(f)
		for p, x := range map[string][]float64{"open": f.Open, "high": f.High, "low": f.Low, "close": f.Close, "volume": f.Volume} {
			cols[p] = x
		}
		scale := math.Abs(f.Close[0])
		checked := 0
		for col, w := range want {
			if col == "time" || col == "enter_long" || col == "exit_long" {
				continue
			}
			got, ok := cols[col]
			if !ok {
				t.Errorf("%s: no column %s", name, col)
				continue
			}
			for i, wv := range w {
				g := got[i]
				switch {
				case wv == nil && !math.IsNaN(g):
					t.Errorf("%s %s[%d] = %v, want NaN", name, col, i, g)
				case wv != nil && math.IsNaN(g):
					t.Errorf("%s %s[%d] = NaN, want %v", name, col, i, *wv)
				case wv != nil && math.Abs(g-*wv) > 1e-9*(math.Abs(g)+math.Abs(*wv))+1e-12*scale:
					t.Errorf("%s %s[%d] = %v, want %v", name, col, i, g, *wv)
				default:
					checked++
				}
			}
		}
		fired := 0
		for i := range f.Time {
			for _, side := range []struct {
				entry bool
				col   string
			}{{true, "enter_long"}, {false, "exit_long"}} {
				got, known := s.Fires(side.entry, f, cols, i)
				if want := want[side.col][i] != nil; got != want || !known {
					t.Errorf("%s %s[%d] = %v (known %v), freqtrade %v", name, side.col, i, got, known, want)
				}
				if got {
					fired++
				}
			}
		}
		t.Logf("%s: %d values, %d signals", name, checked, fired)
		if checked < 3000 || fired < 100 {
			t.Fatalf("%s: only %d values and %d signals compared", name, checked, fired)
		}
	}
}

func TestFill(t *testing.T) {
	b := []Bar{{Time: 0, Close: 1}, {Time: 3600, Close: 2}, {Time: 3600, Close: 9}, {Time: 3 * 3600, Open: 3, High: 4, Low: 2, Close: 3, Volume: 7}}
	f := Fill(b, 3600, 3600)
	if !slices.Equal(f.Time, []int64{3600, 7200, 10800}) || !slices.Equal(f.Close, []float64{2, 2, 3}) ||
		f.Open[1] != 2 || f.High[1] != 2 || f.Low[1] != 2 || f.Volume[1] != 0 || f.Volume[2] != 7 {
		t.Fatalf("%+v", f)
	}
	if TimeframeSeconds("15m") != 900 || TimeframeSeconds("4h") != 14400 || TimeframeSeconds("1w") != 604800 || TimeframeSeconds("x") != 0 {
		t.Fatal("timeframe seconds")
	}
}
