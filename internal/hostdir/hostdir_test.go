package hostdir

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMakeUnderUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	d := filepath.Join(t.TempDir(), "a", "b")
	if err := Make(d); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o070 != 0o070 || st.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("mode %v, want group rwx and setgid", st.Mode())
	}
	if err := Make(d); err != nil {
		t.Fatal("making an existing dir again must be fine")
	}
}
