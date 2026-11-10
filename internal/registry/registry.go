// Package registry loads bot definitions: either a flat directory of
// <bot>.yaml files, or a repo with defaults.yaml, bots/ and strategies/.
package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"wickfleet/internal/watch"
)

// Bot is one bot definition, defaults already applied.
type Bot struct {
	Name          string         `yaml:"name" json:"name"`
	Owner         string         `yaml:"owner" json:"owner"`
	Strategy      string         `yaml:"strategy" json:"strategy"`
	StrategyFile  string         `yaml:"strategy_file" json:"strategy_file,omitempty"`
	Exchange      string         `yaml:"exchange" json:"exchange"`
	Pairs         []string       `yaml:"pairs" json:"pairs"`
	StakeCurrency string         `yaml:"stake_currency" json:"stake_currency"`
	Wallet        float64        `yaml:"wallet" json:"wallet"`
	MaxOpenTrades int            `yaml:"max_open_trades" json:"max_open_trades"`
	DryRun        *bool          `yaml:"dry_run" json:"dry_run"`
	Fee           *float64       `yaml:"fee" json:"fee,omitempty"`
	Fiat          *string        `yaml:"fiat" json:"fiat,omitempty"`
	Image         string         `yaml:"image" json:"image,omitempty"`
	MemLimit      string         `yaml:"mem_limit" json:"mem_limit,omitempty"`
	FreqaiModel   string         `yaml:"freqaimodel" json:"freqaimodel,omitempty"`
	ConfigExtra   map[string]any `yaml:"config_extra" json:"config_extra,omitempty"`
	// Watch is the entry conditions the hub reports as "waiting for".
	Watch []watch.Rule `yaml:"watch" json:"watch,omitempty"`
	// Managed names the controller that owns this bot. A managed bot changes only
	// through that controller. The key name is kept for registry compatibility.
	Managed string `yaml:"arena" json:"arena,omitempty"`
}

// Policy is what an instance allows. The zero value is a dry-run-only instance.
type Policy struct {
	AllowLive bool // WICKFLEET_LIVE_TRADING: bots may set dry_run: false
}

// Dry reports whether the bot trades paper money. Unset means dry.
func (b Bot) Dry() bool { return b.DryRun == nil || *b.DryRun }

var (
	nameRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	strategyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	fileRE     = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}\.py$`)
	memRE      = regexp.MustCompile(`^[1-9][0-9]{0,4}[mg]$`)
)

// ValidName reports whether s is usable as a bot name, path segment and
// container name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

func (b Bot) validate(p Policy) error {
	switch {
	case !ValidName(b.Name):
		return fmt.Errorf("bad bot name %q", b.Name)
	case b.Owner == "":
		return fmt.Errorf("bot %s: owner missing", b.Name)
	case b.Strategy == "":
		return fmt.Errorf("bot %s: strategy missing", b.Name)
	case !strategyRE.MatchString(b.Strategy):
		return fmt.Errorf("bot %s: bad strategy class %q", b.Name, b.Strategy)
	case b.StrategyFile != "" && !fileRE.MatchString(b.StrategyFile):
		return fmt.Errorf("bot %s: bad strategy_file %q", b.Name, b.StrategyFile)
	case b.MemLimit != "" && !memRE.MatchString(b.MemLimit):
		return fmt.Errorf("bot %s: bad mem_limit %q", b.Name, b.MemLimit)
	case b.FreqaiModel != "" && !strategyRE.MatchString(b.FreqaiModel):
		return fmt.Errorf("bot %s: bad freqaimodel %q", b.Name, b.FreqaiModel)
	case b.Managed != "" && !ValidName(b.Managed):
		return fmt.Errorf("bot %s: bad managed tag %q", b.Name, b.Managed)
	case !b.Dry() && !p.AllowLive:
		return fmt.Errorf("bot %s: live trading is not allowed on this instance", b.Name)
	case !b.Dry() && b.Managed != "":
		return fmt.Errorf("bot %s: managed bots never trade live", b.Name)
	}
	for _, r := range b.Watch {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("bot %s: %w", b.Name, err)
		}
	}
	return nil
}

// Load reads a registry. A directory with bots/ is a repo (defaults.yaml
// applies to every bot); otherwise every *.yaml in dir is a bot.
func Load(dir string) ([]Bot, error) { return LoadWith(dir, Policy{}) }

// LoadWith is Load under an instance policy.
func LoadWith(dir string, p Policy) ([]Bot, error) {
	// A missing checkout must not read as "no bots": the reconciler would
	// remove every container.
	if st, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("registry %s: %w", dir, err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("registry %s is not a directory", dir)
	}
	botDir, defaults := dir, map[string]any{}
	if st, err := os.Stat(filepath.Join(dir, "bots")); err == nil && st.IsDir() {
		botDir = filepath.Join(dir, "bots")
		if raw, err := os.ReadFile(filepath.Join(dir, "defaults.yaml")); err == nil {
			if err := yaml.Unmarshal(raw, &defaults); err != nil {
				return nil, fmt.Errorf("defaults.yaml: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	files, err := filepath.Glob(filepath.Join(botDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	bots := make([]Bot, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		b, err := loadBot(f, defaults, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if want := strings.TrimSuffix(filepath.Base(f), ".yaml"); want != b.Name {
			return nil, fmt.Errorf("%s: name %q does not match the file", filepath.Base(f), b.Name)
		}
		if seen[b.Name] {
			return nil, fmt.Errorf("duplicate bot %q", b.Name)
		}
		seen[b.Name] = true
		bots = append(bots, b)
	}
	return bots, nil
}

// loadBot merges the file over the defaults (top-level keys), then decodes
// strictly so a typo is an error, not a silently ignored key.
func loadBot(path string, defaults map[string]any, p Policy) (Bot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Bot{}, err
	}
	return ParseBotWith(raw, defaults, p)
}

// ParseBot checks one bot file over defaults exactly as Load does, so a
// writer can refuse a file that would break the next registry load.
func ParseBot(raw []byte, defaults map[string]any) (Bot, error) {
	return ParseBotWith(raw, defaults, Policy{})
}

// ParseBotWith is ParseBot under an instance policy.
func ParseBotWith(raw []byte, defaults map[string]any, p Policy) (Bot, error) {
	own := map[string]any{}
	if err := yaml.Unmarshal(raw, &own); err != nil {
		return Bot{}, err
	}
	merged := map[string]any{}
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range own {
		merged[k] = v
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return Bot{}, err
	}
	var b Bot
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil {
		return Bot{}, err
	}
	return b, b.validate(p)
}

// Find returns the bot called name.
func Find(bots []Bot, name string) (Bot, bool) {
	for _, b := range bots {
		if b.Name == name {
			return b, true
		}
	}
	return Bot{}, false
}
