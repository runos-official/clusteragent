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
