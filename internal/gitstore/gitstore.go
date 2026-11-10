// Package gitstore writes registry changes as commits. It shells out to git
// with a repo-scoped deploy key; one working clone, one writer at a time.
package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wickfleet/internal/atomicfile"
)

// Store is a working clone of the registry repo.
type Store struct {
	URL        string // e.g. ssh://git@host/owner/repo.git, or a path in tests
	Branch     string // default main
	Dir        string // working clone, owned by the hub
	KeyFile    string // deploy key with write access; empty = ambient ssh config
	KnownHosts string // pinned host keys for KeyFile

	mu sync.Mutex
}

// ErrNoChange means the file already had that content.
var ErrNoChange = errors.New("no change")

func (s *Store) branch() string {
	if s.Branch == "" {
		return "main"
	}
	return s.Branch
}

func (s *Store) git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "HOME="+s.Dir)
	if s.KeyFile != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf(
			"GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -F /dev/null",
			s.KeyFile, s.KnownHosts))
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// sync makes the clone exactly the remote branch.
func (s *Store) sync(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.Dir, ".git")); err != nil {
		if err := os.MkdirAll(s.Dir, 0o750); err != nil {
			return err
		}
		if _, err := s.git(ctx, "clone", "--branch", s.branch(), s.URL, "."); err != nil {
			return err
		}
	}
	if _, err := s.git(ctx, "fetch", "origin", s.branch()); err != nil {
		return err
	}
	_, err := s.git(ctx, "reset", "--hard", "origin/"+s.branch())
	return err
}

// Read returns a file at the remote head.
func (s *Store) Read(ctx context.Context, rel string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sync(ctx); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.Dir, rel))
}

// Change writes (data != nil) or deletes (data == nil) rel and pushes one
// commit. A push race is retried once on top of the new remote head.
func (s *Store) Change(ctx context.Context, rel string, data []byte, msg, author string) error {
	return s.ChangeAll(ctx, map[string][]byte{rel: data}, msg, author)
}

// ChangeAll writes or deletes several files in one commit; ErrNoChange when
// none of them would change.
func (s *Store) ChangeAll(ctx context.Context, files map[string][]byte, msg, author string) error {
	rels := make([]string, 0, len(files))
	for rel := range files {
		if rel == "" || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("bad path %q", rel)
		}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = s.sync(ctx); err != nil {
			return err
		}
		changed := 0
		for _, rel := range rels {
			data, path := files[rel], filepath.Join(s.Dir, rel)
			if data == nil {
				if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
					continue
				}
				if _, err = s.git(ctx, "rm", "-q", rel); err != nil {
					return err
				}
			} else {
				if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					return err
				}
				if old, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(old, data) {
					continue
				}
				if err = atomicfile.Write(path, data, 0o640); err != nil {
					return err
				}
				if _, err = s.git(ctx, "add", rel); err != nil {
					return err
				}
			}
			changed++
		}
		if changed == 0 {
			return ErrNoChange
		}
		if _, err = s.git(ctx, "-c", "user.name=wickfleet-hub", "-c", "user.email=hub@wickfleet.invalid",
			"commit", "-q", "--author", author, "-m", msg); err != nil {
			return err
		}
		if _, err = s.git(ctx, "push", "-q", "origin", "HEAD:"+s.branch()); err == nil {
			return nil
		}
	}
	return err
}
