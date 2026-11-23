package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// gzipMagic is the first two bytes of a gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// openArchiveReader opens a tar file for reading, transparently
// decompressing it when it starts with the gzip magic bytes. The caller
// must call Close.
func openArchiveReader(path string) (*reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	peek, err := br.Peek(2)
	if err != nil && err != io.EOF {
		f.Close()
		return nil, err
	}
	var r io.Reader = br
	var gz *gzip.Reader
	if len(peek) == 2 && peek[0] == gzipMagic[0] && peek[1] == gzipMagic[1] {
		gz, err = gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("invalid gzip stream: %w", err)
		}
		r = gz
	}
	return &reader{file: f, gz: gz, r: r}, nil
}

// reader streams entries out of a tar archive.
type reader struct {
	file *os.File
	gz   *gzip.Reader
	r    io.Reader

	// remaining bytes of the current entry's data, plus padding to skip.
	remaining int64
	// entrySize is the full data size of the current entry; it is used to
	// compute the block padding in skipData and cleared once the data
	// (and its padding) has been consumed by any path.
	entrySize int64
	// longName / longLink carried from a preceding GNU 'L'/'K' entry.
	longName string
	longLink string
	// sawEnd is set after the first zero block; a second one ends the stream.
	sawEnd bool
	// entries counts headers returned, for diagnostics.
	entries int
}

// Close releases the archive file (and the gzip reader, if any).
func (r *reader) Close() error {
	if r.gz != nil {
		r.gz.Close()
	}
	return r.file.Close()
}

// skipData discards the rest of the current entry's data plus its padding
// to the next 512-byte boundary.
func (r *reader) skipData() error {
	if r.remaining > 0 {
		if _, err := io.CopyN(io.Discard, r.r, r.remaining); err != nil {
			return fmt.Errorf("truncated archive: entry data ends early: %w", err)
		}
		r.remaining = 0
	}
	if r.entrySize > 0 {
		if pad := (-r.entrySize) & (blockSize - 1); pad > 0 {
			if _, err := io.CopyN(io.Discard, r.r, pad); err != nil {
				return fmt.Errorf("truncated archive: padding ends early: %w", err)
			}
		}
		r.entrySize = 0
	}
	return nil
}

// readData returns exactly size bytes of the current entry's data and then
// skips the padding. It must be called at most once per entry, before the
// next call to Next.
func (r *reader) readData(size int64) ([]byte, error) {
	if size < 0 || size > r.remaining {
		return nil, fmt.Errorf("invalid data size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r.r, buf); err != nil {
		return nil, fmt.Errorf("truncated archive: entry data ends early: %w", err)
	}
	r.remaining -= size
	// Skip padding to the next block boundary.
	if pad := (-size) & (blockSize - 1); pad > 0 {
		if _, err := io.CopyN(io.Discard, r.r, pad); err != nil {
			return nil, fmt.Errorf("truncated archive: padding ends early: %w", err)
		}
	}
	r.entrySize = 0
	return buf, nil
}

// Next returns the next entry header, or io.EOF at the end of the archive.
// GNU longname/longlink and pax extended headers are consumed here: the
// header returned is always the real entry that follows them.
func (r *reader) Next() (header, error) {
	for {
		if err := r.skipData(); err != nil {
			return header{}, err
		}
		block := make([]byte, blockSize)
		if _, err := io.ReadFull(r.r, block); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return header{}, fmt.Errorf("truncated archive: incomplete header block (%d entries read)", r.entries)
			}
			return header{}, err
		}
		h, zero, err := parseHeader(block)
		if err != nil {
			return header{}, err
		}
		if zero {
			if r.sawEnd {
				return header{}, io.EOF
			}
			// First zero block: the archive proper is over, but keep
			// reading so a second zero block (or trailing garbage)
			// is handled the same way. Treat a lone zero block
			// followed by EOF as a clean end.
			r.sawEnd = true
			// Peek: if the stream is already at EOF, finish now.
			if _, err := io.ReadFull(r.r, block); err != nil {
				if err == io.EOF {
					return header{}, io.EOF
				}
				return header{}, fmt.Errorf("truncated archive: partial block after end-of-archive marker: %w", err)
			}
			// There is more data after the zero block. It must be a
			// second zero block; anything else is trailing garbage.
			allZero := true
			for _, b := range block {
				if b != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				return header{}, io.EOF
			}
			return header{}, errors.New("trailing garbage after end-of-archive marker")
		}
		r.remaining = h.Size
		r.entrySize = h.Size
		r.entries++

		switch h.TypeFlag {
		case typeLongName, typeLongLink:
			data, err := r.readData(h.Size)
			if err != nil {
				return header{}, err
			}
			name := strings.TrimRight(string(data), "\x00")
			if h.TypeFlag == typeLongName {
				r.longName = name
			} else {
				r.longLink = name
			}
			continue
		case typePAX:
			// Pax extended headers carry per-entry metadata we do not
			// need (atime, long names, ...). Skip the data and use the
			// plain header that follows; long pax names fall back to
			// the truncated name field.
			if _, err := r.readData(h.Size); err != nil {
				return header{}, err
			}
			continue
		}
		if r.longName != "" {
			h.Name = r.longName
			r.longName = ""
		}
		if r.longLink != "" {
			h.LinkName = r.longLink
			r.longLink = ""
		}
		return h, nil
	}
}

// archiveWriter appends entries to a tar archive, optionally gzip-compressed.
type archiveWriter struct {
	file *os.File
	gz   *gzip.Writer
	w    io.Writer
}

// createArchiveWriter creates (truncating) the output file and returns a
// writer for it. Pass gzipFlag to compress with gzip.
func createArchiveWriter(path string, gzipFlag bool) (*archiveWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	aw := &archiveWriter{file: f, w: f}
	if gzipFlag {
		aw.gz = gzip.NewWriter(f)
		aw.w = aw.gz
	}
	return aw, nil
}

// writeLongEntry writes a GNU longname ('L') or longlink ('K') entry whose
// data is the full name, NUL-terminated and padded to a block boundary.
func (w *archiveWriter) writeLongEntry(flag byte, value string) error {
	data := append([]byte(value), 0)
	h := header{
		Name:     "././@LongLink",
		Mode:     0644,
		Size:     int64(len(data)),
		ModTime:  0,
		TypeFlag: flag,
	}
	block, err := encodeHeader(h)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(block); err != nil {
		return err
	}
	if _, err := w.w.Write(data); err != nil {
		return err
	}
	if pad := (-len(data)) & (blockSize - 1); pad > 0 {
		if _, err := w.w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	return nil
}

// WriteEntry writes one header plus its data (for regular files). Data is
// read from r; for directories and symlinks r must be nil and size 0.
func (w *archiveWriter) WriteEntry(h header, r io.Reader) error {
	if _, _, ok := splitName(h.Name); !ok {
		// Name does not fit the ustar fields: precede it with a GNU
		// longname entry so readers recover the full path.
		if err := w.writeLongEntry(typeLongName, h.Name); err != nil {
			return err
		}
	}
	if _, _, ok := splitName(h.LinkName); h.LinkName != "" && !ok {
		if err := w.writeLongEntry(typeLongLink, h.LinkName); err != nil {
			return err
		}
	}
	block, err := encodeHeader(h)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(block); err != nil {
		return err
	}
	if h.IsRegular() && h.Size > 0 {
		if _, err := io.CopyN(w.w, r, h.Size); err != nil {
			return fmt.Errorf("writing data for %s: %w", h.Name, err)
		}
		if pad := (-h.Size) & (blockSize - 1); pad > 0 {
			if _, err := w.w.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close writes the two zero end-of-archive blocks and closes the file.
func (w *archiveWriter) Close() error {
	if _, err := w.w.Write(make([]byte, blockSize*2)); err != nil {
		w.file.Close()
		return err
	}
	if w.gz != nil {
		if err := w.gz.Close(); err != nil {
			w.file.Close()
			return err
		}
	}
	return w.file.Close()
}
