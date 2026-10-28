package pacelever

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrBusy = errors.New("operation already in progress")

// WithFileLock uses an advisory kernel lock, released even after SIGKILL.
// The lock inode is never unlinked, preventing competing lock generations.
func WithFileLock(path string, nonblock bool, fn func() error) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	op := syscall.LOCK_EX
	if nonblock {
		op |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), op); e != nil {
		if errors.Is(e, syscall.EWOULDBLOCK) {
			return ErrBusy
		}
		return e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
func AtomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".pacelever-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}
func WriteJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return AtomicWrite(path, append(b, '\n'))
}
func ReadJSON(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if len(b) > 32<<20 {
		return fmt.Errorf("state file too large")
	}
	if e = json.Unmarshal(b, v); e != nil {
		return fmt.Errorf("invalid JSON state")
	}
	return nil
}
