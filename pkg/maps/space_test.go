package maps

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Frame headers lifted byte for byte off archives produced by `zstd -19`, so
// the table is checked against what the tool actually writes rather than
// against a second reading of the spec. Only the header is kept: the parser
// stops there and never looks at the compressed blocks.
//
//	fx_tiny    zstd -19 on a 33 byte file            single segment, 1 byte FCS
//	fx_small   zstd -19 on a 990 byte file           single segment, 2 byte FCS
//	fx_med     zstd -19 on a 99000 byte file         single segment, 4 byte FCS
//	fx_big     zstd -19 on a 99000000 byte file      window descriptor, 4 byte FCS
//	fx_huge    zstd -1  on a 5000000000 byte file    window descriptor, 8 byte FCS
//	fx_dict    zstd -19 -D dict on the 99000 file    4 byte dictionary ID
//	fx_stream  zstd -19 reading stdin                no FCS at all
func TestZstdFrameContentSize(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		want    int64
		known   bool
		err     error
		wantErr bool
	}{
		{
			name:  "real zstd -19, single segment, 1 byte FCS",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x24, 0x21},
			want:  33,
			known: true,
		},
		{
			name:  "real zstd -19, single segment, 2 byte FCS",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x64, 0xde, 0x02},
			want:  990, // 0x02de is 734, and the 2 byte form is offset by 256
			known: true,
		},
		{
			name:  "real zstd -19, single segment, 4 byte FCS",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0xa4, 0xb8, 0x82, 0x01, 0x00},
			want:  99000,
			known: true,
		},
		{
			name:  "real zstd -19, window descriptor, 4 byte FCS",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x84, 0x68, 0xc0, 0x9e, 0xe6, 0x05},
			want:  99000000,
			known: true,
		},
		{
			name:  "real zstd, window descriptor, 8 byte FCS",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0xc4, 0x48, 0x00, 0xf2, 0x05, 0x2a, 0x01, 0x00, 0x00, 0x00},
			want:  5000000000,
			known: true,
		},
		{
			name:  "real zstd -19 -D, 4 byte dictionary ID skipped",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0xa7, 0x3b, 0x05, 0x41, 0x59, 0xb8, 0x82, 0x01, 0x00},
			want:  99000,
			known: true,
		},
		{
			name:  "real zstd -19 from stdin, no content size",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x04, 0x68},
			want:  0,
			known: false,
		},

		// Synthetic, for the header shapes the CLI does not produce on its own.
		{
			// desc 0x21: FCS flag 0, single segment, 1 byte dictionary ID.
			name:  "1 byte dictionary ID skipped",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x21, 0x07, 0x2a},
			want:  42,
			known: true,
		},
		{
			// desc 0x62: FCS flag 1, single segment, 2 byte dictionary ID.
			name:  "2 byte dictionary ID skipped",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x62, 0x34, 0x12, 0x00, 0x01},
			want:  512, // 0x0100 is 256, plus the 256 offset
			known: true,
		},
		{
			name:  "2 byte FCS at its minimum is 256, not 0",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x60, 0x00, 0x00},
			want:  256,
			known: true,
		},
		{
			// desc 0x02: FCS flag 0 and single segment clear means no FCS
			// field at all, even though a dictionary ID follows the window.
			name:  "no content size, window and dictionary ID still consumed",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x02, 0x68, 0x34, 0x12},
			want:  0,
			known: false,
		},
		{
			name:  "1 byte FCS of zero is a known empty file",
			in:    []byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x00},
			want:  0,
			known: true,
		},

		// Rejections.
		{
			name:    "wrong magic number",
			wantErr: true,
			in:      []byte{0x28, 0xb5, 0x2f, 0xfc, 0x24, 0x21},
		},
		{
			name:    "gzip, not zstd",
			wantErr: true,
			in:      []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00},
		},
		{
			name: "empty input",
			in:   []byte{},
			err:  errFrameTruncated,
		},
		{
			name: "truncated inside the magic number",
			in:   []byte{0x28, 0xb5, 0x2f},
			err:  errFrameTruncated,
		},
		{
			name: "truncated before the descriptor",
			in:   []byte{0x28, 0xb5, 0x2f, 0xfd},
			err:  errFrameTruncated,
		},
		{
			name: "truncated inside the window descriptor",
			in:   []byte{0x28, 0xb5, 0x2f, 0xfd, 0x84},
			err:  errFrameTruncated,
		},
		{
			name: "truncated inside the dictionary ID",
			in:   []byte{0x28, 0xb5, 0x2f, 0xfd, 0xa7, 0x3b, 0x05},
			err:  errFrameTruncated,
		},
		{
			name: "truncated inside the content size",
			in:   []byte{0x28, 0xb5, 0x2f, 0xfd, 0xa4, 0xb8, 0x82},
			err:  errFrameTruncated,
		},
		{
			// desc 0xcc has bit 3 set, which the spec reserves and requires to
			// be zero. Anything reading it as a frame is misreading something.
			name:    "reserved bit set",
			wantErr: true,
			in:      []byte{0x28, 0xb5, 0x2f, 0xfd, 0xcc, 0x48, 0x00, 0xf2, 0x05, 0x2a, 0x01, 0x00, 0x00, 0x00},
		},
		{
			name:    "8 byte content size overflows int64",
			wantErr: true,
			in: []byte{0x28, 0xb5, 0x2f, 0xfd, 0xc4, 0x48,
				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known, err := zstdFrameContentSize(bytes.NewReader(c.in))

			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("err = %v, want %v", err, c.err)
				}
				return
			}
			if c.wantErr {
				if err == nil {
					t.Fatalf("got (%d, %v, nil), want an error", got, known)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if known != c.known {
				t.Fatalf("known = %v, want %v", known, c.known)
			}
			if got != c.want {
				t.Fatalf("size = %d, want %d", got, c.want)
			}
		})
	}
}

// The parser must stop at the end of the header. The caller opens the archive
// only for this, but a reader that ran on would be a bug waiting to bite if it
// is ever handed a stream.
func TestZstdFrameContentSizeStopsAfterHeader(t *testing.T) {
	header := []byte{0x28, 0xb5, 0x2f, 0xfd, 0xa4, 0xb8, 0x82, 0x01, 0x00}
	body := []byte("compressed blocks would follow here")
	r := bytes.NewReader(append(append([]byte{}, header...), body...))

	if _, _, err := zstdFrameContentSize(r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(rest, body) {
		t.Fatalf("consumed past the header, %d bytes left of %d", len(rest), len(body))
	}
}

func TestParseDfAvailableBytes(t *testing.T) {
	const kb = 1024

	cases := []struct {
		name    string
		out     string
		want    int64
		wantErr bool
	}{
		{
			// Straight off the DBC.
			name: "df -Pk on the DBC",
			out: "Filesystem     1024-blocks   Used Available Capacity Mounted on\n" +
				"/dev/mmcblk3p4     5126105 926497   3973633      19% /data",
			want: 3973633 * kb,
		},
		{
			name: "busybox df -k",
			out: "Filesystem           1K-blocks      Used Available Use% Mounted on\n" +
				"/dev/mmcblk3p4         5126105    926497   3973633  19% /data",
			want: 3973633 * kb,
		},
		{
			// coreutils breaks the line when the device name is long, which
			// puts the numbers on their own line with one column fewer.
			name: "coreutils wrapping a long device name",
			out: "Filesystem                    1K-blocks      Used Available Use% Mounted on\n" +
				"/dev/mapper/a-very-long-device-name-indeed\n" +
				"                                5126105    926497   3973633  19% /data",
			want: 3973633 * kb,
		},
		{
			// RunCommand returns stdout and stderr together.
			name: "ssh noise above the table",
			out: "Warning: Permanently added '192.168.7.2' (ED25519) to the list of known hosts.\n" +
				"Filesystem     1024-blocks   Used Available Capacity Mounted on\n" +
				"/dev/mmcblk3p4     5126105 926497   3973633      19% /data",
			want: 3973633 * kb,
		},
		{
			name: "full filesystem",
			out: "Filesystem     1024-blocks    Used Available Capacity Mounted on\n" +
				"/dev/mmcblk3p4     5126105 5126105         0     100% /data",
			want: 0,
		},
		{
			name: "mount point containing a percent sign",
			out: "Filesystem     1024-blocks   Used Available Capacity Mounted on\n" +
				"/dev/mmcblk3p4     5126105 926497   3973633      19% /data/50%off",
			want: 3973633 * kb,
		},
		{
			name:    "df failed and said so",
			out:     "df: /data: No such file or directory",
			wantErr: true,
		},
		{
			name:    "header only",
			out:     "Filesystem     1024-blocks   Used Available Capacity Mounted on",
			wantErr: true,
		},
		{
			name:    "empty output",
			out:     "",
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDfAvailableBytes(c.out)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got %d, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("available = %d, want %d", got, c.want)
			}
		})
	}
}
