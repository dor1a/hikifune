package hostlock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOneHolderAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := Try(path)
	if err != nil {
		t.Fatal(err)
	}
	// Readable by all, so a user other than the one who made it can lock it.
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("lock file mode %v, %v", fi.Mode(), err)
	}
	if _, err := Try(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Try: %v, want ErrBusy", err)
	}
	a.Release()
	b, err := Try(path)
	if err != nil {
		t.Fatalf("after Release: %v", err)
	}
	b.Release()
	var none *Lock
	none.Release()
	if _, err := Try(filepath.Join(t.TempDir(), "missing", "lock")); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("a lock file that cannot be made: %v", err)
	}
	if h := Hint(path, fs.ErrPermission); !strings.Contains(h, "remove it") {
		t.Errorf("hint for a file another user made: %s", h)
	}
}
