package atomicfile

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReaderKeepsTheWholeOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.yaml")
	if err := os.WriteFile(path, []byte("name: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := Write(path, []byte("name: new\nwallet: 500\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// An in-place rewrite truncates under the open reader; a rename does not.
	if old, _ := io.ReadAll(r); string(old) != "name: old\n" {
		t.Fatalf("open reader got %q, want the old file whole", old)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "name: new\nwallet: 500\n" {
		t.Fatalf("new file %q %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v, want 0640", st.Mode(), err)
	}
}

func TestNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "a.yaml"), []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	busy := filepath.Join(dir, "busy")
	os.MkdirAll(filepath.Join(busy, "x"), 0o755)
	if err := Write(busy, []byte("x\n"), 0o640); err == nil {
		t.Fatal("renaming over a full dir must fail")
	}
	if err := Write(filepath.Join(dir, "missing", "b.yaml"), []byte("x\n"), 0o640); err == nil {
		t.Fatal("a missing dir must fail")
	}
	all, _ := filepath.Glob(filepath.Join(dir, "*"))
	hidden, _ := filepath.Glob(filepath.Join(dir, ".*"))
	if len(all) != 2 || len(hidden) != 0 {
		t.Fatalf("dir holds %v %v, want a.yaml and busy only", all, hidden)
	}
}
