package replay

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"wickfleet/internal/registry"
)

var (
	tfRE         = regexp.MustCompile(`^[1-9][0-9]{0,2}[mhdwM]$`)
	strategyTFRE = regexp.MustCompile(`(?m)^\s+timeframe\s*(?::\s*\w+)?\s*=\s*["']([^"']+)["']`)
)

// Timeframes are the candle sizes a bot needs: config_extra.timeframe, else
// the strategy's class attribute, plus FreqAI's include_timeframes.
func Timeframes(b registry.Bot, strategySource []byte) ([]string, error) {
	set := map[string]bool{}
	main, _ := b.ConfigExtra["timeframe"].(string)
	if main == "" {
		if m := strategyTFRE.FindSubmatch(strategySource); m != nil {
			main = string(m[1])
		}
	}
	if main == "" {
		return nil, fmt.Errorf("bot %s: no timeframe in config_extra or the strategy", b.Name)
	}
	set[main] = true
	for _, tf := range freqaiList(b, "include_timeframes") {
		set[tf] = true
	}
	out := make([]string, 0, len(set))
	for tf := range set {
		if !tfRE.MatchString(tf) {
			return nil, fmt.Errorf("bot %s: bad timeframe %q", b.Name, tf)
		}
		out = append(out, tf)
	}
	sort.Strings(out)
	return out, nil
}

// Pairs are the bot's pairs plus FreqAI's correlation pairs.
func Pairs(b registry.Bot) []string {
	set := map[string]bool{}
	for _, p := range append(append([]string{}, b.Pairs...), freqaiList(b, "include_corr_pairlist")...) {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func freqaiList(b registry.Bot, key string) []string {
	fq, _ := b.ConfigExtra["freqai"].(map[string]any)
	fp, _ := fq["feature_parameters"].(map[string]any)
	raw, _ := fp[key].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Kraken serves no candle history; freqtrade builds candles from trades.
func needsTrades(exchange string) bool { return strings.EqualFold(exchange, "kraken") }

const (
	jobMount    = "/job"
	candleMount = "/candles"
)

// downloadCmd appends to the store from `since`. With prepend it fills in
// history before the stored range instead (freqtrade then does not append).
func downloadCmd(exchange string, since time.Time, tfs []string, prepend bool) []string {
	cmd := []string{"download-data", "-c", jobMount + "/config.json",
		"--datadir", candleMount + "/" + exchange,
		"--timerange", since.Format("20060102") + "-", "--timeframes"}
	cmd = append(cmd, tfs...)
	if prepend {
		cmd = append(cmd, "--prepend")
	}
	if needsTrades(exchange) {
		cmd = append(cmd, "--dl-trades")
	}
	return cmd
}

func backtestCmd(b registry.Bot, from, to time.Time) []string {
	cmd := []string{"backtesting", "-c", jobMount + "/config.json",
		"--datadir", candleMount + "/" + b.Exchange,
		"--strategy", b.Strategy, "--strategy-path", jobMount + "/strategies",
		"--timerange", from.Format("20060102") + "-" + to.Format("20060102"),
		"--export", "trades", "--backtest-directory", jobMount + "/out", "--cache", "none"}
	if b.FreqaiModel != "" {
		cmd = append(cmd, "--freqaimodel", b.FreqaiModel)
	}
	return cmd
}

// lookaheadCmd: five trades are enough to judge; daily strategies trade rarely.
func lookaheadCmd(b registry.Bot, from, to time.Time) []string {
	return []string{"lookahead-analysis", "-c", jobMount + "/config.json",
		"--datadir", candleMount + "/" + b.Exchange,
		"--strategy", b.Strategy, "--strategy-path", jobMount + "/strategies",
		"--timerange", from.Format("20060102") + "-" + to.Format("20060102"),
		"--minimum-trade-amount", "5", "--targeted-trade-amount", "20",
		"--lookahead-analysis-exportfilename", jobMount + "/out/lookahead.csv"}
}

func recursiveCmd(b registry.Bot, from, to time.Time) []string {
	return []string{"recursive-analysis", "-c", jobMount + "/config.json",
		"--datadir", candleMount + "/" + b.Exchange,
		"--strategy", b.Strategy, "--strategy-path", jobMount + "/strategies",
		"--timerange", from.Format("20060102") + "-" + to.Format("20060102"), "--no-color"}
}
