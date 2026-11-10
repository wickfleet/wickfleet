package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadWatchlist(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadWatchlist(dir); ok || err != nil {
		t.Fatalf("missing file must be ok=false err=nil, got %v %v", ok, err)
	}
	os.WriteFile(filepath.Join(dir, WatchFile), []byte("symbols:\n  - symbol: XIC.TO\n    note: Canada core\n  - symbol: BTC-USD\n  - symbol: CADUSD=X\n  - symbol: ^GSPC\n"), 0o644)
	w, ok, err := LoadWatchlist(dir)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got := strings.Join(w.Names(), ","); got != "XIC.TO,BTC-USD,CADUSD=X,^GSPC" {
		t.Fatalf("names %s", got)
	}
	if w.Symbols[0].Note != "Canada core" {
		t.Fatalf("%+v", w.Symbols[0])
	}
	os.WriteFile(filepath.Join(dir, WatchFile), []byte(""), 0o644)
	if w, ok, err := LoadWatchlist(dir); err != nil || !ok || len(w.Symbols) != 0 {
		t.Fatalf("empty file is an empty list: %v %v %v", w, ok, err)
	}
}

func TestWatchlistRejects(t *testing.T) {
	long := strings.Repeat("x", 81)
	many := "symbols:\n"
	for i := 0; i <= MaxWatch; i++ {
		many += "  - symbol: S" + string(rune('A'+i%26)) + string(rune('A'+i/26)) + "\n"
	}
	for name, body := range map[string]string{
		"unknown key": "symbols:\n  - symbol: XIC.TO\n    rule: x\n",
		"path":        "symbols:\n  - symbol: ../etc\n",
		"query":       "symbols:\n  - symbol: XIC.TO?x=1\n",
		"space":       "symbols:\n  - symbol: XIC TO\n",
		"duplicate":   "symbols:\n  - symbol: XIC.TO\n  - symbol: XIC.TO\n",
		"long note":   "symbols:\n  - symbol: XIC.TO\n    note: " + long + "\n",
		"too many":    many,
	} {
		if _, err := ParseWatchlist([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
