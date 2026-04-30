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

// TestCleanupStaleFiles exercises the sweep against a temp /data/ota tree.
// /data/ota/{mdb,dbc} are owned by update-service and must never be pruned by
// ums-service: their files (including older delta-base .mender artifacts)
// must survive untouched.
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

	// mdb: multiple stable + nightly artifacts; ALL must survive (update-service owns this dir).
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.7.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.8.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-stable-v0.10.0.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260101T120000.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260229T120000.mender"))
	must(filepath.Join(mdb, "librescoot-foo-mdb-nightly-20260415T120000.mender"))
	// non-update file in mdb stays
	must(filepath.Join(mdb, "notes.txt"))

	// dbc: same — all must survive.
	must(filepath.Join(dbc, "librescoot-foo-dbc-stable-v0.7.0.mender"))
	must(filepath.Join(dbc, "librescoot-foo-dbc-stable-v0.10.0.mender"))

	// mdb-boot: 7 entries, expect newest 5 kept (boot dirs are still pruned).
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
		"dbc/librescoot-foo-dbc-stable-v0.10.0.mender",
		"dbc/librescoot-foo-dbc-stable-v0.7.0.mender",
		"mdb",
		"mdb-boot",
		"mdb-boot/librescoot-mdb-boot-nightly-20260103T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260104T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260105T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260106T120000.mender",
		"mdb-boot/librescoot-mdb-boot-nightly-20260107T120000.mender",
		"mdb/librescoot-foo-mdb-nightly-20260101T120000.mender",
		"mdb/librescoot-foo-mdb-nightly-20260229T120000.mender",
		"mdb/librescoot-foo-mdb-nightly-20260415T120000.mender",
		"mdb/librescoot-foo-mdb-stable-v0.10.0.mender",
		"mdb/librescoot-foo-mdb-stable-v0.7.0.mender",
		"mdb/librescoot-foo-mdb-stable-v0.8.0.mender",
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
