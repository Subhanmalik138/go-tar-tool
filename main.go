package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

const usage = `tar-tool — a zero-dependency tar archive CLI (Go, stdlib only).

Usage:
  tar-tool create  -f ARCHIVE [-z] [--exclude PATTERN]... PATH...
  tar-tool list    -f ARCHIVE [-v]
  tar-tool extract -f ARCHIVE [-C DIR] [--strip-components N] [--force]
  tar-tool verify  -f ARCHIVE

Commands:
  create   Build a ustar archive from files and directories.
  list     Print the entries in an archive (-v for a long listing).
  extract  Extract an archive, refusing entries that would escape -C.
  verify   Re-read an archive, checking every header checksum and that
           every entry's data is fully present.

Flags:
  -f ARCHIVE   archive file (".tar.gz" is detected by content, -z forces gzip)
  -z           gzip-compress on create
  -C DIR       extract into DIR (default: current directory)
  -v           verbose listing
  --force      overwrite existing files on extract
  --exclude PATTERN
               skip paths matching the glob when creating (repeatable)
  --strip-components N
               drop the first N path components on extract
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "create", "list", "extract", "verify":
		if err := run(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "tar-tool: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "tar-tool: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	archive := fs.String("f", "", "archive file")
	gzipFlag := fs.Bool("z", false, "gzip-compress on create")
	dest := fs.String("C", ".", "extract destination directory")
	verbose := fs.Bool("v", false, "verbose listing")
	force := fs.Bool("force", false, "overwrite existing files on extract")
	stripN := fs.Int("strip-components", 0, "drop leading path components on extract")
	var excludes stringList
	fs.Var(&excludes, "exclude", "skip paths matching this glob when creating")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch cmd {
	case "create":
		if *archive == "" {
			return fmt.Errorf("create requires -f ARCHIVE")
		}
		if fs.NArg() == 0 {
			return fmt.Errorf("create requires at least one PATH")
		}
		if strings.HasSuffix(*archive, ".tar.gz") || strings.HasSuffix(*archive, ".tgz") {
			*gzipFlag = true
		}
		return createArchive(*archive, *gzipFlag, fs.Args(), excludeMatcher{patterns: excludes})

	case "list":
		if *archive == "" {
			return fmt.Errorf("list requires -f ARCHIVE")
		}
		return listArchive(*archive, *verbose)

	case "extract":
		if *archive == "" {
			return fmt.Errorf("extract requires -f ARCHIVE")
		}
		if *stripN < 0 {
			return fmt.Errorf("--strip-components must be >= 0")
		}
		return extractArchive(*archive, *dest, *stripN > 0, *force, *stripN)

	case "verify":
		if *archive == "" {
			return fmt.Errorf("verify requires -f ARCHIVE")
		}
		return verifyArchive(*archive)
	}
	return nil
}
