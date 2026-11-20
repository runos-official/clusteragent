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
// destDir/abs/Dockerfile, not destDir/Dockerfile. This runs the SAME
// path.Clean(strings.TrimPrefix(name, "./")) a caller's own traversal
// check (checkTarballContainment) uses, then additionally strips a
// leading "/". Callers that need to detect a traversing ("..") name must
// still check their own unstripped path.Clean result: a leading ".." in
// an absolute name is already collapsed by path.Clean's own rooted-path
// rule before this function is ever reached, exactly as it always was,
// and this function does not change that detection either way.
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
// the three archive-wide refusals extractTarball itself enforces while
// writing to disk (review finding 1311's items 2-4, repeats of findings
// this story closes as C2-C4): a traversing entry name, an absolute
// symlink target, and a symlink whose resolved target escapes the
// destination. These need only the tar headers, so FPL430's "every check
// that CAN run before the claim must run there" moves them here,
// independent of the configured dockerfile (or the lack of one). This does
// NOT replace extractTarball's own copies, which stay as defence in depth
// for whatever reaches extraction regardless (a direct call, a future
// caller that skips this preflight).
//
// Review finding 1452: it also tracks, archive-wide and independent of the
// configured dockerfile, each path's declared kind, and refuses the two
// contradictions extraction's os.MkdirAll/os.Create pair cannot survive on
// a real filesystem - a regular entry where a directory was already
// declared at the SAME path, and any entry whose ancestor path was already
// declared a regular file. These are the only two contradictions that
// pairing can hit; a repeated directory, a repeated regular file, and a
// symlink declared over or under anything all extract without error and
// must keep passing, so only TypeDir and TypeReg are tracked here.
func checkTarballContainment(data []byte) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload is not a gzipped tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	kinds := make(map[string]dockerfileEntryKind)
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
		cleaned := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return fmt.Errorf("invalid file path in tarball: %s", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeSymlink {
			if path.IsAbs(hdr.Linkname) {
				return fmt.Errorf("symlink target is absolute: %s -> %s", hdr.Name, hdr.Linkname)
			}
			resolved := path.Clean(path.Join(path.Dir(cleaned), hdr.Linkname))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink escapes destination: %s -> %s", hdr.Name, hdr.Linkname)
			}
		}

		normalized := normalizeTarEntryName(hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			kinds[normalized] = dockerfileEntryDir
		case tar.TypeReg:
			if kinds[normalized] == dockerfileEntryDir {
				return fmt.Errorf("entry %q is declared as both a directory and a regular file", normalized)
			}
			kinds[normalized] = dockerfileEntryRegular
			for dir := path.Dir(normalized); dir != "." && dir != "/"; dir = path.Dir(dir) {
				if kinds[dir] == dockerfileEntryRegular {
					return fmt.Errorf("entry %q has ancestor %q already declared as a regular file", normalized, dir)
				}
			}
		}
	}
}
