package webhook

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FCR 1631: an entry over the per-file cap was cut at the cap and written,
// extraction reported success, and the build failed later for an unrelated
// reason. Now the whole tarball is refused and the error names the entry.

// lowerFileCap sets MaxFileSize to n for one test.
func lowerFileCap(t *testing.T, n int64) {
	t.Helper()
	orig := MaxFileSize
	MaxFileSize = n
	t.Cleanup(func() { MaxFileSize = orig })
}

func TestExtractTarball_OversizedEntryIsRefusedNotTruncated(t *testing.T) {
	lowerFileCap(t, 10)
	dest := t.TempDir()
	data := buildTarGz(t, []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM x"},
		{name: "big/blob.bin", typeflag: tar.TypeReg, body: "0123456789A"}, // 11 bytes, cap 10
	})
	err := extractTarball(data, dest)
	if err == nil {
		t.Fatal("expected the oversized entry to be refused, got nil")
	}
	if !strings.Contains(err.Error(), "big/blob.bin") || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("error %q must name the entry and the per-file limit", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(dest, "big", "blob.bin")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("no truncated file may be written; stat err = %v", statErr)
	}
}

func TestExtractTarball_EntryExactlyAtCapIsAccepted(t *testing.T) {
	lowerFileCap(t, 10)
	dest := t.TempDir()
	data := buildTarGz(t, []tarEntry{{name: "ten.bin", typeflag: tar.TypeReg, body: "0123456789"}})
	if err := extractTarball(data, dest); err != nil {
		t.Fatalf("an entry exactly at the cap must be accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "ten.bin"))
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("file = %q, %v; want the full 10 bytes", got, err)
	}
}

func TestCheckTarballLimits(t *testing.T) {
	lowerFileCap(t, 10)

	ok := buildTarGz(t, []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM x"},
		{name: "src", typeflag: tar.TypeDir},
	})
	if err := checkTarballLimits(ok); err != nil {
		t.Fatalf("a tarball within every limit must pass: %v", err)
	}

	big := buildTarGz(t, []tarEntry{{name: "assets/model.bin", typeflag: tar.TypeReg, body: "0123456789AB"}})
	err := checkTarballLimits(big)
	var tooLarge *entryTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want an entryTooLargeError", err)
	}
	for _, want := range []string{"assets/model.bin", "12 bytes", "per-file limit of 10 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err.Error(), want)
		}
	}

	if err := checkTarballLimits([]byte("not a gzip stream")); err == nil {
		t.Fatal("a body that is not a gzipped tarball must be refused")
	}
}

// lowerTotalCap sets MaxTotalExtractedBytes to n for one test.
func lowerTotalCap(t *testing.T, n int64) {
	t.Helper()
	orig := MaxTotalExtractedBytes
	MaxTotalExtractedBytes = n
	t.Cleanup(func() { MaxTotalExtractedBytes = orig })
}

// TestCheckTarballLimits_AggregateSize pins review finding 1078: the
// preflight must also refuse on the TOTAL declared decompressed size, not
// just each individual file, so the same decompression-bomb case
// extractTarball catches during the (post-claim) asynchronous build is
// instead caught here, before any claim or persistence. Each file here is
// comfortably under the per-file cap; only the sum trips the guard.
func TestCheckTarballLimits_AggregateSize(t *testing.T) {
	lowerTotalCap(t, 15)

	ok := buildTarGz(t, []tarEntry{
		{name: "a.txt", typeflag: tar.TypeReg, body: "aaaaaaa"}, // 7
		{name: "b.txt", typeflag: tar.TypeReg, body: "bbbbbbb"}, // 7, total 14 <= 15
	})
	if err := checkTarballLimits(ok); err != nil {
		t.Fatalf("a tarball within the aggregate cap must pass: %v", err)
	}

	over := buildTarGz(t, []tarEntry{
		{name: "a.txt", typeflag: tar.TypeReg, body: "aaaaaaa"},  // 7
		{name: "b.txt", typeflag: tar.TypeReg, body: "bbbbbbbb"}, // 8, total 15... still boundary
		{name: "c.txt", typeflag: tar.TypeReg, body: "c"},        // +1, total 16 > 15
	})
	err := checkTarballLimits(over)
	if err == nil || !strings.Contains(err.Error(), "decompression bomb") {
		t.Fatalf("err = %v, want a decompression-bomb refusal once the aggregate cap is exceeded", err)
	}
}

// TestValidateDockerfileTarget pins review finding 1078: a configured
// (non-default) Dockerfile target is validated against the tarball's own
// entries before claim, the same traversal and existence rules
// splitTarballDockerfile applies later against the extracted disk tree.
func TestValidateDockerfileTarget(t *testing.T) {
	data := buildTarGz(t, []tarEntry{
		{name: "apps/api/Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "not-a-file", typeflag: tar.TypeDir},
	})

	cases := []struct {
		name          string
		dockerfile    string
		wantErrSubstr string
	}{
		{name: "empty input is BuildKit's default, not validated here", dockerfile: ""},
		{name: "whitespace-only input is also the default", dockerfile: "   "},
		{name: "present entry is accepted", dockerfile: "apps/api/Dockerfile"},
		{name: "absolute path rejected", dockerfile: "/etc/passwd", wantErrSubstr: "must be relative"},
		{name: "path escaping the tarball root rejected", dockerfile: "../../etc/passwd", wantErrSubstr: "escapes tarball root"},
		{name: "missing entry rejected", dockerfile: "apps/api/Dockerfile.missing", wantErrSubstr: "not found inside tarball"},
		{name: "directory at the configured path rejected", dockerfile: "not-a-file", wantErrSubstr: "not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDockerfileTarget(data, tc.dockerfile)
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErrSubstr)
			}
		})
	}
}
