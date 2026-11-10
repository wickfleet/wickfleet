// Package atomicfile writes a file whole or not at all: a reader of the
// directory sees the old content or the new, never an empty or torn file.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write puts data at path through a synced temp file in the same directory,
// renamed over path. The temp name is ".<base>.tmp-<n>", so a glob such as
// bots/*.yaml never lists it.
func Write(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
