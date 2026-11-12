package main

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// moveAside renames a state file that does not parse, so the next save does
// not overwrite it with an empty state. It can then be fixed by hand.
func moveAside(path string, err error) {
	bad := path + ".bad-" + time.Now().Format("20060102-150405")
	if os.Rename(path, bad) == nil {
		log.Printf("[state] %s did not parse (%v); moved to %s", path, err, bad)
	}
}

// writeFileAtomic writes data to a temp file next to path and renames it
// over path, so a reader or a crash never sees a half-written file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
