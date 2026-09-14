//go:build unix

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDataDir takes an exclusive flock on DATA_DIR/api.lock for the life of the process.
func lockDataDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "api.lock"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open DATA_DIR lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("DATA_DIR is locked by another API process")
		}
		return nil, fmt.Errorf("lock DATA_DIR: %w", err)
	}
	return func() { f.Close() }, nil
}
