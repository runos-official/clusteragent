package webhook

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

// entryTooLargeError reports a tarball entry over MaxFileSize (FCR 1631).
type entryTooLargeError struct {
	name string
	size int64
}

func (e *entryTooLargeError) Error() string {
	return fmt.Sprintf("tarball entry %q is %d bytes, over the per-file limit of %d bytes", e.name, e.size, MaxFileSize)
}

// checkTarballLimits reads every entry header of a gzipped tarball and refuses
// it when an entry is over MaxFileSize or the entry count is over
// MaxEntryCount. It writes nothing, so the upload handler can refuse a tarball
// before it answers 202 and before the archive is stored.
func checkTarballLimits(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload is not a gzipped tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := 0
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
		if hdr.Typeflag == tar.TypeReg && hdr.Size > MaxFileSize {
			return &entryTooLargeError{name: hdr.Name, size: hdr.Size}
		}
	}
}
