package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// safeJoin resolves an archive member name against the destination
// directory and rejects anything that would escape it: absolute paths and
// ".." components. This is the classic tar path-traversal ("zip slip")
// defence, applied before anything is written.
func safeJoin(dest, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("entry %q has an absolute path", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entry %q escapes the destination directory", name)
	}
	return filepath.Join(dest, clean), nil
}

// stripComponents drops the first n '/'-separated components from a name,
// the way GNU tar's --strip-components works. It reports false when the
// name has fewer components than n.
func stripComponents(name string, n int) (string, bool) {
	if n == 0 {
		return name, true
	}
	parts := strings.Split(name, "/")
	if len(parts) <= n {
		return "", false
	}
	return strings.Join(parts[n:], "/"), true
}

// extractArchive extracts every entry into dest, honouring strip and
// force. Modes and modification times are restored; symlinks are created
// only when their target is relative and stays inside the destination.
func extractArchive(path, dest string, strip, force bool, stripN int) error {
	r, err := openArchiveReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := h.Name
		if strip {
			var ok bool
			name, ok = stripComponents(name, stripN)
			if !ok {
				if err := r.skipData(); err != nil {
					return err
				}
				continue
			}
		}
		target, err := safeJoin(absDest, name)
		if err != nil {
			return fmt.Errorf("refusing unsafe entry: %w", err)
		}
		if err := extractOne(r, h, target, force); err != nil {
			return err
		}
	}
}

// extractOne writes a single entry to target. The entry's data is read
// from r (and skipped for non-regular entries).
func extractOne(r *reader, h header, target string, force bool) error {
	switch {
	case h.IsDir():
		if err := r.skipData(); err != nil {
			return err
		}
		if err := os.MkdirAll(target, fs.FileMode(h.Mode).Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, time.Now(), time.Unix(h.ModTime, 0))

	case h.IsSymlink():
		if err := r.skipData(); err != nil {
			return err
		}
		// A symlink whose target is absolute or climbs out of the tree
		// can be used to write outside the destination on a later
		// entry, so refuse it outright.
		if filepath.IsAbs(h.LinkName) || strings.HasPrefix(filepath.Clean(h.LinkName), "..") {
			return fmt.Errorf("refusing unsafe symlink %q -> %q", h.Name, h.LinkName)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil {
			if !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", target)
			}
			if err := os.Remove(target); err != nil {
				return err
			}
		}
		return os.Symlink(h.LinkName, target)

	case h.IsRegular():
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil && !force {
			if err := r.skipData(); err != nil {
				return err
			}
			return fmt.Errorf("%s already exists (use --force to overwrite)", target)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode).Perm())
		if err != nil {
			return err
		}
		// Copy exactly Size bytes, then skip the block padding.
		remaining := h.Size
		buf := make([]byte, 32*1024)
		for remaining > 0 {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			if _, err := io.ReadFull(r.r, buf[:n]); err != nil {
				f.Close()
				return fmt.Errorf("truncated archive: data for %s ends early: %w", h.Name, err)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			remaining -= n
		}
		if pad := (-h.Size) & (blockSize - 1); pad > 0 {
			if _, err := io.CopyN(io.Discard, r.r, pad); err != nil {
				f.Close()
				return fmt.Errorf("truncated archive: padding for %s ends early: %w", h.Name, err)
			}
		}
		r.remaining = 0
		r.entrySize = 0
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Chmod(target, fs.FileMode(h.Mode).Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, time.Now(), time.Unix(h.ModTime, 0))

	default:
		// Unknown type flags (hard links, devices, ...) are skipped
		// rather than mis-extracted.
		if err := r.skipData(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: skipping unsupported entry %s (type %q)\n", h.Name, h.TypeFlag)
		return nil
	}
}

// formatMode renders a unix mode like "-rw-r--r--" / "drwxr-xr-x".
func formatMode(mode int64, flag byte) string {
	perm := fs.FileMode(mode).Perm()
	var b strings.Builder
	switch flag {
	case typeDir:
		b.WriteByte('d')
	case typeSymlink:
		b.WriteByte('l')
	default:
		b.WriteByte('-')
	}
	for _, bit := range []struct {
		mask fs.FileMode
		ch   byte
	}{
		{0o400, 'r'}, {0o200, 'w'}, {0o100, 'x'},
		{0o040, 'r'}, {0o020, 'w'}, {0o010, 'x'},
		{0o004, 'r'}, {0o002, 'w'}, {0o001, 'x'},
	} {
		if perm&bit.mask != 0 {
			b.WriteByte(bit.ch)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// formatTime renders a unix timestamp the way ls -l does.
func formatTime(unix int64) string {
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}
