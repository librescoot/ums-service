package maps

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// spaceHeadroom is left free on the DBC on top of everything the install needs
// at its peak. The compressed path holds three files at once for a moment: the
// tiles.tar already installed, the uploaded .tar.zst, and the .tar.tmp zstd is
// writing. The installed one is already counted as used space by df, so only
// the latter two are added on top, plus this margin for the rest of /data.
const spaceHeadroom = 64 * 1024 * 1024

// zstdMagic is the little-endian Magic_Number that opens every zstd frame.
const zstdMagic = 0xFD2FB528

// errFrameTruncated stands in for the various short-read errors io.ReadFull can
// hand back, so callers and tests have one thing to match on. A frame that
// carries no content size at all is a different matter and comes back through
// the known return instead, because it is legal and not an error.
var errFrameTruncated = errors.New("zstd frame header is truncated")

// zstdFrameContentSize reads a zstd frame header off r and returns the
// decompressed length the frame declares.
//
// known is false when the frame carries no Frame_Content_Size at all, which is
// legal and happens when the input was a stream rather than a file. The zstd
// CLI compressing a regular file always writes one, which is how the tile
// archives are produced, so the caller treats an absent size as "cannot check"
// rather than as an error.
//
// Only the header is consumed, so r may be a file opened purely for this.
func zstdFrameContentSize(r io.Reader) (int64, bool, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return 0, false, fmt.Errorf("zstd magic number: %w", truncated(err))
	}
	if got := binary.LittleEndian.Uint32(magic[:]); got != zstdMagic {
		return 0, false, fmt.Errorf("not a zstd frame: magic 0x%08X, want 0x%08X", got, uint32(zstdMagic))
	}

	var descByte [1]byte
	if _, err := io.ReadFull(r, descByte[:]); err != nil {
		return 0, false, fmt.Errorf("zstd frame header descriptor: %w", truncated(err))
	}
	desc := descByte[0]

	// Frame_Header_Descriptor layout, high bit first:
	//   7-6 Frame_Content_Size_flag
	//   5   Single_Segment_flag
	//   4   Unused
	//   3   Reserved, must be zero
	//   2   Content_Checksum_flag
	//   1-0 Dictionary_ID_flag
	fcsFlag := desc >> 6
	singleSegment := desc&0x20 != 0
	if desc&0x08 != 0 {
		return 0, false, errors.New("zstd frame header has the reserved bit set")
	}

	// Window_Descriptor is present only when Single_Segment_flag is clear, then
	// a Dictionary_ID of 0, 1, 2 or 4 bytes. Neither is needed here, so skip
	// past both to reach the Frame_Content_Size.
	skip := 0
	if !singleSegment {
		skip++
	}
	switch desc & 0x03 {
	case 1:
		skip += 1
	case 2:
		skip += 2
	case 3:
		skip += 4
	}
	if skip > 0 {
		if _, err := io.ReadFull(r, make([]byte, skip)); err != nil {
			return 0, false, fmt.Errorf("zstd window/dictionary fields: %w", truncated(err))
		}
	}

	// FCS_Field_Size from the flag. The 0 row is the awkward one: no field at
	// all normally, but one byte when Single_Segment_flag is set, because a
	// single-segment frame has no window descriptor and the decoder needs the
	// content size to know how much to allocate.
	fcsSize := 0
	switch fcsFlag {
	case 0:
		if singleSegment {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	if fcsSize == 0 {
		return 0, false, nil
	}

	fcs := make([]byte, fcsSize)
	if _, err := io.ReadFull(r, fcs); err != nil {
		return 0, false, fmt.Errorf("zstd frame content size: %w", truncated(err))
	}
	switch fcsSize {
	case 1:
		return int64(fcs[0]), true, nil
	case 2:
		// The 2-byte form is offset by 256: a size that small would have used
		// the 1-byte form, so the range starts where that one leaves off.
		return int64(binary.LittleEndian.Uint16(fcs)) + 256, true, nil
	case 4:
		return int64(binary.LittleEndian.Uint32(fcs)), true, nil
	default:
		v := binary.LittleEndian.Uint64(fcs)
		if v > math.MaxInt64 {
			return 0, false, fmt.Errorf("zstd frame content size %d does not fit in an int64", v)
		}
		return int64(v), true, nil
	}
}

func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errFrameTruncated
	}
	return err
}

// parseDfAvailableBytes pulls the available byte count out of `df -k` output.
//
// The format is not as fixed as it looks: coreutils wraps a long device name
// onto its own line so the numbers start the second line, busybox does not, and
// RunCommand hands back stdout and stderr combined so an ssh warning can be
// sitting above either. Rather than counting columns, find the capacity field
// (the only one ending in %) and take the column before it, which is Available
// in every layout. Sizes are 1K blocks because of -k.
func parseDfAvailableBytes(out string) (int64, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if i == 0 || !strings.HasSuffix(f, "%") {
				continue
			}
			if _, err := strconv.Atoi(strings.TrimSuffix(f, "%")); err != nil {
				continue
			}
			blocks, err := strconv.ParseInt(fields[i-1], 10, 64)
			if err != nil {
				continue
			}
			return blocks * 1024, nil
		}
	}
	return 0, fmt.Errorf("no usable df line in %q", out)
}

// dbcAvailableBytes asks the DBC how much room is left on the filesystem
// holding dir. -k pins the block size at 1K and -P asks for the POSIX layout,
// which keeps each filesystem on one line; busybox ignores -P but its own
// layout parses the same way.
func (u *Updater) dbcAvailableBytes(ctx context.Context, dir string) (int64, error) {
	out, err := u.dbcInterface.RunCommand(ctx, fmt.Sprintf("df -Pk %s", dir))
	if err != nil {
		return 0, err
	}
	return parseDfAvailableBytes(out)
}

// checkDBCSpace refuses an install that plainly will not fit before anything is
// transferred, so a full upload is not spent to arrive at a raw zstd error.
//
// Every way the check itself can fail is a skip, not a refusal. A df that does
// not answer, output in a shape nothing here recognises, or an archive with no
// declared content size all mean the check has no opinion, and an update that
// would have worked must not be blocked by that.
func (u *Updater) checkDBCSpace(ctx context.Context, localPath string, compressed bool) error {
	fi, err := os.Stat(localPath)
	if err != nil {
		log.Printf("Skipping the DBC free-space check: cannot stat %s: %v", localPath, err)
		return nil
	}

	required := fi.Size() + spaceHeadroom
	if compressed {
		f, err := os.Open(localPath)
		if err != nil {
			log.Printf("Skipping the DBC free-space check: cannot open %s: %v", localPath, err)
			return nil
		}
		size, known, err := zstdFrameContentSize(f)
		closeErr := f.Close()
		switch {
		case err != nil:
			log.Printf("Skipping the DBC free-space check: cannot read the zstd header of %s: %v", localPath, err)
			return nil
		case !known:
			log.Printf("Skipping the DBC free-space check: %s declares no uncompressed size", localPath)
			return nil
		case closeErr != nil:
			log.Printf("Skipping the DBC free-space check: cannot close %s: %v", localPath, closeErr)
			return nil
		}
		required += size
	}

	available, err := u.dbcAvailableBytes(ctx, u.dbcValhallaDir)
	if err != nil {
		log.Printf("Skipping the DBC free-space check: cannot read free space on %s: %v", u.dbcValhallaDir, err)
		return nil
	}

	if available < required {
		return fmt.Errorf("not enough space on the DBC for %s: needs %d MB in %s (%d MB archive, %d MB unpacked, %d MB headroom), %d MB free",
			filepath.Base(localPath), toMB(required), u.dbcValhallaDir,
			toMB(fi.Size()), toMB(required-fi.Size()-spaceHeadroom), toMB(spaceHeadroom),
			toMB(available))
	}

	log.Printf("DBC free-space check passed: %s needs %d MB in %s, %d MB free",
		filepath.Base(localPath), toMB(required), u.dbcValhallaDir, toMB(available))
	return nil
}

func toMB(b int64) int64 { return b / (1024 * 1024) }
