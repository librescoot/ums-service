package disk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The backing store is created before the service answers anything, so its
// cost is time the vehicle spends unresponsive. It has to be allocated
// sparsely rather than written out.
func TestCreateDriveFileIsSparse(t *testing.T) {
	const size = 1 << 30 // the production 1 GiB

	path := filepath.Join(t.TempDir(), "usb.drive")
	m := NewManager(path, size)

	if err := m.createDriveFile(path); err != nil {
		t.Fatalf("createDriveFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != size {
		t.Errorf("apparent size = %d, want %d", fi.Size(), size)
	}

	// st_blocks counts 512-byte blocks actually allocated. A written-out
	// gigabyte reports about two million; a sparse one reports next to none.
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t on this platform")
	}
	allocated := st.Blocks * 512
	if allocated > size/100 {
		t.Errorf("allocated %d bytes on disk for a %d byte file; expected a sparse allocation, not a full write", allocated, size)
	}
}

func TestShouldSetLabel(t *testing.T) {
	for current, want := range map[string]bool{
		"":           true, // mkfs.fat without -n
		"   ":        true,
		"NO NAME":    true,
		"no name":    true,
		"LIBRESCOOT": false, // already ours
		"MY MUSIC":   false, // renamed from the host, leave it
	} {
		if got := shouldSetLabel(current); got != want {
			t.Errorf("shouldSetLabel(%q) = %v, want %v", current, got, want)
		}
	}
}

func TestVolumeLabelFitsFAT(t *testing.T) {
	if len(volumeLabel) > 11 {
		t.Errorf("label %q is %d chars; FAT allows 11", volumeLabel, len(volumeLabel))
	}
	if volumeLabel != strings.ToUpper(volumeLabel) {
		t.Errorf("label %q must be uppercase", volumeLabel)
	}
}

func TestFATBootSectorValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drive")
	sector := make([]byte, 512)
	sector[11], sector[12] = 0, 2
	sector[13] = 8
	sector[14] = 32
	sector[16] = 2
	sector[510], sector[511] = 0x55, 0xaa
	if err := os.WriteFile(path, sector, 0o600); err != nil {
		t.Fatal(err)
	}
	valid, err := hasValidFATBootSector(path)
	if err != nil || !valid {
		t.Fatalf("valid FAT boot sector: valid=%v err=%v", valid, err)
	}
	sector[11], sector[12] = 0, 0
	if err := os.WriteFile(path, sector, 0o600); err != nil {
		t.Fatal(err)
	}
	valid, err = hasValidFATBootSector(path)
	if err != nil || valid {
		t.Fatalf("invalid FAT boot sector: valid=%v err=%v", valid, err)
	}
}

func TestFilesystemErrorExitStatuses(t *testing.T) {
	for _, code := range []int{1, 4} {
		err := &fsckFailure{err: exec.Command("sh", "-c", "exit 1").Run(), exitCode: code}
		if !hasFilesystemErrors(err) {
			t.Errorf("exit status %d should indicate filesystem errors", code)
		}
	}
	for _, code := range []int{-1, 2, 8} {
		err := &fsckFailure{err: exec.Command("sh", "-c", "exit 1").Run(), exitCode: code}
		if hasFilesystemErrors(err) {
			t.Errorf("exit status %d should indicate a tool or operational failure", code)
		}
	}
}

func TestRepairFilesystemRechecksAfterCorrection(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "checks")
	script := `#!/bin/sh
if [ "$1" = "-a" ]; then exit 1; fi
count=0
[ ! -f "` + counter + `" ] || count=$(cat "` + counter + `")
count=$((count + 1))
printf %s "$count" > "` + counter + `"
[ "$count" -gt 1 ]
`
	if err := os.WriteFile(filepath.Join(dir, "fsck.fat"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	m := NewManager(filepath.Join(dir, "usb.drive"), 1<<20)
	if err := m.checkFilesystem(); err == nil {
		t.Fatal("initial check should report an inconsistency")
	}
	if err := m.repairFilesystem(); err != nil {
		t.Fatalf("repairFilesystem: %v", err)
	}
}

// Integrity is verified when the service starts, rather than on the first UMS
// entry, where finding a corrupt drive used to block the user behind a
// recreation they could not anticipate.
func TestInitializeVerifiesAndRepairsFilesystemAtStartup(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "checks")
	script := `#!/bin/sh
count=0
[ ! -f "` + counter + `" ] || count=$(cat "` + counter + `")
count=$((count + 1))
printf %s "$count" > "` + counter + `"
if [ "$1" = "-a" ]; then exit 0; fi
if [ "$count" -eq 1 ]; then exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "fsck.fat"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	drive := filepath.Join(dir, "usb.drive")
	if err := os.WriteFile(drive, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(drive, 1<<20)
	if err := m.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// check (-n, inconsistent) -> repair (-a) -> recheck (-n, clean)
	got, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if string(got) != "3" {
		t.Fatalf("fsck invocations = %s, want 3 (check, repair, recheck)", got)
	}
}

func TestReplaceCorruptDriveKeepsOriginalWhenFormatFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usb.drive")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mkfs.fat"), []byte("#!/bin/sh\nexit 8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	m := NewManager(path, 1<<20)
	if err := m.replaceCorruptDrive(); err == nil {
		t.Fatal("replaceCorruptDrive succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("original drive changed to %q", got)
	}
}

func TestCreateDriveFileReplacesAShorterFile(t *testing.T) {
	// Replacement has to overwrite whatever is already there rather than extend it.
	path := filepath.Join(t.TempDir(), "usb.drive")
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(path, 1<<20)
	if err := m.createDriveFile(path); err != nil {
		t.Fatalf("createDriveFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 1<<20 {
		t.Errorf("size = %d, want %d", fi.Size(), 1<<20)
	}
}
