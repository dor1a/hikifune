// Package hostlock keeps one job at a time on a host across processes. The
// agent's jobs and hikifune-agent update, run by hand in the agent's
// container, take the same lock file, so the two never recreate containers
// at once (R13).
package hostlock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// ErrBusy means another process holds the lock.
var ErrBusy = errors.New("another job holds the host")

// Lock is a lock taken by Try.
type Lock struct{ f *os.File }

// Path is the lock file the agent and the update command share in the
// agent's container, which docker exec reaches. It is fixed rather than in
// TMPDIR, which docker exec -e could set apart from the agent's.
const Path = "/tmp/hikifune-agent.lock"

// Hint says what to do about err from Try when it is not ErrBusy.
func Hint(path string, err error) string {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Sprintf("%v; another user made %s without read for others, so remove it", err, path)
	}
	return err.Error()
}

// Try takes the lock at path without waiting. It returns ErrBusy when
// another process, or another Try in this one, holds it. The lock goes when
// the process ends, however it ends.
//
// The file is opened read only, which is enough for flock, and made
// readable by all: an update by hand run as root through docker exec -u 0
// must not leave a file the agent's own user cannot open. It holds nothing.
func Try(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	// Against a umask that took the read bits away; the owner only may.
	f.Chmod(0o644)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release lets the lock go. A nil lock does nothing.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
