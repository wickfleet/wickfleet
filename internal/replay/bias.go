package replay

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Bias is what freqtrade's lookahead and recursive analysis found.
type Bias struct {
	// Lookahead: clean | biased | inconclusive | skipped. Only biased blocks a bot.
	Lookahead  string `json:"lookahead"`
	Signals    int    `json:"signals,omitempty"`
	Indicators string `json:"indicators,omitempty"` // the biased columns
	// Recursive: clean | variance | skipped. A warning, never a block.
	Recursive string `json:"recursive"`
	Note      string `json:"note,omitempty"`
}

// Blocks reports whether this result must stop a new bot.
func (b *Bias) Blocks() bool { return b != nil && b.Lookahead == "biased" }

// recursiveLimit is the variance at the strategy's own startup that counts
// as a problem; freqtrade's docs want it near zero.
const recursiveLimit = 0.1

// ParseLookahead reads lookahead-analysis's CSV export. A missing row means
// freqtrade could not judge (too few trades), which is not a pass.
func ParseLookahead(path string) (status string, signals int, indicators string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return "", 0, "", err
	}
	if len(rows) < 2 {
		return "inconclusive", 0, "", nil
	}
	head, row := rows[0], rows[1]
	col := func(name string) string {
		for i, h := range head {
			if h == name && i < len(row) {
				return row[i]
			}
		}
		return ""
	}
	signals, _ = strconv.Atoi(col("total_signals"))
	switch col("has_bias") {
	case "True":
		return "biased", signals, col("biased_indicators"), nil
	case "False":
		return "clean", signals, "", nil
	}
	return "inconclusive", signals, "", nil
}

// ParseRecursive reads recursive-analysis's log. freqtrade prints a "No
// variance" line when all is well; otherwise the table's column for the
// strategy's own startup shows how far each indicator drifts.
func ParseRecursive(log string) (status, note string) {
	if strings.Contains(log, "No variance on indicator(s) found") {
		return "clean", ""
	}
	lines := strings.Split(log, "\n")
	col := -1
	var bad []string
	for _, ln := range lines {
		if strings.Contains(ln, "(from strategy)") && strings.Contains(ln, "┃") {
			for i, c := range strings.Split(ln, "┃") {
				if strings.Contains(c, "(from strategy)") {
					col = i
				}
			}
			continue
		}
		if col < 0 || !strings.Contains(ln, "│") {
			continue
		}
		cells := strings.Split(ln, "│")
		if col >= len(cells) {
			continue
		}
		name := strings.TrimSpace(cells[1])
		v := strings.TrimSuffix(strings.TrimSpace(cells[col]), "%")
		if v == "-" || v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.Abs(f) > recursiveLimit {
			bad = append(bad, fmt.Sprintf("%s %s%%", name, v))
		}
	}
	if col < 0 {
		return "variance", "no recursive analysis table in the output"
	}
	if len(bad) == 0 {
		return "clean", ""
	}
	return "variance", "at the strategy's startup: " + strings.Join(bad, ", ") + "; raise startup_candle_count"
}
