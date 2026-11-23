package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildFixture creates a small directory tree and returns its path.
func buildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "hello.txt"), "hello world\n")
	mustWrite(t, filepath.Join(root, "sub", "data.bin"), bytes.Repeat([]byte{0xAB, 0x01}, 700))
	mustWrite(t, filepath.Join(root, "empty.txt"), "")
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustWrite(t *testing.T, path string, data any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b []byte
	switch v := data.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// archiveBytes runs createArchive into a temp file and returns its bytes.
func archiveBytes(t *testing.T, gzipFlag bool, paths []string, excludes ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.tar")
	if err := createArchive(out, gzipFlag, paths, excludeMatcher{patterns: excludes}); err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHeaderRoundTrip(t *testing.T) {
	h := header{
		Name: "some/dir/file.txt", Mode: 0o644, UID: 1000, GID: 1000,
		Size: 42, ModTime: 1700000000, TypeFlag: typeReg,
		UName: "alice", GName: "staff",
	}
	block, err := encodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	got, zero, err := parseHeader(block)
	if err != nil || zero {
		t.Fatalf("parseHeader: zero=%v err=%v", zero, err)
	}
	if got != h {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

func TestHeaderChecksumMismatch(t *testing.T) {
	h := header{Name: "a.txt", Mode: 0o644, Size: 1, TypeFlag: typeReg}
	block, _ := encodeHeader(h)
	block[10] ^= 0xFF // corrupt the name after the checksum was computed
	if _, _, err := parseHeader(block); err == nil {
		t.Fatal("expected checksum error, got nil")
	}
}

func TestHeaderLongNamePrefixSplit(t *testing.T) {
	// 150-char path that splits cleanly at a '/' boundary.
	name := strings.Repeat("dir/", 30) + "file.txt" // 124 chars
	if len(name) <= 100 {
		t.Fatalf("fixture too short: %d", len(name))
	}
	h := header{Name: name, Mode: 0o644, TypeFlag: typeReg}
	block, err := encodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := parseHeader(block)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name {
		t.Fatalf("prefix split lost the name: got %q want %q", got.Name, name)
	}
}

func TestCreateListRoundTrip(t *testing.T) {
	root := buildFixture(t)
	out := filepath.Join(t.TempDir(), "out.tar")
	if err := createArchive(out, false, []string{root}, excludeMatcher{}); err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	r, err := openArchiveReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		names = append(names, h.Name)
		if err := r.skipData(); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Base(root)
	want := map[string]bool{
		base + "/": true, base + "/hello.txt": true,
		base + "/sub/": true, base + "/sub/data.bin": true,
		base + "/empty.txt": true, base + "/emptydir/": true,
		base + "/link.txt": true,
	}
	if len(names) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(names), names, len(want))
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("unexpected entry %q", n)
		}
	}
}

func TestExtractRoundTripPreservesContentModeAndMtime(t *testing.T) {
	root := buildFixture(t)
	// Pin a known mtime and mode on one file.
	hello := filepath.Join(root, "hello.txt")
	mtime := time.Unix(1700000000, 0)
	if err := os.Chtimes(hello, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hello, 0o640); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.tar")
	if err := createArchive(out, false, []string{root}, excludeMatcher{}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractArchive(out, dest, false, false, 0); err != nil {
		t.Fatalf("extractArchive: %v", err)
	}
	base := filepath.Base(root)
	got, err := os.ReadFile(filepath.Join(dest, base, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world\n" {
		t.Fatalf("content mismatch: %q", got)
	}
	info, err := os.Stat(filepath.Join(dest, base, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 0640", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("mtime = %v, want %v", info.ModTime(), mtime)
	}
	// Empty file and empty dir survive the round trip.
	if _, err := os.Stat(filepath.Join(dest, base, "empty.txt")); err != nil {
		t.Fatalf("empty.txt missing: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dest, base, "emptydir")); err != nil || !st.IsDir() {
		t.Fatalf("emptydir missing or not a dir: %v", err)
	}
	// Symlink is restored as a symlink with the same target.
	link, err := os.Readlink(filepath.Join(dest, base, "link.txt"))
	if err != nil {
		t.Fatalf("link.txt is not a symlink: %v", err)
	}
	if link != "hello.txt" {
		t.Fatalf("symlink target = %q, want %q", link, "hello.txt")
	}
}

func TestGzipRoundTrip(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, true, []string{root})
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatal("gzip output does not start with the gzip magic")
	}
	out := filepath.Join(t.TempDir(), "gz.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractArchive(out, dest, false, false, 0); err != nil {
		t.Fatalf("extract gzip archive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.Base(root), "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world\n" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestLongNameRoundTrip(t *testing.T) {
	root := t.TempDir()
	// A 130-char file name: too long for the 100-byte name field and with
	// no '/' split that fits the prefix field, so a GNU longname entry
	// is required.
	long := strings.Repeat("a", 130) + ".txt"
	mustWrite(t, filepath.Join(root, long), "long name content\n")
	b := archiveBytes(t, false, []string{root})
	out := filepath.Join(t.TempDir(), "long.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractArchive(out, dest, false, false, 0); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.Base(root), long))
	if err != nil {
		t.Fatalf("long-named file missing after extract: %v", err)
	}
	if string(got) != "long name content\n" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	// Hand-build an archive with a malicious entry and check extraction
	// refuses it before writing anything outside the destination.
	tmp := t.TempDir()
	arc := filepath.Join(tmp, "evil.tar")
	w, err := createArchiveWriter(arc, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.txt", "/abs.txt", "ok/../../escape2.txt"} {
		h := header{Name: name, Mode: 0o644, Size: 4, TypeFlag: typeReg}
		if err := w.WriteEntry(h, strings.NewReader("evil")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "dest")
	err = extractArchive(arc, dest, false, false, 0)
	if err == nil {
		t.Fatal("expected extraction to refuse traversal entries")
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "escape.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("traversal entry escaped the destination")
	}
}

func TestUnsafeSymlinkRejected(t *testing.T) {
	tmp := t.TempDir()
	arc := filepath.Join(tmp, "link.tar")
	w, err := createArchiveWriter(arc, false)
	if err != nil {
		t.Fatal(err)
	}
	h := header{Name: "badlink", Mode: 0o777, TypeFlag: typeSymlink, LinkName: "../../etc/passwd"}
	if err := w.WriteEntry(h, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(arc, filepath.Join(tmp, "dest"), false, false, 0); err == nil {
		t.Fatal("expected extraction to refuse an escaping symlink")
	}
}

func TestTruncatedArchiveDetected(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root})
	// Cut the archive in half: the reader must notice the missing data.
	half := b[:len(b)/2]
	out := filepath.Join(t.TempDir(), "cut.tar")
	if err := os.WriteFile(out, half, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(out); err == nil {
		t.Fatal("verify accepted a truncated archive")
	}
}

func TestCorruptChecksumDetected(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root})
	b[10] ^= 0xFF // corrupt the first header's name field
	out := filepath.Join(t.TempDir(), "bad.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(out); err == nil {
		t.Fatal("verify accepted an archive with a bad checksum")
	}
}

func TestVerifyCleanArchive(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root})
	out := filepath.Join(t.TempDir(), "ok.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(out); err != nil {
		t.Fatalf("verify rejected a clean archive: %v", err)
	}
}

func TestExcludePattern(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root}, "sub")
	out := filepath.Join(t.TempDir(), "ex.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := openArchiveReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "sub/") {
			t.Fatalf("excluded path still present: %q", h.Name)
		}
		if err := r.skipData(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStripComponents(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root})
	out := filepath.Join(t.TempDir(), "strip.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractArchive(out, dest, true, false, 1); err != nil {
		t.Fatalf("extract with --strip-components: %v", err)
	}
	// The top-level directory name is stripped, so hello.txt lands
	// directly in dest.
	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("stripped file missing: %v", err)
	}
	if string(got) != "hello world\n" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestExtractRefusesOverwriteWithoutForce(t *testing.T) {
	root := buildFixture(t)
	b := archiveBytes(t, false, []string{root})
	out := filepath.Join(t.TempDir(), "ow.tar")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := extractArchive(out, dest, false, false, 0); err != nil {
		t.Fatal(err)
	}
	// Second extraction into the same directory must refuse to clobber.
	if err := extractArchive(out, dest, false, false, 0); err == nil {
		t.Fatal("expected refusal to overwrite without --force")
	}
	if err := extractArchive(out, dest, false, true, 0); err != nil {
		t.Fatalf("extraction with --force failed: %v", err)
	}
}

func TestSafeJoin(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a/b.txt", true},
		{"./a.txt", true},
		{"../evil", false},
		{"a/../../evil", false},
		{"/etc/passwd", false},
		{"..", false},
	}
	for _, c := range cases {
		_, err := safeJoin("/tmp/dest", c.name)
		if c.ok && err != nil {
			t.Errorf("safeJoin(%q) wrongly rejected: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("safeJoin(%q) wrongly accepted", c.name)
		}
	}
}
