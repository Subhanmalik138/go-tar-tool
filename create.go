package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// excludeMatcher reports whether a path (slash-separated, relative to the
// archive root) matches any of the --exclude glob patterns.
type excludeMatcher struct {
	patterns []string
}

func (m excludeMatcher) excluded(rel string) bool {
	for _, p := range m.patterns {
		// Match the whole path, any parent directory (so excluding
		// "node_modules" prunes the whole subtree), and any single
		// path component (so "sub" matches "root/sub" at any depth).
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		for _, part := range strings.Split(rel, "/") {
			if ok, _ := filepath.Match(p, part); ok {
				return true
			}
		}
		for dir := filepath.Dir(rel); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			if ok, _ := filepath.Match(p, dir); ok {
				return true
			}
		}
	}
	return false
}

// addPath appends one filesystem path (file, directory or symlink) to the
// archive, recursing into directories. arcName is the name stored in the
// archive; it always uses forward slashes.
func addPath(w *archiveWriter, fsPath, arcName string, excludes excludeMatcher) error {
	info, err := os.Lstat(fsPath)
	if err != nil {
		return err
	}
	// Directories are stored with a trailing slash.
	if info.IsDir() {
		h := headerFromInfo(arcName+"/", info, typeDir, "")
		if err := w.WriteEntry(h, nil); err != nil {
			return err
		}
		entries, err := os.ReadDir(fsPath)
		if err != nil {
			return err
		}
		for _, e := range entries {
			childArc := arcName + "/" + e.Name()
			if excludes.excluded(childArc) {
				continue
			}
			if err := addPath(w, filepath.Join(fsPath, e.Name()), childArc, excludes); err != nil {
				return err
			}
		}
		return nil
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(fsPath)
		if err != nil {
			return err
		}
		h := headerFromInfo(arcName, info, typeSymlink, target)
		return w.WriteEntry(h, nil)
	}

	if !info.Mode().IsRegular() {
		// Sockets, devices, fifos and other specials have no portable
		// representation in ustar; skip them loudly rather than silently.
		fmt.Fprintf(os.Stderr, "warning: skipping non-regular file %s\n", fsPath)
		return nil
	}

	f, err := os.Open(fsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := headerFromInfo(arcName, info, typeReg, "")
	if err := w.WriteEntry(h, f); err != nil {
		return err
	}
	return nil
}

// headerFromInfo builds a ustar header from filesystem metadata. uid/gid
// come from the process (portable enough for an archive tool); uname/gname
// are left empty because resolving them needs cgo on some platforms.
func headerFromInfo(name string, info fs.FileInfo, flag byte, link string) header {
	mode := info.Mode().Perm()
	// Only regular files carry data; a directory's FileInfo.Size is the
	// size of the directory entry on disk, not a payload, and a symlink's
	// is the length of its target — both must be stored as 0 or readers
	// will consume following headers as data.
	size := int64(0)
	if flag == typeReg {
		size = info.Size()
	}
	return header{
		Name:     name,
		Mode:     int64(mode),
		UID:      int64(os.Getuid()),
		GID:      int64(os.Getgid()),
		Size:     size,
		ModTime:  info.ModTime().Unix(),
		TypeFlag: flag,
		LinkName: link,
	}
}

// createArchive builds an archive from the given paths. Each path is
// stored under its base name (a path's own name, not its full path), the
// way tar behaves when given relative paths.
func createArchive(archivePath string, gzipFlag bool, paths []string, excludes excludeMatcher) error {
	w, err := createArchiveWriter(archivePath, gzipFlag)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := w.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	for _, p := range paths {
		// Store the path under its base name, with any trailing slash
		// stripped, so "docs/" and "docs" archive identically.
		clean := strings.TrimSuffix(filepath.Clean(p), string(filepath.Separator))
		arcName := filepath.Base(clean)
		if arcName == "." || arcName == string(filepath.Separator) {
			return fmt.Errorf("cannot archive %q: use a named path, not %q", p, p)
		}
		arcName = filepath.ToSlash(arcName)
		if excludes.excluded(arcName) {
			continue
		}
		if err := addPath(w, p, arcName, excludes); err != nil {
			return fmt.Errorf("adding %s: %w", p, err)
		}
	}
	return nil
}

// listArchive prints every entry, one per line. Verbose mode adds the
// long-listing columns (mode, size, mtime, name).
func listArchive(path string, verbose bool) error {
	r, err := openArchiveReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if verbose {
			fmt.Printf("%s %8d %s %s\n", formatMode(h.Mode, h.TypeFlag), h.Size, formatTime(h.ModTime), h.Name)
		} else {
			fmt.Println(h.Name)
		}
		if err := r.skipData(); err != nil {
			return err
		}
	}
}

// verifyArchive re-reads an archive and checks every header checksum and
// that every entry's data is fully present. It reports per-entry status
// and returns an error (with a count) when anything is wrong.
func verifyArchive(path string) error {
	r, err := openArchiveReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	checked, problems := 0, 0
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			problems++
			fmt.Printf("FAIL: %v\n", err)
			break
		}
		checked++
		// Force the data to be read so truncation is caught even for
		// entries nobody extracts.
		if _, err := r.readData(h.Size); err != nil {
			problems++
			fmt.Printf("FAIL: %s: %v\n", h.Name, err)
			break
		}
		fmt.Printf("OK   %s\n", h.Name)
	}
	fmt.Printf("%d entries checked, %d problems\n", checked, problems)
	if problems > 0 {
		return fmt.Errorf("archive verification failed: %d problem(s)", problems)
	}
	return nil
}
