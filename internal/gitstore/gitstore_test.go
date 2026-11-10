package gitstore

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain turns off auto maintenance: its detached run writes into
// objects/ while TempDir cleanup removes the repo.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_COUNT", "2")
	os.Setenv("GIT_CONFIG_KEY_0", "maintenance.auto")
	os.Setenv("GIT_CONFIG_VALUE_0", "false")
	os.Setenv("GIT_CONFIG_KEY_1", "gc.auto")
	os.Setenv("GIT_CONFIG_VALUE_1", "0")
	os.Exit(m.Run())
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote makes a bare repo with one commit on main.
func remote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "bots.git")
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	// Local push strips GIT_CONFIG_*, so receive-pack needs its own.
	run(t, bare, "config", "receive.autogc", "false")
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "bots"), 0o755)
	os.WriteFile(filepath.Join(seed, "bots", "trend.yaml"), []byte("name: trend\n"), 0o644)
	run(t, seed, "add", ".")
	run(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "seed")
	run(t, seed, "push", "-q", "origin", "HEAD:main")
	return bare
}

func TestChangeCommitsAndPushes(t *testing.T) {
	bare := remote(t)
	s := &Store{URL: bare, Dir: filepath.Join(t.TempDir(), "rw")}
	ctx := context.Background()
	if err := s.Change(ctx, "bots/probe.yaml", []byte("name: probe\n"), "feat: alice creates probe", "alice <alice@wickfleet.invalid>"); err != nil {
		t.Fatal(err)
	}
	log := run(t, bare, "log", "--format=%an|%s", "main")
	if !strings.HasPrefix(log, "alice|feat: alice creates probe") {
		t.Fatalf("log %q", log)
	}
	if err := s.Change(ctx, "bots/probe.yaml", []byte("name: probe\n"), "again", "alice <a@x>"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("same content must be ErrNoChange, got %v", err)
	}
	if b, err := s.Read(ctx, "bots/trend.yaml"); err != nil || string(b) != "name: trend\n" {
		t.Fatalf("read %q %v", b, err)
	}
	if err := s.Change(ctx, "bots/probe.yaml", nil, "feat: alice removes probe", "alice <a@x>"); err != nil {
		t.Fatal(err)
	}
	if files := run(t, bare, "ls-tree", "-r", "--name-only", "main"); strings.Contains(files, "probe") {
		t.Fatalf("probe still in tree: %s", files)
	}
	if err := s.Change(ctx, "bots/probe.yaml", nil, "x", "a <a@x>"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("deleting a missing file must be ErrNoChange, got %v", err)
	}
}

func TestChangeRebasesOnRemoteMoves(t *testing.T) {
	bare := remote(t)
	s := &Store{URL: bare, Dir: filepath.Join(t.TempDir(), "rw")}
	ctx := context.Background()
	if err := s.Change(ctx, "bots/a.yaml", []byte("name: a\n"), "a", "x <x@x>"); err != nil {
		t.Fatal(err)
	}
	// Someone else pushes in between; the next change must land on top.
	other := filepath.Join(t.TempDir(), "other")
	run(t, filepath.Dir(other), "clone", "-q", bare, other)
	os.WriteFile(filepath.Join(other, "bots", "b.yaml"), []byte("name: b\n"), 0o644)
	run(t, other, "add", ".")
	run(t, other, "-c", "user.name=o", "-c", "user.email=o@o", "commit", "-q", "-m", "b")
	run(t, other, "push", "-q", "origin", "HEAD:main")
	if err := s.Change(ctx, "bots/c.yaml", []byte("name: c\n"), "c", "x <x@x>"); err != nil {
		t.Fatal(err)
	}
	files := run(t, bare, "ls-tree", "-r", "--name-only", "main")
	for _, f := range []string{"bots/a.yaml", "bots/b.yaml", "bots/c.yaml", "bots/trend.yaml"} {
		if !strings.Contains(files, f) {
			t.Fatalf("%s missing after concurrent push: %s", f, files)
		}
	}
}

func TestRejectsTraversal(t *testing.T) {
	s := &Store{URL: remote(t), Dir: t.TempDir()}
	for _, p := range []string{"../x", "/etc/passwd", "bots/../../x"} {
		if err := s.Change(context.Background(), p, []byte("x"), "m", "a <a@x>"); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
}

func TestChangeAllIsOneCommit(t *testing.T) {
	bare := remote(t)
	s := &Store{URL: bare, Dir: filepath.Join(t.TempDir(), "rw")}
	ctx := context.Background()
	before := run(t, bare, "rev-list", "--count", "main")
	err := s.ChangeAll(ctx, map[string][]byte{
		"strategies/X.json": []byte("{}\n"),
		"strategies/X.py":   []byte("class X: pass\n"),
		"bots/missing.yaml": nil, // deleting what is not there is no change
	}, "feat: a adds X", "a <a@x>")
	if err != nil {
		t.Fatal(err)
	}
	if after := run(t, bare, "rev-list", "--count", "main"); after == before {
		t.Fatal("no commit")
	} else if run(t, bare, "show", "--stat", "--format=%s", "main") == "" {
		t.Fatal("empty show")
	}
	files := run(t, bare, "show", "--name-only", "--format=", "main")
	if files != "strategies/X.json\nstrategies/X.py" {
		t.Fatalf("one commit must carry both files, got %q", files)
	}
	if err := s.ChangeAll(ctx, map[string][]byte{"strategies/X.json": []byte("{}\n")}, "again", "a <a@x>"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("same content: %v", err)
	}
	if err := s.ChangeAll(ctx, map[string][]byte{"strategies/X.json": nil, "strategies/X.py": nil}, "rm", "a <a@x>"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, bare, "ls-tree", "-r", "--name-only", "main"); strings.Contains(got, "strategies/X") {
		t.Fatalf("still there: %s", got)
	}
	if err := s.ChangeAll(ctx, map[string][]byte{"a/../../x": []byte("x")}, "m", "a <a@x>"); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestChangeNeverTruncatesUnderAReader(t *testing.T) {
	s := &Store{URL: remote(t), Dir: filepath.Join(t.TempDir(), "rw")}
	ctx := context.Background()
	if _, err := s.Read(ctx, "bots/trend.yaml"); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(filepath.Join(s.Dir, "bots", "trend.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := s.Change(ctx, "bots/trend.yaml", []byte("name: trend\nwallet: 500\n"), "m", "a <a@x>"); err != nil {
		t.Fatal(err)
	}
	// The hub may read this checkout while it commits (a controller does).
	if old, _ := io.ReadAll(r); string(old) != "name: trend\n" {
		t.Fatalf("open reader got %q, want the old file whole", old)
	}
}
