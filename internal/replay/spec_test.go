package replay

import (
	"strings"
	"testing"
	"time"

	"wickfleet/internal/registry"
)

func TestTimeframes(t *testing.T) {
	src := []byte("class EmaCross(IStrategy):\n    timeframe = \"1d\"\n    startup_candle_count = 200\n")
	b := registry.Bot{Name: "trend", Pairs: []string{"BTC/USDT"}}
	if got, err := Timeframes(b, src); err != nil || strings.Join(got, ",") != "1d" {
		t.Fatalf("%v %v", got, err)
	}
	typed := []byte("class X(IStrategy):\n    timeframe: str = '5m'\n")
	if got, _ := Timeframes(b, typed); strings.Join(got, ",") != "5m" {
		t.Fatalf("typed attribute: %v", got)
	}
	fq := registry.Bot{Name: "freqai", ConfigExtra: map[string]any{
		"timeframe": "1h",
		"freqai": map[string]any{"feature_parameters": map[string]any{
			"include_timeframes":    []any{"1h", "4h"},
			"include_corr_pairlist": []any{"BTC/USDT", "ETH/USDT"},
		}},
	}, Pairs: []string{"SOL/USDT", "BTC/USDT"}}
	if got, err := Timeframes(fq, src); err != nil || strings.Join(got, ",") != "1h,4h" {
		t.Fatalf("config_extra wins over the source, plus freqai: %v %v", got, err)
	}
	if got := strings.Join(Pairs(fq), ","); got != "BTC/USDT,ETH/USDT,SOL/USDT" {
		t.Fatalf("pairs %s", got)
	}
	if _, err := Timeframes(b, []byte("class X: pass\n")); err == nil {
		t.Fatal("no timeframe must fail")
	}
	bad := registry.Bot{Name: "x", ConfigExtra: map[string]any{"timeframe": "1d; rm -rf /"}}
	if _, err := Timeframes(bad, nil); err == nil {
		t.Fatal("a junk timeframe must fail")
	}
}

func TestCommands(t *testing.T) {
	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	k := strings.Join(downloadCmd("kraken", since, []string{"1d", "1h"}, false), " ")
	if k != "download-data -c /job/config.json --datadir /candles/kraken --timerange 20250101- --timeframes 1d 1h --dl-trades" {
		t.Fatalf("kraken %q", k)
	}
	if strings.Contains(strings.Join(downloadCmd("binance", since, []string{"1d"}, false), " "), "--dl-trades") {
		t.Fatal("binance has candles, no trades download")
	}
	if p := strings.Join(downloadCmd("binance", since, []string{"1d"}, true), " "); !strings.HasSuffix(p, "--timeframes 1d --prepend") {
		t.Fatalf("prepend %q", p)
	}
	b := registry.Bot{Exchange: "binance", Strategy: "FreqaiExample", FreqaiModel: "LightGBMRegressor"}
	got := strings.Join(backtestCmd(b, since, time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)), " ")
	want := "backtesting -c /job/config.json --datadir /candles/binance --strategy FreqaiExample --strategy-path /job/strategies --timerange 20250101-20250630 --export trades --backtest-directory /job/out --cache none --freqaimodel LightGBMRegressor"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
