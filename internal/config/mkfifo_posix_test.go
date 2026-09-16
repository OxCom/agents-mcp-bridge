//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

func TestNonRegularConfigRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "config.yaml")
	if err := syscallMkfifo(fifo); err != nil {
		t.Skipf("fifo unavailable: %v", err)
	}
	// Opening a fifo for reading blocks without a writer, so a writer is
	// attached; the point is that the loader refuses a non-regular file.
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err == nil {
			_, _ = w.WriteString(minimal)
			_ = w.Close()
		}
	}()
	_, err := Load(fifo, Options{LookPath: fakeLookPath(t)})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected refusal of a non-regular config, got %v", err)
	}
}
