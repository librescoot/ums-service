package scripts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMDBScriptFailureIsReturnedAndTemporaryFileRemoved(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "scripts")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mdb.sh"), []byte("echo expected-output\nexit 23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove("/tmp/ums-mdb.sh")

	runner := New(nil)
	err := runner.ProcessScripts(context.Background(), time.Second, nil, root)
	if err == nil {
		t.Fatal("ProcessScripts succeeded, want script failure")
	}
	if !strings.Contains(err.Error(), "exit status 23") || !strings.Contains(err.Error(), "expected-output") {
		t.Fatalf("ProcessScripts error = %q", err)
	}
	if _, err := os.Stat("/tmp/ums-mdb.sh"); !os.IsNotExist(err) {
		t.Fatalf("temporary script remains: %v", err)
	}
}

func TestScriptErrorBoundsOutput(t *testing.T) {
	err := scriptError("test", context.Canceled, []byte(strings.Repeat("x", maxErrorOutput+100)))
	if len(err.Error()) > maxErrorOutput+100 {
		t.Fatalf("error is not bounded: %d bytes", len(err.Error()))
	}
}
