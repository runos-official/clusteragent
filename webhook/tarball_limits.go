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
// target exists in the tarball as a regular file, or a symlink that
// resolves to one, and stays inside the tarball root, without extracting
// anything to disk. It runs the tar entry set through the same shared
// resolver (resolveDockerfileTarget, webhook/dockerfile_target.go) that
// splitTarballDockerfile runs the real extracted filesystem through
// (review finding 1078's "keep preflight and extraction rules consistent";
// review finding 1311's closure: the two boundaries must share ONE
// resolver, not two that happen to agree on some inputs).
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
	var headers []*tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}
		headers = append(headers, hdr)
	}

	return resolveDockerfileTarget(newTarDockerfileLookup(headers), cleaned, trimmed)
}

// tarDockerfileLookup resolves a dockerfile path against the in-memory tar
// entry set collected from the archive's headers, before anything is
// extracted to disk.
type tarDockerfileLookup struct {
	entries map[string]dockerfileEntry
	// implicitDirs holds every path that is a proper ancestor of some
	// entry's name but has no explicit tar.TypeDir header of its own - the
	// common case for a tar producer that only emits leaf entries.
	// extractTarball calls os.MkdirAll on an entry's parent before writing
	// it (cli_deploy_handler.go), so the real, post-extraction filesystem
	// has these directories regardless; the lookup must agree, or every
	// Dockerfile under such a path would wrongly refuse as "not found".
	implicitDirs map[string]bool
}

// normalizeTarEntryName returns the tarball-root-relative path extraction
// actually writes an entry to (review finding 1451's instance 2):
// extractTarball's filepath.Join(destDir, header.Name) absorbs a leading
// "/" into the join instead of treating it as a jump to the real
// filesystem root, so an entry named "/abs/Dockerfile" lands at
// destDir/abs/Dockerfile, not destDir/Dockerfile.
//
// It is a KEY, not a containment test, and it must never be used as one:
// path.Clean's rooted-path rule discards a ".." that follows a leading
// "/", so "/../escape.txt" normalizes to the innocent-looking
// "escape.txt" even though extraction's own join escapes the destination
// with it (review finding 1456). tarEntryEscapes is the one containment
// authority; call that before trusting a name this function returns.
func normalizeTarEntryName(name string) string {
	cleaned := path.Clean(strings.TrimPrefix(name, "./"))
	return strings.TrimPrefix(cleaned, "/")
}

// newTarDockerfileLookup builds the lookup from the archive's headers, IN
// ARCHIVE ORDER, mirroring what extraction does to the real filesystem well
// enough to detect the one conflict that matters here (review finding
// 1311's C7): a path declared as a directory and then, later, as a regular
// file. A plain map overwrite would silently keep only the later entry;
// extraction's MkdirAll followed by os.Create on the same real path fails
// outright, so this records the conflict instead. Every other repeat
// (regular-over-regular, symlink-over-anything, dir-over-dir) keeps the
// simple last-one-wins behavior this map already had, since no instance in
// review finding 1311's finite list needs more than that.
//
// Review finding 1451's instance 1: extraction writes an entry THROUGH any
// already-contained ancestor symlink transparently (the OS resolves
// "link/Dockerfile" to "real/Dockerfile" once "link" exists on disk as a
// symlink to "real"), so an entry's real, on-disk location can differ from
// its own literal tar name. resolveTarAncestorSymlinks resolves that against
// what has been registered so far (archive order: an ancestor dir or
// symlink conventionally precedes its descendants) and the entry is
// registered under the resolved location instead, matching what
// fsDockerfileLookup will actually find after real extraction.
func newTarDockerfileLookup(headers []*tar.Header) *tarDockerfileLookup {
	lookup := &tarDockerfileLookup{
		entries:      make(map[string]dockerfileEntry),
		implicitDirs: make(map[string]bool),
	}
	for _, hdr := range headers {
		name := normalizeTarEntryName(hdr.Name)
		if name == "" || name == "." {
			continue
		}
		var next dockerfileEntry
		switch hdr.Typeflag {
		case tar.TypeDir:
			next = dockerfileEntry{kind: dockerfileEntryDir}
		case tar.TypeReg:
			next = dockerfileEntry{kind: dockerfileEntryRegular}
		case tar.TypeSymlink:
			next = dockerfileEntry{kind: dockerfileEntrySymlink, linkname: hdr.Linkname}
		default:
			next = dockerfileEntry{kind: dockerfileEntryOther}
		}
		if resolved, ok := resolveTarAncestorSymlinks(lookup, name); ok {
			name = resolved
		}
		lookup.register(name, next)
	}
	return lookup
}

// register records one entry at a normalized path, applying review
// finding 1311's C7/1452's directory-then-regular-file conflict rule; see
// newTarDockerfileLookup's doc comment.
func (l *tarDockerfileLookup) register(name string, next dockerfileEntry) {
	if existing, ok := l.entries[name]; ok && existing.kind == dockerfileEntryDir && next.kind == dockerfileEntryRegular {
		l.entries[name] = dockerfileEntry{kind: dockerfileEntryConflict}
	} else {
		l.entries[name] = next
	}
	for dir := path.Dir(name); dir != "." && dir != "/"; dir = path.Dir(dir) {
		l.implicitDirs[dir] = true
	}
}

// resolveTarAncestorSymlinks resolves any symlink among name's ANCESTOR
// directory components (never its final component) against what lookup
// has registered so far, returning name with every such ancestor replaced
// by its resolved target. ok is false when no ancestor component is a
// registered symlink (the common case: the caller keeps name unchanged),
// or when resolution fails for any reason (an absolute or escaping link
// target, or the shared hop limit) - in that case the caller also keeps
// the literal name, which is safe: a failed resolution here only means
// this entry won't be found under an alias, never that something is
// wrongly accepted.
func resolveTarAncestorSymlinks(lookup *tarDockerfileLookup, name string) (string, bool) {
	components := splitDockerfileComponents(name)
	if len(components) < 2 {
		return "", false
	}
	built := ""
	changed := false
	hops := 0
	for _, comp := range components[:len(components)-1] {
		candidate := joinDockerfileComponent(built, comp)
		entry := lookup.stat(candidate)
		if entry.kind != dockerfileEntrySymlink {
			built = candidate
			continue
		}
		hops++
		if hops > maxDockerfileSymlinkHops {
			return "", false
		}
		resolved, err := resolveDockerfileSymlink(lookup, candidate, entry.linkname, name)
		if err != nil {
			return "", false
		}
		built = resolved
		changed = true
	}
	if !changed {
		return "", false
	}
	return joinDockerfileComponent(built, components[len(components)-1]), true
}

func (l *tarDockerfileLookup) stat(relPath string) dockerfileEntry {
	if relPath == "" {
		return dockerfileEntry{kind: dockerfileEntryDir}
	}
	if entry, ok := l.entries[relPath]; ok {
		return entry
	}
	if l.implicitDirs[relPath] {
		return dockerfileEntry{kind: dockerfileEntryDir}
	}
	return dockerfileEntry{kind: dockerfileEntryMissing}
}

// checkTarballContainment re-applies, header-only and before any claim,
// every refusal extractTarball itself reaches while writing to disk, so an
// archive extraction cannot write is refused before the token is claimed
// and the archive is persisted (review finding 1311's items 2-4, closed as
// C2-C4; review findings 1452, 1456 and 1457). FPL430's "every check that
// CAN run before the claim must run there" applies: all of these need only
// the tar headers. This does NOT replace extractTarball's own checks,
// which stay as defence in depth for whatever reaches extraction anyway.
//
// It refuses three things directly, matching extraction's own explicit
// guards: an entry name that escapes the destination, an absolute symlink
// target, and a symlink target that escapes the destination. Both escape
// tests use tarEntryEscapes, which reproduces extraction's
// filepath.Join arithmetic rather than re-cleaning the name, because
// path.Clean discards a ".." that follows a leading "/" while the join
// keeps it (review finding 1456).
//
// Everything else is decided by replaying the headers through
// tarExtractSim, which models the three syscalls extraction uses and so
// refuses exactly the archives extraction cannot write: a path the archive
// declares with contradictory kinds in either order, an entry under a path
// that is not a directory, a symlink at an occupied path, a symlink whose
// parent does not exist yet, and the cases where a symlink makes two
// literal names one real path.
func checkTarballContainment(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload is not a gzipped tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	sim := newTarExtractSim()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}
		if hdr.Name == "./" || hdr.Name == "." {
			continue
		}
		if tarEntryEscapes(hdr.Name) {
			return fmt.Errorf("invalid file path in tarball: %s", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeSymlink {
			if path.IsAbs(hdr.Linkname) {
				return fmt.Errorf("symlink target is absolute: %s -> %s", hdr.Name, hdr.Linkname)
			}
			// Concatenated, never path.Join-ed: Join cleans, and the
			// whole point of tarEntryEscapes is to count the ".."
			// elements a clean would cancel out or discard.
			parent := path.Dir(normalizeTarEntryName(hdr.Name))
			if parent == "." {
				parent = ""
			}
			if tarEntryEscapes(parent + "/" + hdr.Linkname) {
				return fmt.Errorf("symlink escapes destination: %s -> %s", hdr.Name, hdr.Linkname)
			}
		}
		if err := sim.apply(hdr.Name, hdr.Typeflag, hdr.Linkname); err != nil {
			return err
		}
	}
}
