// Command storezip packages a built plugin library in the layout the
// CLIProxyAPI plugin store installs from: one zip per platform named
// <id>_<version>_<goos>_<goarch>.zip, holding only <id>.<ext> at the zip root,
// plus a checksums.txt in sha256sum format.
//
// The output is byte-for-byte reproducible: fixed timestamp, fixed mode,
// fixed compression. Run it once per platform with the same -out directory;
// checksums.txt is rewritten to list every zip in that directory.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// zipTime is the fixed modification time of every entry (reproducibility).
var zipTime = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// pluginIDPattern mirrors CLIProxyAPI's plugin id rule.
var pluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// versionPattern is the dotted numeric version the store derives from a v<version> tag.
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

func libraryExt(goos string) (string, error) {
	switch goos {
	case "linux":
		return ".so", nil
	case "darwin":
		return ".dylib", nil
	case "windows":
		return ".dll", nil
	default:
		return "", fmt.Errorf("unsupported goos %q", goos)
	}
}

// buildZip returns the zip bytes holding lib as <id><ext> at the root.
func buildZip(id, ext string, lib []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: id + ext, Method: zip.Deflate, Modified: zipTime}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(lib); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeChecksums lists every *.zip in dir in sha256sum format, sorted by name.
func writeChecksums(dir string) error {
	names, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		return err
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(name) // #nosec G304 -- operator-supplied output directory
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(name))
	}
	return os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(b.String()), 0o644) // #nosec G306 -- public release file
}

func run(args []string) error {
	fs := flag.NewFlagSet("storezip", flag.ContinueOnError)
	so := fs.String("so", "", "path to the built plugin library")
	id := fs.String("id", "", "plugin id")
	version := fs.String("version", "", "release version without the leading v")
	goos := fs.String("goos", "", "target GOOS")
	goarch := fs.String("goarch", "", "target GOARCH")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *so == "" || *out == "" || *goarch == "" {
		return errors.New("-so, -out, -goos, -goarch, -id and -version are required")
	}
	if !pluginIDPattern.MatchString(*id) {
		return fmt.Errorf("invalid plugin id %q", *id)
	}
	if !versionPattern.MatchString(*version) {
		return fmt.Errorf("invalid version %q (no leading v, dotted numbers only)", *version)
	}
	ext, err := libraryExt(*goos)
	if err != nil {
		return err
	}
	lib, err := os.ReadFile(*so) // #nosec G304 -- operator-supplied build artifact
	if err != nil {
		return err
	}
	if len(lib) == 0 {
		return fmt.Errorf("%s is empty", *so)
	}
	data, err := buildZip(*id, ext, lib)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil { // #nosec G301 -- public release directory
		return err
	}
	name := fmt.Sprintf("%s_%s_%s_%s.zip", *id, *version, *goos, *goarch)
	if err := os.WriteFile(filepath.Join(*out, name), data, 0o644); err != nil { // #nosec G306 G703 -- operator-chosen output dir, validated file name; public release file
		return err
	}
	return writeChecksums(*out)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "storezip:", err)
		os.Exit(1)
	}
}
