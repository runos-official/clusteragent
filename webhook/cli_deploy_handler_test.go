package webhook

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry describes one member to write into a test tarball.
type tarEntry struct {
	name     string
	typeflag byte
	linkname string // for symlinks
	body     string // for regular files
}

// buildTarGz packs entries into a gzipped tar archive and returns the bytes.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o644,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// countOutside reports how many filesystem entries exist under root other than
// root itself. Used to assert a rejected extraction wrote nothing escaping it.
func countOutside(t *testing.T, root string) {
	t.Helper()
	// Walk the parent of root and confirm nothing landed as a sibling.
	parent := filepath.Dir(root)
	siblings, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent dir: %v", err)
	}
	for _, s := range siblings {
		if filepath.Join(parent, s.Name()) != filepath.Clean(root) {
			t.Errorf("unexpected entry written outside destDir: %s", filepath.Join(parent, s.Name()))
		}
	}
}

// TestExtractTarball_Rejects pins the path-traversal and symlink-escape
// defenses in extractTarball: a ../ path, an absolute symlink target, and a
// symlink whose resolved target shares the destDir prefix but is a sibling
// (e.g. dest vs dest-evil) must all be rejected, and nothing may be written
// outside destDir.
func TestExtractTarball_Rejects(t *testing.T) {
	t.Run("dot-dot path traversal", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "dest")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		data := buildTarGz(t, []tarEntry{
			{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
		})
		err := extractTarball(data, dest)
		if err == nil {
			t.Fatal("expected rejection of ../ path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "invalid file path") {
			t.Errorf("error %q should report an invalid path", err.Error())
		}
		countOutside(t, dest)
	})

	t.Run("absolute symlink target", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "dest")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		data := buildTarGz(t, []tarEntry{
			{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		})
		err := extractTarball(data, dest)
		if err == nil {
			t.Fatal("expected rejection of absolute symlink target, got nil")
		}
		if !strings.Contains(err.Error(), "absolute") {
			t.Errorf("error %q should report an absolute symlink target", err.Error())
		}
		if _, statErr := os.Lstat(filepath.Join(dest, "link")); statErr == nil {
			t.Error("symlink should not have been created")
		}
		countOutside(t, dest)
	})

	t.Run("sibling-prefix symlink escape", func(t *testing.T) {
		base := t.TempDir()
		dest := filepath.Join(base, "app")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		// A symlink at app/link pointing to ../app-evil/secret resolves to
		// base/app-evil/secret, which shares the "app" prefix but is a sibling
		// of destDir. A bare HasPrefix(destDir) check would wrongly allow it;
		// the trailing-separator guard must reject it.
		data := buildTarGz(t, []tarEntry{
			{name: "link", typeflag: tar.TypeSymlink, linkname: "../app-evil/secret"},
		})
		err := extractTarball(data, dest)
		if err == nil {
			t.Fatal("expected rejection of sibling-prefix symlink escape, got nil")
		}
		if !strings.Contains(err.Error(), "escapes destination") {
			t.Errorf("error %q should report a symlink escape", err.Error())
		}
		if _, statErr := os.Lstat(filepath.Join(dest, "link")); statErr == nil {
			t.Error("symlink should not have been created")
		}
		countOutside(t, dest)
	})

	t.Run("valid contained symlink is accepted", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "dest")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		data := buildTarGz(t, []tarEntry{
			{name: "target.txt", typeflag: tar.TypeReg, body: "hello"},
			{name: "link", typeflag: tar.TypeSymlink, linkname: "target.txt"},
		})
		if err := extractTarball(data, dest); err != nil {
			t.Fatalf("contained symlink should be accepted: %v", err)
		}
		if _, statErr := os.Lstat(filepath.Join(dest, "link")); statErr != nil {
			t.Errorf("expected symlink to be created: %v", statErr)
		}
		countOutside(t, dest)
	})
}

// TestExtractTarball_EntryCountCap pins the decompression-bomb defense for the
// many-tiny-files vector: a tarball whose entry count exceeds MaxEntryCount is
// rejected before it can exhaust inodes/CPU, even though every entry is small
// enough to pass the per-file MaxFileSize LimitReader.
func TestExtractTarball_EntryCountCap(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	// MaxEntryCount+1 tiny regular files: one over the cap. Each is a few bytes
	// so total size is trivial; only the entry COUNT trips the guard.
	entries := make([]tarEntry, 0, MaxEntryCount+1)
	for i := 0; i <= MaxEntryCount; i++ {
		entries = append(entries, tarEntry{
			name:     fmt.Sprintf("f%d.txt", i),
			typeflag: tar.TypeReg,
			body:     "x",
		})
	}
	data := buildTarGz(t, entries)

	err := extractTarball(data, dest)
	if err == nil {
		t.Fatal("expected rejection once entry count exceeds MaxEntryCount, got nil")
	}
	if !strings.Contains(err.Error(), "too many entries") {
		t.Errorf("error %q should report too many entries", err.Error())
	}
}

// TestExtractTarball_EntryCountCap_BoundaryAccepted confirms the cap is an
// upper bound, not off-by-one: exactly MaxEntryCount entries is accepted. Kept
// small in body size so this stays fast.
func TestExtractTarball_EntryCountCap_BoundaryAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-entry-count extraction in -short")
	}
	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	entries := make([]tarEntry, 0, MaxEntryCount)
	for i := 0; i < MaxEntryCount; i++ {
		entries = append(entries, tarEntry{
			name:     fmt.Sprintf("f%d.txt", i),
			typeflag: tar.TypeReg,
			body:     "x",
		})
	}
	data := buildTarGz(t, entries)
	if err := extractTarball(data, dest); err != nil {
		t.Fatalf("exactly MaxEntryCount entries should be accepted, got: %v", err)
	}
}

// TestExtractTarball_TotalBytesCap pins the total-decompressed-bytes guard by
// driving the real extractor past the cap. To keep the test fast it overrides
// the package caps to small values for the duration of the test (restored via
// defer): a handful of small files whose summed size exceeds the lowered total
// cap must be rejected, even though each file is individually under the
// per-file limit. This exercises the live accumulation in extractTarball, not a
// reimplementation of it.
func TestExtractTarball_TotalBytesCap(t *testing.T) {
	origTotal, origEntries := MaxTotalExtractedBytes, MaxEntryCount
	MaxTotalExtractedBytes = 10 // bytes
	MaxEntryCount = 1000
	defer func() {
		MaxTotalExtractedBytes = origTotal
		MaxEntryCount = origEntries
	}()

	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	// Two 6-byte files = 12 bytes total, over the lowered 10-byte cap; each is
	// individually under MaxFileSize, so only the TOTAL guard can reject this.
	data := buildTarGz(t, []tarEntry{
		{name: "a.txt", typeflag: tar.TypeReg, body: "aaaaaa"},
		{name: "b.txt", typeflag: tar.TypeReg, body: "bbbbbb"},
	})
	err := extractTarball(data, dest)
	if err == nil {
		t.Fatal("expected rejection once total bytes exceed MaxTotalExtractedBytes, got nil")
	}
	if !strings.Contains(err.Error(), "decompression bomb") {
		t.Errorf("error %q should report a decompression bomb", err.Error())
	}
}

// TestExtractTarball_UnderCapsAccepted confirms a normal small tarball within
// both the total-bytes and entry-count caps extracts cleanly.
func TestExtractTarball_UnderCapsAccepted(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dest")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	data := buildTarGz(t, []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "main.go", typeflag: tar.TypeReg, body: "package main"},
	})
	if err := extractTarball(data, dest); err != nil {
		t.Fatalf("small tarball under all caps should extract, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "Dockerfile")); statErr != nil {
		t.Errorf("expected Dockerfile to be extracted: %v", statErr)
	}
}

// TestSplitTarballDockerfile pins the iter-27 I27-Y dockerfile split.
//
// Pre-fix the CLI deploy handler hardcoded ContextPath/DockerfileDir/
// DockerfileFilename empty, so BuildKit's `--opt filename=Dockerfile`
// always looked for a Dockerfile at the tarball root and monorepo
// deploys with `dockerfile: apps/api/Dockerfile` failed with
// `failed to read dockerfile: open Dockerfile: no such file or directory`.
//
// The split must produce: dir = <tarballRoot>/<subdir>, filename =
// basename(dockerfile). Empty input preserves the pre-monorepo behaviour
// (BuildKit defaults: context root + "Dockerfile").
func TestSplitTarballDockerfile(t *testing.T) {
	tmpDir := t.TempDir()
	apiDir := filepath.Join(tmpDir, "apps", "api")
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dockerfilePath := filepath.Join(apiDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte("FROM alpine"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	// Companion files used by the "is a regular file" check.
	rootDockerfile := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(rootDockerfile, []byte("FROM alpine"), 0o644); err != nil {
		t.Fatalf("write root Dockerfile: %v", err)
	}
	someDir := filepath.Join(tmpDir, "not-a-file")
	if err := os.MkdirAll(someDir, 0o755); err != nil {
		t.Fatalf("mkdir not-a-file: %v", err)
	}

	cases := []struct {
		name          string
		dockerfile    string
		wantDir       string
		wantFilename  string
		wantErrSubstr string
	}{
		{
			name:         "empty input returns empty (BuildKit defaults)",
			dockerfile:   "",
			wantDir:      "",
			wantFilename: "",
		},
		{
			name:         "whitespace-only input returns empty",
			dockerfile:   "   ",
			wantDir:      "",
			wantFilename: "",
		},
		{
			name:         "monorepo subdir Dockerfile",
			dockerfile:   "apps/api/Dockerfile",
			wantDir:      apiDir,
			wantFilename: "Dockerfile",
		},
		{
			name:         "root Dockerfile via explicit path",
			dockerfile:   "Dockerfile",
			wantDir:      tmpDir,
			wantFilename: "Dockerfile",
		},
		{
			name:          "absolute path rejected",
			dockerfile:    "/etc/passwd",
			wantErrSubstr: "must be relative",
		},
		{
			name:          "path escaping tarball root rejected",
			dockerfile:    "../../../etc/passwd",
			wantErrSubstr: "escapes tarball root",
		},
		{
			name:          "missing file rejected",
			dockerfile:    "apps/api/Dockerfile.missing",
			wantErrSubstr: "not found inside tarball",
		},
		{
			name:          "directory at the dockerfile path rejected",
			dockerfile:    "not-a-file",
			wantErrSubstr: "not a regular file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, filename, err := splitTarballDockerfile(tmpDir, tc.dockerfile)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got dir=%q filename=%q", tc.wantErrSubstr, dir, filename)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dir != tc.wantDir {
				t.Errorf("dir mismatch: got %q want %q", dir, tc.wantDir)
			}
			if filename != tc.wantFilename {
				t.Errorf("filename mismatch: got %q want %q", filename, tc.wantFilename)
			}
		})
	}
}
