// Package main implements tar-tool, a zero-dependency tar archive CLI.
//
// The ustar header layout implemented here (one 512-byte block):
//
//	bytes 0-99    file name
//	bytes 100-107 file mode (octal)
//	bytes 108-115 owner uid (octal)
//	bytes 116-123 owner gid (octal)
//	bytes 124-135 file size in bytes (octal)
//	bytes 136-147 modification time (octal)
//	bytes 148-155 header checksum (octal, checksum field itself as spaces)
//	byte  156     type flag ('0' file, '5' dir, '2' symlink, 'L' GNU longname,
//	              'K' GNU longlink, 'x' pax extended header)
//	bytes 157-256 name of linked file (symlinks)
//	bytes 257-262 magic "ustar\x00", bytes 263-264 version "00"
//	bytes 265-296 owner user name, bytes 297-328 owner group name
//	bytes 329-336 device major, bytes 337-344 device minor
//	bytes 345-499 name prefix (joined to the name with '/')
package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// blockSize is the tar block size; every header and every file's data is
// padded up to a multiple of it.
const blockSize = 512

// type flags we understand.
const (
	typeReg      = '0' // regular file ('\0' is treated the same)
	typeLink     = '1' // hard link (read as a regular file with a link name)
	typeSymlink  = '2' // symbolic link
	typeDir      = '5' // directory
	typeLongName = 'L' // GNU long name, data block holds the real name
	typeLongLink = 'K' // GNU long link name, data block holds the real target
	typePAX      = 'x' // pax extended header, data block is skipped
)

// header is a parsed ustar header block.
type header struct {
	Name     string
	Mode     int64
	UID      int64
	GID      int64
	Size     int64
	ModTime  int64
	TypeFlag byte
	LinkName string
	UName    string
	GName    string
	DevMajor int64
	DevMinor int64
}

// IsDir reports whether the entry is a directory.
func (h header) IsDir() bool { return h.TypeFlag == typeDir }

// IsSymlink reports whether the entry is a symbolic link.
func (h header) IsSymlink() bool { return h.TypeFlag == typeSymlink }

// IsRegular reports whether the entry holds file data.
func (h header) IsRegular() bool {
	return h.TypeFlag == typeReg || h.TypeFlag == 0 || h.TypeFlag == typeLink
}

func parseOctal(b []byte) (int64, error) {
	s := strings.Trim(string(b), "\x00 ")
	if s == "" {
		return 0, nil
	}
	// Base-256 encoding (top bit set) is a GNU extension for huge values;
	// out of scope for this tool — reject it rather than misread it.
	if b[0]&0x80 != 0 {
		return 0, errors.New("base-256 numeric field not supported")
	}
	v, err := strconv.ParseInt(s, 8, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid octal field %q: %w", s, err)
	}
	return v, nil
}

// fieldString reads a null-terminated string field.
func fieldString(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// checksum sums a header block with the checksum field treated as spaces,
// which is how the stored checksum is computed.
func checksum(block []byte) int64 {
	var sum int64
	for i, b := range block {
		if i >= 148 && i < 156 {
			sum += ' '
		} else {
			sum += int64(b)
		}
	}
	return sum
}

// parseHeader decodes one 512-byte block. A block of all zeroes reports
// (zero, true, nil): the end-of-archive marker.
func parseHeader(block []byte) (header, bool, error) {
	var h header
	zero := true
	for _, b := range block {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return h, true, nil
	}

	stored, err := parseOctal(block[148:156])
	if err != nil {
		return h, false, fmt.Errorf("bad checksum field: %w", err)
	}
	if stored != checksum(block) {
		return h, false, fmt.Errorf("header checksum mismatch: stored %d, computed %d", stored, checksum(block))
	}

	// The magic is checked leniently: pre-ustar (v7) archives have no magic
	// at all, and a valid checksum is the real integrity signal.
	magic := string(block[257:263])
	if magic != "ustar\x00" && magic != "ustar " && magic != "\x00\x00\x00\x00\x00\x00" {
		return h, false, fmt.Errorf("not a tar archive: bad magic %q", magic)
	}

	h.Name = fieldString(block[0:100])
	if h.Mode, err = parseOctal(block[100:108]); err != nil {
		return h, false, fmt.Errorf("bad mode: %w", err)
	}
	if h.UID, err = parseOctal(block[108:116]); err != nil {
		return h, false, fmt.Errorf("bad uid: %w", err)
	}
	if h.GID, err = parseOctal(block[116:124]); err != nil {
		return h, false, fmt.Errorf("bad gid: %w", err)
	}
	if h.Size, err = parseOctal(block[124:136]); err != nil {
		return h, false, fmt.Errorf("bad size: %w", err)
	}
	if h.Size < 0 {
		return h, false, errors.New("negative file size in header")
	}
	if h.ModTime, err = parseOctal(block[136:148]); err != nil {
		return h, false, fmt.Errorf("bad mtime: %w", err)
	}
	h.TypeFlag = block[156]
	h.LinkName = fieldString(block[157:257])
	h.UName = fieldString(block[265:297])
	h.GName = fieldString(block[297:329])
	if h.DevMajor, err = parseOctal(block[329:337]); err != nil {
		return h, false, fmt.Errorf("bad devmajor: %w", err)
	}
	if h.DevMinor, err = parseOctal(block[337:345]); err != nil {
		return h, false, fmt.Errorf("bad devminor: %w", err)
	}
	if prefix := fieldString(block[345:500]); prefix != "" {
		h.Name = prefix + "/" + h.Name
	}
	return h, false, nil
}

// splitName splits a long path into a ustar prefix (<=155 bytes) and name
// (<=100 bytes) at a '/' boundary. It reports false when no split fits.
func splitName(name string) (prefix, base string, ok bool) {
	if len(name) <= 100 {
		return "", name, true
	}
	// Try every '/' from the right, keeping both halves within their limits.
	for i := len(name) - 1; i > 0; i-- {
		if name[i] != '/' {
			continue
		}
		prefix, base = name[:i], name[i+1:]
		if len(prefix) <= 155 && len(base) <= 100 && len(base) > 0 {
			return prefix, base, true
		}
	}
	return "", "", false
}

func putString(block []byte, off, size int, s string) {
	copy(block[off:off+size], s)
}

func putOctal(block []byte, off, size int, v int64) {
	// "%0*o\x00": octal digits, NUL-terminated, field width size-1.
	s := fmt.Sprintf("%0*o\x00", size-1, v)
	copy(block[off:off+size], s)
}

// encodeHeader serializes a header into one 512-byte block. Names that do
// not fit the name/prefix fields are truncated here; the caller writes a
// GNU longname ('L') entry first for those.
func encodeHeader(h header) ([]byte, error) {
	block := make([]byte, blockSize)
	prefix, name, ok := splitName(h.Name)
	if !ok {
		// Fall back to the first 100 bytes; the longname entry written
		// before this header carries the full name.
		name = h.Name
		if len(name) > 100 {
			name = name[:100]
		}
	}
	putString(block, 0, 100, name)
	putOctal(block, 100, 8, h.Mode)
	putOctal(block, 108, 8, h.UID)
	putOctal(block, 116, 8, h.GID)
	putOctal(block, 124, 12, h.Size)
	putOctal(block, 136, 12, h.ModTime)
	putString(block, 156, 1, string(h.TypeFlag))
	putString(block, 157, 100, h.LinkName)
	putString(block, 257, 6, "ustar\x00")
	putString(block, 263, 2, "00")
	putString(block, 265, 32, h.UName)
	putString(block, 297, 32, h.GName)
	putOctal(block, 329, 8, h.DevMajor)
	putOctal(block, 337, 8, h.DevMinor)
	putString(block, 345, 155, prefix)
	// Checksum field: six octal digits, NUL, space.
	sum := checksum(block)
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return block, nil
}
