package registry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// WatchFile is the watchlist's path inside a registry repo.
const WatchFile = "watchlist.yaml"

// MaxWatch caps the list: every symbol is one Yahoo request per hour.
const MaxWatch = 50

// Watch is one symbol the hub computes signals for. Any Yahoo Finance
// symbol works: ETFs and stocks (XIC.TO), crypto (BTC-USD), FX (CADUSD=X).
type Watch struct {
	Symbol string `yaml:"symbol" json:"symbol"`
	Note   string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Watchlist is the file's shape.
type Watchlist struct {
	Symbols []Watch `yaml:"symbols" json:"symbols"`
}

var symbolRE = regexp.MustCompile(`^[A-Za-z0-9^][A-Za-z0-9.=^-]{0,19}$`)

// Validate checks symbols, notes, duplicates and the size cap.
func (w Watchlist) Validate() error {
	if len(w.Symbols) > MaxWatch {
		return fmt.Errorf("at most %d symbols", MaxWatch)
	}
	seen := map[string]bool{}
	for _, s := range w.Symbols {
		switch {
		case !symbolRE.MatchString(s.Symbol):
			return fmt.Errorf("bad symbol %q", s.Symbol)
		case seen[s.Symbol]:
			return fmt.Errorf("duplicate symbol %q", s.Symbol)
		case len(s.Note) > 80:
			return fmt.Errorf("%s: note longer than 80 characters", s.Symbol)
		}
		seen[s.Symbol] = true
	}
	return nil
}

// Names returns the symbols in file order.
func (w Watchlist) Names() []string {
	out := make([]string, 0, len(w.Symbols))
	for _, s := range w.Symbols {
		out = append(out, s.Symbol)
	}
	return out
}

// LoadWatchlist reads dir/watchlist.yaml. ok is false when the file does
// not exist, so a caller can fall back to another source.
func LoadWatchlist(dir string) (w Watchlist, ok bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, WatchFile))
	if errors.Is(err, os.ErrNotExist) {
		return Watchlist{}, false, nil
	}
	if err != nil {
		return Watchlist{}, false, err
	}
	w, err = ParseWatchlist(raw)
	return w, err == nil, err
}

// ParseWatchlist decodes strictly and validates.
func ParseWatchlist(raw []byte) (Watchlist, error) {
	var w Watchlist
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&w); err != nil && !errors.Is(err, io.EOF) {
		return Watchlist{}, fmt.Errorf("%s: %w", WatchFile, err)
	}
	if err := w.Validate(); err != nil {
		return Watchlist{}, fmt.Errorf("%s: %w", WatchFile, err)
	}
	return w, nil
}
