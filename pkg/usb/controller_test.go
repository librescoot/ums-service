package usb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableInterfaceBringsLinkUp(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > \"$ARGS_PATH\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("ARGS_PATH", argsPath)

	if err := enableInterface("usb0"); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(args)); got != "link set usb0 up" {
		t.Fatalf("ip arguments = %q", got)
	}
}
