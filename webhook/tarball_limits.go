package webhook

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"path"
	"strings"
)

// entryTooLargeError reports a tarball entry over MaxFileSize (FCR 1631).
type entryTooLargeError struct {
	name string
	size int64
}

func (e *entryTooLargeError) Error() string {
	return fmt.Sprintf("tarball entry %q is %d bytes, over the per-file limit of %d bytes", e.name, e.size, MaxFileSize)
}

// checkTarballLimits reads every entry header of a gzipped tarball and
// refuses it when an entry is over MaxFileSize, the entry count is over
// MaxEntryCount, or the declared total decompressed size of all regular
// files is over MaxTotalExtractedBytes. It writes nothing, so the upload
// handler can refuse a tarball before it answers 202, before the token is
// claimed, and before the archive is stored.
//
// The aggregate check mirrors extractTarball's own running total (review
// finding 1078: "Keep preflight and extraction rules consistent") so a
// tarball that would later trip the decompression-bomb guard during the
// asynchronous build is refused here instead, before any claim or persist.
func checkTarballLimits(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload is not a gzipped tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := 0
	var totalSize int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}
		entries++
		if entries > MaxEntryCount {
			return fmt.Errorf("tarball has too many entries (limit %d)", MaxEntryCount)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > MaxFileSize {
			return &entryTooLargeError{name: hdr.Name, size: hdr.Size}
		}
		totalSize += hdr.Size
		if totalSize > MaxTotalExtractedBytes {
			return fmt.Errorf("tarball decompresses to more than %d bytes: possible decompression bomb", MaxTotalExtractedBytes)
		}
	}
}

// validateDockerfileTarget checks that a configured (non-default) Dockerfile
// target exists in the tarball as a regular file and stays inside the
// tarball root, without extracting anything to disk. It mirrors
// splitTarballDockerfile's traversal rules on the tar entry names instead
// of real files (review finding 1078): that disk-backed check still runs
// during the asynchronous build on the same dockerfile value, so the two
// must agree.
//
// Empty dockerfile is BuildKit's default ("Dockerfile" at the context root,
// resolved by BuildKit itself) and is not validated here, matching
// splitTarballDockerfile's own empty-input behavior: there both return
// immediately with no check.
func validateDockerfileTarget(data []byte, dockerfile string) error {
	trimmed := strings.TrimSpace(dockerfile)
	if trimmed == "" {
		return nil
	}
	if path.IsAbs(trimmed) {
		return fmt.Errorf("dockerfile must be relative to the tarball root, got absolute path %q", trimmed)
	}
	cleaned := path.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("dockerfile %q escapes tarball root", trimmed)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload is not a gzipped tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("dockerfile %q not found inside tarball", trimmed)
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name != cleaned {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("dockerfile %q is not a regular file", trimmed)
		}
		return nil
	}
}
