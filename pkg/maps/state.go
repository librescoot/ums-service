package maps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The dashboard's record of what is installed, on the DBC. scootui-qt owns the
// format (src/models/MapMetadata.h); writing the same shape here is what stops
// it re-deriving everything from scratch, which it can only do with network
// access to the release manifest.
const dbcMetadataPath = "/data/maps/metadata.json"

// The Redis hash mirroring the above onto the MDB, so a consumer can read what
// is installed without powering the DBC up. scootui-qt republishes it on every
// boot and reconnect; this fills it in from the moment of a USB install, which
// may be well before the dashboard next runs.
const mapsHash = "maps"

// TileInfo is one artifact's entry in metadata.json.
type TileInfo struct {
	Digest      string `json:"digest"`
	PublishedAt string `json:"publishedAt,omitempty"`
	Size        int64  `json:"size"`
}

// Metadata is /data/maps/metadata.json. The omitempty tags match what
// scootui-qt's MapMetadata::toJson leaves out, so a round trip through here
// does not rewrite the file into a shape it would not have produced.
type Metadata struct {
	Region          string    `json:"region,omitempty"`
	DisplayTiles    *TileInfo `json:"displayTiles,omitempty"`
	ValhallaTiles   *TileInfo `json:"valhallaTiles,omitempty"`
	LastUpdateCheck string    `json:"lastUpdateCheck,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable,omitempty"`
}

// RegionFromFilename recovers the region slug from a published artifact name:
// tiles_<slug>.mbtiles, valhalla_tiles_<slug>.tar, valhalla_tiles_<slug>.tar.zst.
//
// Returns "" for anything else, which includes the two generic names the USB
// flow also accepts (map.mbtiles, tiles.tar). An empty result is not a failure:
// it means the region cannot be known from the file, and the caller clears the
// recorded region rather than leaving the previous one to describe tiles it no
// longer refers to.
func RegionFromFilename(filename string) string {
	base := strings.TrimSuffix(filepath.Base(filename), ".zst")

	if slug, ok := strings.CutSuffix(base, ".mbtiles"); ok {
		if region, found := strings.CutPrefix(slug, "tiles_"); found {
			return region
		}
		return ""
	}

	if slug, ok := strings.CutSuffix(base, ".tar"); ok {
		if region, found := strings.CutPrefix(slug, "valhalla_tiles_"); found {
			return region
		}
	}
	return ""
}

// remoteFileFacts is what the DBC can tell us about an installed artifact.
// Size and mtime are what a consumer needs to tell "installed" from "absent";
// the digest is what identifies the build.
type remoteFileFacts struct {
	Digest string
	Size   int64
	MTime  time.Time
}

// statAndHashRemote reads back the artifact the DBC just received. The hash is
// computed there rather than over the local file because for a .tar.zst the
// installed file is the decompressed tar, whose digest the MDB never sees.
//
// One pass over a few hundred MB on the DBC costs seconds. That is bounded
// separately from the transfer so a slow hash cannot eat the caller's budget.
func (u *Updater) statAndHashRemote(ctx context.Context, remotePath string) (*remoteFileFacts, error) {
	opCtx, cancel := context.WithTimeout(ctx, remoteFactsTimeout)
	defer cancel()

	// One round trip: size, mtime, then the digest on its own line.
	cmd := fmt.Sprintf("stat -c '%%s %%Y' %s && sha256sum %s | cut -d' ' -f1", remotePath, remotePath)
	out, err := u.dbcInterface.RunCommand(opCtx, cmd)
	if err != nil {
		return nil, fmt.Errorf("reading back %s: %w", remotePath, err)
	}

	lines := strings.Fields(strings.ReplaceAll(strings.TrimSpace(out), "\n", " "))
	if len(lines) != 3 {
		return nil, fmt.Errorf("unexpected output for %s: %q", remotePath, out)
	}

	size, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing size of %s: %w", remotePath, err)
	}
	epoch, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing mtime of %s: %w", remotePath, err)
	}

	return &remoteFileFacts{Digest: lines[2], Size: size, MTime: time.Unix(epoch, 0).UTC()}, nil
}

// remoteFactsTimeout bounds the stat + sha256 read-back. Hashing 500 MB on the
// DBC is the worst case here and lands well inside this.
const remoteFactsTimeout = 5 * time.Minute

func (u *Updater) readRemoteMetadata(ctx context.Context) *Metadata {
	// A vehicle whose tiles were flashed has no metadata.json at all, so a
	// missing file is the normal case and not worth reporting as an error.
	out, err := u.dbcInterface.RunCommand(ctx, "cat "+dbcMetadataPath+" 2>/dev/null || true")
	if err != nil || strings.TrimSpace(out) == "" {
		return &Metadata{}
	}

	var meta Metadata
	if err := json.Unmarshal([]byte(out), &meta); err != nil {
		log.Printf("Ignoring unparseable %s on the DBC: %v", dbcMetadataPath, err)
		return &Metadata{}
	}
	return &meta
}

func (u *Updater) writeRemoteMetadata(ctx context.Context, meta *Metadata) error {
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}

	// Single-quoted for the remote shell. Safe because JSON quotes with double
	// quotes and every value here is a hex digest, a region slug or an integer,
	// none of which can contain an apostrophe. Checked rather than assumed,
	// because the failure mode is a mangled file rather than an error.
	//
	// base64 would be the general answer and is deliberately not used: the
	// dashboard's busybox has no base64 applet, the same trap the missing
	// timeout applet already sprang once.
	if bytes.ContainsRune(payload, '\'') {
		return fmt.Errorf("refusing to write %s: encoded metadata contains an apostrophe", dbcMetadataPath)
	}

	// Temp file plus mv so a dropped connection cannot leave the dashboard
	// reading a half-written record.
	cmd := fmt.Sprintf(
		"mkdir -p %s && printf '%%s' '%s' > %s.tmp && mv -f %s.tmp %s && sync",
		filepath.Dir(dbcMetadataPath), payload, dbcMetadataPath, dbcMetadataPath, dbcMetadataPath)

	if _, err := u.dbcInterface.RunCommand(ctx, cmd); err != nil {
		return fmt.Errorf("writing %s: %w", dbcMetadataPath, err)
	}
	return nil
}

// recordInstall updates the DBC's metadata.json and the MDB's `maps` hash to
// describe an artifact that was just installed.
//
// Failures here are logged, never returned: the tiles are on the DBC and
// working, and losing the bookkeeping costs a re-derivation by the dashboard,
// not a broken install.
func (u *Updater) recordInstall(ctx context.Context, isDisplay bool, sourceName, remotePath string) {
	facts, err := u.statAndHashRemote(ctx, remotePath)
	if err != nil {
		log.Printf("Could not record what was installed: %v", err)
		return
	}

	meta := u.readRemoteMetadata(ctx)

	info := &TileInfo{Digest: facts.Digest, Size: facts.Size}
	if isDisplay {
		meta.DisplayTiles = info
	} else {
		meta.ValhallaTiles = info
	}

	// The region comes from the published filename. When the file was renamed
	// to one of the generic names the USB flow also accepts, we cannot know it,
	// and keeping the previous value would have it describe tiles it no longer
	// refers to. Clearing it makes the dashboard re-identify the region from
	// the manifest next time it has network.
	meta.Region = RegionFromFilename(sourceName)

	// Whatever the last update check concluded was about the tiles that were
	// installed before this one. Clearing both forces a fresh check, which is
	// also what re-establishes the region when the filename could not give it.
	meta.UpdateAvailable = false
	meta.LastUpdateCheck = ""

	if err := u.writeRemoteMetadata(ctx, meta); err != nil {
		log.Printf("Could not record what was installed: %v", err)
		return
	}

	u.publishState(meta, isDisplay, facts)
	log.Printf("Recorded installed %s tiles: region %q, sha256 %s",
		tileKind(isDisplay), meta.Region, facts.Digest)
}

func tileKind(isDisplay bool) string {
	if isDisplay {
		return "display"
	}
	return "routing"
}

// publishState mirrors the install into the `maps` hash. Only the half that
// changed is written: the other artifact's fields are whatever the dashboard or
// an earlier install left there, and this has not re-read them.
func (u *Updater) publishState(meta *Metadata, isDisplay bool, facts *remoteFileFacts) {
	if u.client == nil {
		return
	}

	prefix := "map"
	if !isDisplay {
		prefix = "routing"
	}

	set := func(field, value string) {
		if err := u.client.HSet(mapsHash, field, value); err != nil {
			log.Printf("Could not publish %s[%s]: %v", mapsHash, field, err)
			return
		}
		// Matches the platform's HSET-then-PUBLISH-<field> convention, so a
		// live subscriber sees the change rather than only the next poller.
		if _, err := u.client.Publish(mapsHash, field); err != nil {
			log.Printf("Could not announce %s[%s]: %v", mapsHash, field, err)
		}
	}
	clear := func(field string) {
		if _, err := u.client.Do("HDEL", mapsHash, field); err != nil {
			log.Printf("Could not clear %s[%s]: %v", mapsHash, field, err)
			return
		}
		if _, err := u.client.Publish(mapsHash, field); err != nil {
			log.Printf("Could not announce %s[%s]: %v", mapsHash, field, err)
		}
	}

	set(prefix+":sha256", facts.Digest)
	set(prefix+":size", strconv.FormatInt(facts.Size, 10))
	set(prefix+":mtime", facts.MTime.Format(time.RFC3339))
	// A USB install has no manifest to consult, so there is no upstream
	// publication date to report. Drop a stale one rather than let it describe
	// the artifact that was just replaced.
	clear(prefix + ":published-at")

	if meta.Region != "" {
		set("region", meta.Region)
	} else {
		clear("region")
	}
	// The display name is the dashboard's to fill in; it owns the slug-to-name
	// table. Clearing it keeps a name from the previous region off the hash.
	clear("region-name")

	clear("last-update-check")
	set("update-available", "false")
	set("updated-at", time.Now().UTC().Format(time.RFC3339))
}
