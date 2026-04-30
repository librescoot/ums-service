package update

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.7.0", "v0.10.0", -1},
		{"v0.10.0", "v0.7.0", 1},
		{"v0.8.0", "v0.8.0", 0},
		{"20260101T120000", "20260415T120000", -1},
		{"20260415T120000", "20260101T120000", 1},
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSplitVersion(t *testing.T) {
	cases := []struct {
		in             string
		wantKey, wantV string
	}{
		{"librescoot-foo-mdb-nightly-20260415T120000.mender", "librescoot-foo-mdb-nightly", "20260415T120000"},
		{"librescoot-foo-mdb-stable-v0.10.0.mender", "librescoot-foo-mdb-stable", "v0.10.0"},
		{"librescoot-foo-mdb-stable-v0.10.0.delta", "librescoot-foo-mdb-stable", "v0.10.0"},
	}
	for _, c := range cases {
		k, v := splitVersion(c.in)
		if k != c.wantKey || v != c.wantV {
			t.Errorf("splitVersion(%q) = (%q,%q), want (%q,%q)", c.in, k, v, c.wantKey, c.wantV)
		}
	}
}

// TestCleanupStaleFiles exercises the full sweep against a temp /data/ota tree.
func TestCleanupStaleFiles(t *testing.T) {
	root := t.TempDir()
	mdb := filepath.Join(root, "mdb")
	dbc := filepath.Join(root, "dbc")
	mdbBoot := filepath.Join(root, "mdb-boot")
	dbcBoot := filepath.Join(root, "dbc-boot")
	tmp := filepath.Join(root, "tmp")
	random := filepath.Join(root, "random")
	for _, d := range []string{mdb, dbc, mdbBoot, dbcBoot, tmp, random} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	must := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// mdb: 3 stable semver + 3 nightly timestamps; expect newest of each kept.
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.7.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.8.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.10.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260101T120000.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260229T120000.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260415T120000.mender"))
	// non-update file in mdb stays
	must(filepath.Join(mdb, "notes.txt"))

	// mdb-boot: 7 entries, expect newest 5 kept
	for _, day := range []string{"01", "02", "03", "04", "05", "06", "07"} {
		must(filepath.Join(mdbBoot, "librescoot-mdb-boot-nightly-202601"+day+"T120000.mender"))
	}

	// orphans (under root, tmp/, random/) should all be deleted
	must(filepath.Join(root, "loose.mender"))
	must(filepath.Join(tmp, "leftover.delta"))
	must(filepath.Join(random, "wandered.mender"))
	// non-update files at root stay
	must(filepath.Join(root, "some.service"))
	must(filepath.Join(tmp, "scratch.txt"))

	l := &Loader{
		otaRootDir: root,
		otaDir:     mdb,
		dbcOtaDir:  dbc,
		managedDirs: []managedDir{
			{mdb, 1},
			{dbc, 1},
			{mdbBoot, 5},
			{dbcBoot, 5},
		},
	}

	if err := l.CleanupStaleFiles(); err != nil {
		t.Fatalf("CleanupStaleFiles: %v", err)
	}

	got := walkRel(t, root)
	sort.Strings(got)
	want := []string{
		"dbc",
		"dbc-boot",
		"mdb",
		"mdb-boot",
		"mdb-boot/librescoot-mdb-boot-nightly-20260103T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260104T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260105T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260106T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260107T120000.mender",
		"mdb/librescoot-foo-mdb-nightly-20260415T120000.mender",
		"mdb/librescoot-foo-mdb-stable-v0.10.0.mender",
		"mdb/notes.txt",
		"random",
		"some.service",
		"tmp",
		"tmp/scratch.txt",
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("post-cleanup tree mismatch.\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCleanupStaleFilesPostCycleSkipsMdbDbc(t *testing.T) {
	root := t.TempDir()
	mdb := filepath.Join(root, "mdb")
	dbc := filepath.Join(root, "dbc")
	mdbBoot := filepath.Join(root, "mdb-boot")
	dbcBoot := filepath.Join(root, "dbc-boot")
	for _, d := range []string{mdb, dbc, mdbBoot, dbcBoot} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.7.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.10.0.mender"))
	must(filepath.Join(root, "loose.mender"))
	for _, day := range []string{"01", "02", "03", "04", "05", "06", "07"} {
		must(filepath.Join(mdbBoot, "librescoot-mdb-boot-nightly-202601"+day+"T120000.mender"))
	}

	l := &Loader{
		otaRootDir: root,
		otaDir:     mdb,
		dbcOtaDir:  dbc,
		managedDirs: []managedDir{
			{mdb, 1},
			{dbc, 1},
			{mdbBoot, 5},
			{dbcBoot, 5},
		},
	}
	if err := l.CleanupStaleFilesPostCycle(); err != nil {
		t.Fatalf("CleanupStaleFilesPostCycle: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "loose.mender")); !os.IsNotExist(err) {
		t.Errorf("orphan loose.mender should be removed")
	}
	// Both mdb files preserved (no pruning post-cycle)
	for _, p := range []string{
		filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.7.0.mender"),
		filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.10.0.mender"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("mdb file %s was unexpectedly removed: %v", filepath.Base(p), err)
		}
	}
	// mdb-boot pruned to 5 (post-cycle still prunes boot dirs)
	entries, _ := os.ReadDir(mdbBoot)
	if len(entries) != 5 {
		t.Errorf("mdb-boot should have 5 entries, got %d", len(entries))
	}
}

func walkRel(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
