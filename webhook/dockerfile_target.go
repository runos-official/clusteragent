package webhook

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxDockerfileSymlinkHops bounds symlink-chain resolution so a cyclical
// chain refuses instead of looping forever. Shared by both the pre-claim
// preflight (validateDockerfileTarget) and the post-extraction resolution
// (splitTarballDockerfile) - review findings 1272 and 1311 found the two
// boundaries applying different limits in different places, and one of
// them (preflight) applying no limit at all to a chain spanning several
// components. Real Dockerfile configurations need at most one or two hops;
// this is generous headroom.
const maxDockerfileSymlinkHops = 10

// dockerfileEntryKind is the one piece of shape resolveDockerfileTarget
// needs about a path: whether it is there, and if so, what it is.
type dockerfileEntryKind int

const (
	dockerfileEntryMissing dockerfileEntryKind = iota
	dockerfileEntryDir
	dockerfileEntryRegular
	dockerfileEntrySymlink
	// dockerfileEntryConflict marks a path the archive declares twice,
	// first as a directory and then as a regular file (review finding
	// 1311's C7): on a real filesystem, extraction's MkdirAll followed by
	// os.Create at the same path fails outright, so the resolver must
	// refuse here too instead of silently keeping only the later entry,
	// the way a plain map overwrite would.
	dockerfileEntryConflict
	// dockerfileEntryOther is any tar entry type the build never treats
	// as a usable Dockerfile target (character device, fifo, hardlink,
	// and so on).
	dockerfileEntryOther
)

// dockerfileEntry is one path's resolved shape, as either boundary's
// dockerfileLookup reports it.
type dockerfileEntry struct {
	kind dockerfileEntryKind
	// linkname is the symlink's own target string, verbatim, only
	// meaningful when kind == dockerfileEntrySymlink. It is NOT resolved;
	// resolveDockerfileSymlink does that.
	linkname string
}

// dockerfileLookup abstracts the one piece of state resolveDockerfileTarget
// needs: what sits at a given tarball-root-relative, slash-separated path.
// validateDockerfileTarget (webhook/tarball_limits.go) implements it over
// the in-memory tar entry set, before anything is extracted;
// splitTarballDockerfile (webhook/cli_deploy_handler.go) implements it over
// the real, already-extracted filesystem. Both then share every resolution
// rule below, closing review findings 1272 and 1311 (preflight and
// extraction disagreeing on the same configured dockerfile path).
type dockerfileLookup interface {
	stat(relPath string) dockerfileEntry
}

// resolveDockerfileTarget walks `cleaned` (already validated by the caller
// as relative and non-escaping) component by component against lookup,
// following a symlink at ANY component - not only the final one, closing
// review finding 1311's C1 - through one shared hop budget. It applies one
// rule for an absolute or escaping symlink target, one rule for a trailing
// slash in a link's target (POSIX requires the landing path to be a
// directory; review finding 1311's C6), and one rule for a directory then
// a regular file declared at the same path (review finding 1311's C7). It
// returns nil only when the fully resolved target is a regular file.
// original is the caller-supplied dockerfile value, used only for error
// text.
func resolveDockerfileTarget(lookup dockerfileLookup, cleaned, original string) error {
	hops := 0
	return resolveDockerfileComponents(lookup, splitDockerfileComponents(cleaned), original, &hops)
}

// splitDockerfileComponents splits an already-path.Clean-ed, relative path
// into its slash-separated components. "" and "." (the tarball root) have
// no components: resolving to the root itself is never a regular file.
func splitDockerfileComponents(cleaned string) []string {
	if cleaned == "" || cleaned == "." {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// joinDockerfileComponent appends one path component to an already-resolved
// base path ("" meaning the tarball root).
func joinDockerfileComponent(base, component string) string {
	if base == "" {
		return component
	}
	return base + "/" + component
}

// resolveDockerfileComponents resolves `components` against lookup,
// starting fresh from the tarball root every call: a symlink hop restarts
// this with a brand-new component list (its resolved target's own
// components, followed by whatever of the original path came after the
// symlink), rather than trying to track a partially-resolved base across
// recursive calls. hops is a running total across the WHOLE resolution
// (every symlink hop anywhere in the walk), not per call.
func resolveDockerfileComponents(lookup dockerfileLookup, components []string, original string, hops *int) error {
	if len(components) == 0 {
		return fmt.Errorf("dockerfile %q is not a regular file", original)
	}
	built := ""
	for i, comp := range components {
		built = joinDockerfileComponent(built, comp)
		last := i == len(components)-1
		entry := lookup.stat(built)
		switch entry.kind {
		case dockerfileEntryMissing:
			return fmt.Errorf("dockerfile %q not found inside tarball", original)
		case dockerfileEntryConflict:
			return fmt.Errorf("dockerfile %q resolves through %q, which the archive declares as both a directory and a regular file", original, built)
		case dockerfileEntryDir:
			if last {
				return fmt.Errorf("dockerfile %q is not a regular file", original)
			}
			continue
		case dockerfileEntryRegular:
			if !last {
				return fmt.Errorf("dockerfile %q treats regular file %q as a directory", original, built)
			}
			return nil
		case dockerfileEntrySymlink:
			*hops++
			if *hops > maxDockerfileSymlinkHops {
				return fmt.Errorf("dockerfile %q has too many nested symlinks", original)
			}
			resolved, err := resolveDockerfileSymlink(lookup, built, entry.linkname, original)
			if err != nil {
				return err
			}
			next := append(splitDockerfileComponents(resolved), components[i+1:]...)
			return resolveDockerfileComponents(lookup, next, original, hops)
		default:
			return fmt.Errorf("dockerfile %q is not a regular file", original)
		}
	}
	return nil
}

// resolveDockerfileSymlink validates linkname's syntax (never absolute,
// never escaping the tarball root once joined against the symlink's own
// directory) and returns the tarball-root-relative path it names. It also
// applies the trailing-slash rule (review finding 1311's C6): a link whose
// target string ends in "/" requires the path it immediately names to be a
// directory (or another symlink, resolved by the caller's continued walk),
// matching POSIX's own symlink resolution semantics for a trailing-slash
// target - the real filesystem refuses to resolve such a link through a
// regular file.
func resolveDockerfileSymlink(lookup dockerfileLookup, linkPath, linkname, original string) (string, error) {
	if path.IsAbs(linkname) {
		return "", fmt.Errorf("dockerfile %q resolves through an absolute symlink target %q", original, linkname)
	}
	trailingSlash := strings.HasSuffix(linkname, "/")
	trimmed := strings.TrimSuffix(linkname, "/")
	parent := path.Dir(linkPath)
	if parent == "." {
		parent = ""
	}
	resolved := path.Clean(path.Join(parent, trimmed))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("dockerfile %q resolves through a symlink that escapes the tarball root", original)
	}
	if resolved == "." {
		resolved = ""
	}
	if trailingSlash {
		landing := lookup.stat(resolved)
		if landing.kind != dockerfileEntryDir && landing.kind != dockerfileEntrySymlink {
			return "", fmt.Errorf("dockerfile %q resolves through a symlink target %q with a trailing slash, but that path is not a directory", original, linkname)
		}
	}
	return resolved, nil
}

// fsDockerfileLookup resolves a dockerfile path against the real,
// already-extracted filesystem rooted at `root`, using Lstat on each
// component instead of Stat, so resolveDockerfileTarget's manual
// component-by-component symlink walk runs here exactly as it does over
// the in-memory tar entry set - rather than relying on the OS's own
// transparent symlink following, which is what let preflight and
// extraction disagree in the first place (review findings 1272 and 1311).
type fsDockerfileLookup struct {
	root string
}

func (l fsDockerfileLookup) stat(relPath string) dockerfileEntry {
	if relPath == "" {
		return dockerfileEntry{kind: dockerfileEntryDir}
	}
	full := filepath.Join(l.root, filepath.FromSlash(relPath))
	info, err := os.Lstat(full)
	if err != nil {
		return dockerfileEntry{kind: dockerfileEntryMissing}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(full)
		if readErr != nil {
			return dockerfileEntry{kind: dockerfileEntryOther}
		}
		return dockerfileEntry{kind: dockerfileEntrySymlink, linkname: filepath.ToSlash(target)}
	}
	if info.IsDir() {
		return dockerfileEntry{kind: dockerfileEntryDir}
	}
	if info.Mode().IsRegular() {
		return dockerfileEntry{kind: dockerfileEntryRegular}
	}
	return dockerfileEntry{kind: dockerfileEntryOther}
}
