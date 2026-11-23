# go-tar-tool

A zero-dependency tar archive CLI written in Go, using only the standard
library. It implements the ustar format from scratch — header encoding and
checksum validation, GNU long-name entries, gzip detection — instead of
wrapping `archive/tar`. The point is the format work, not the feature list.

## What it does

- `create` — build a `.tar` (or `.tar.gz` with `-z`) from files and
  directories, preserving modes, modification times and symlinks.
- `list` — print an archive's entries, with `-v` for a long listing.
- `extract` — unpack an archive, refusing entries that would escape the
  destination directory (absolute paths, `..` components, escaping
  symlinks) and refusing to clobber existing files unless `--force` is set.
- `verify` — re-read an archive, checking every header checksum and that
  every entry's data is fully present.

## Usage

```sh
# Build the tool
go build -o tar-tool .

# Create an archive (gzip when the name ends in .tar.gz/.tgz, or with -z)
./tar-tool create -f backup.tar.gz -z docs/ photos/

# Skip a subtree while creating
./tar-tool create -f src.tar --exclude node_modules --exclude '*.log' src/

# List contents
./tar-tool list -f backup.tar.gz
./tar-tool list -v -f backup.tar.gz

# Extract into a directory
./tar-tool extract -f backup.tar.gz -C restored/

# Drop the top-level directory on extract
./tar-tool extract -f backup.tar.gz -C restored/ --strip-components 1

# Check an archive's integrity (exit code non-zero on any problem)
./tar-tool verify -f backup.tar.gz
```

## Design decisions

- **Hand-rolled ustar, stdlib only.** Header parsing, octal fields, the
  checksum (computed with the checksum field itself as spaces), and the
  512-byte block padding are implemented directly, so the tool has no
  dependencies and the format is visible in the code.
- **GNU long names, not pax.** Paths longer than the 100-byte name field
  are split into the ustar prefix field when possible, and written with a
  preceding `L` (long name) entry otherwise. Pax extended headers are
  skipped on read — the plain header that follows is used, so pax
  archives still list and extract, just without their extended metadata.
- **Gzip is detected by content, not by filename.** The reader sniffs the
  `0x1f 0x8b` magic bytes, so a misnamed `.tar.gz` still opens, and a
  plain `.tar` that happens to end in `.gz` is not misread.
- **Extraction is safe by default.** Every member path is resolved
  against the destination and rejected if it escapes it; symlinks with
  absolute or escaping targets are refused; existing files are not
  overwritten without `--force`. This is the classic tar path-traversal
  ("zip slip") defence, applied before anything is written.
- **Verification actually reads the data.** `verify` does not just walk
  headers — it consumes every entry's bytes, so a truncated archive is
  caught instead of silently passing.

## Limitations

- Pax extended headers are skipped (see above); per-entry metadata such
  as atime is not preserved.
- Base-256 numeric fields (GNU's encoding for files over 8 GiB) are
  rejected rather than misread.
- Hard links, devices and other special files are skipped on create with
  a warning, and skipped on extract.

## Tests

```sh
go test ./...
```

14 tests cover the header round-trip and checksum, prefix-split and GNU
long names, create/list/extract round-trips (content, mode, mtime,
symlinks, empty files and directories), gzip, path-traversal and unsafe
symlinks, truncation and checksum corruption, `--exclude`,
`--strip-components`, and overwrite protection.

## Tech stack

Go 1.23, standard library only. No third-party dependencies.
