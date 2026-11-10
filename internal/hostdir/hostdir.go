// Package hostdir makes host directories that containers running as another
// user share with the hub through the group.
package hostdir

import (
	"os"
	"syscall"
)

// GroupWritable adds group rwx and setgid to a dir the hub owns; MkdirAll
// honours the umask. Dirs owned by someone else are left alone.
func GroupWritable(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != os.Getuid() || st.Mode().Perm()&0o070 == 0o070 {
		return nil
	}
	return os.Chmod(dir, st.Mode().Perm()|0o070|os.ModeSetgid)
}

// Make creates dir and its parents and opens it to the group.
func Make(dir string) error {
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	return GroupWritable(dir)
}
