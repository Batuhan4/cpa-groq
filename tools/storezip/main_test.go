package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLib(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "lib.so")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunProducesStoreLayout(t *testing.T) {
	dir := t.TempDir()
	lib := bytes.Repeat([]byte("ELF-test-library"), 1000)
	so := writeLib(t, dir, lib)
	out := filepath.Join(dir, "store")
	args := []string{"-so", so, "-id", "cpa-groq", "-version", "0.1.0", "-goos", "linux", "-goarch", "amd64", "-out", out}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(out, "cpa-groq_0.1.0_linux_amd64.zip")
	data, err := os.ReadFile(zipPath) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("zip has %d entries, want exactly 1", len(zr.File))
	}
	f := zr.File[0]
	if f.Name != "cpa-groq.so" {
		t.Fatalf("entry name %q, want cpa-groq.so at the zip root", f.Name)
	}
	if f.Mode().Perm() != 0o755 || !f.Mode().IsRegular() {
		t.Fatalf("entry mode %v, want regular 0755", f.Mode())
	}
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, lib) {
		t.Fatal("zipped library differs from the input")
	}

	sums, err := os.ReadFile(filepath.Join(out, "checksums.txt")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:]) + "  cpa-groq_0.1.0_linux_amd64.zip\n"
	if string(sums) != want {
		t.Fatalf("checksums.txt = %q, want %q", sums, want)
	}
}

func TestRunIsReproducible(t *testing.T) {
	dir := t.TempDir()
	so := writeLib(t, dir, []byte("same library bytes"))
	var zips [][]byte
	for _, sub := range []string{"a", "b"} {
		out := filepath.Join(dir, sub)
		if err := run([]string{"-so", so, "-id", "cpa-groq", "-version", "1.2.3", "-goos", "linux", "-goarch", "arm64", "-out", out}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, "cpa-groq_1.2.3_linux_arm64.zip")) // #nosec G304 -- test temp dir
		if err != nil {
			t.Fatal(err)
		}
		zips = append(zips, data)
	}
	if !bytes.Equal(zips[0], zips[1]) {
		t.Fatal("two runs over the same input produced different zips")
	}
}

func TestChecksumsListEveryPlatform(t *testing.T) {
	dir := t.TempDir()
	so := writeLib(t, dir, []byte("lib"))
	out := filepath.Join(dir, "store")
	for _, p := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}} {
		if err := run([]string{"-so", so, "-id", "x", "-version", "2.0.0", "-goos", p[0], "-goarch", p[1], "-out", out}); err != nil {
			t.Fatal(err)
		}
	}
	sums, err := os.ReadFile(filepath.Join(out, "checksums.txt")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(lines) != 3 {
		t.Fatalf("checksums.txt has %d lines, want 3:\n%s", len(lines), sums)
	}
	for i, want := range []string{"x_2.0.0_darwin_arm64.zip", "x_2.0.0_linux_amd64.zip", "x_2.0.0_windows_amd64.zip"} {
		if !strings.HasSuffix(lines[i], "  "+want) {
			t.Fatalf("line %d = %q, want sorted entry for %s", i, lines[i], want)
		}
	}
	// The entry inside each zip carries the platform's library extension.
	for zipName, entry := range map[string]string{"x_2.0.0_darwin_arm64.zip": "x.dylib", "x_2.0.0_windows_amd64.zip": "x.dll"} {
		zr, err := zip.OpenReader(filepath.Join(out, zipName))
		if err != nil {
			t.Fatal(err)
		}
		name := zr.File[0].Name
		_ = zr.Close()
		if name != entry {
			t.Fatalf("%s holds %q, want %q", zipName, name, entry)
		}
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	so := writeLib(t, dir, []byte("lib"))
	empty := filepath.Join(dir, "empty.so")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "store")
	base := map[string]string{"-so": so, "-id": "cpa-groq", "-version": "0.1.0", "-goos": "linux", "-goarch": "amd64", "-out": out}
	cases := map[string]map[string]string{
		"leading v":      {"-version": "v0.1.0"},
		"non-numeric":    {"-version": "0.1.0-rc1"},
		"bad id":         {"-id": "-bad"},
		"unknown goos":   {"-goos": "plan9"},
		"missing input":  {"-so": filepath.Join(dir, "nope.so")},
		"empty library":  {"-so": empty},
		"missing goarch": {"-goarch": ""},
	}
	for name, override := range cases {
		var args []string
		for k, v := range base {
			if o, ok := override[k]; ok {
				v = o
			}
			args = append(args, k, v)
		}
		if err := run(args); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("no output may be written for rejected input")
	}
}
