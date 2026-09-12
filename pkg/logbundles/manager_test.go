package logbundles

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileCancelledLeavesNoDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	if err := os.WriteFile(src, []byte("bundle"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyFile(ctx, src, dst); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyFile error = %v, want context.Canceled", err)
	}
	assertNotExist(t, dst)
	assertNotExist(t, dst+".part")
}

func TestCopyReaderToFileInterruptedLeavesNoStrayFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "bundle.tar.gz")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := copyReaderToFile(ctx, &cancelAfterFirstRead{cancel: cancel}, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyReaderToFile error = %v, want context.Canceled", err)
	}
	assertNotExist(t, dst)
	assertNotExist(t, dst+".part")
}

type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	p[0] = 'x'
	r.cancel()
	return 1, nil
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or could not be checked: %v", path, err)
	}
}
