//go:build unix

package media

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInspectRejectsNamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe.png")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// Opening a FIFO for reading would block forever; the check must refuse
	// it from its mode alone.
	_, err := Inspect(fifo)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error = %v, want a non-regular file rejection", err)
	}
}
