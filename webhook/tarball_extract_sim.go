package webhook

import (
	"archive/tar"
	"fmt"
	"path"
	"strings"
)

// tarEntryEscapes reports whether a tar entry name, or a symlink target
// resolved against its own directory, leaves the extraction destination.
//
// It reproduces extractTarball's own arithmetic instead of re-cleaning the
// name on its own (review finding 1456): extraction computes
// filepath.Join(destDir, name), which ABSORBS a leading "/" and keeps
// every ".." element, so the ".." elements pop components of destDir once
// the name's own components are used up. path.Clean on the bare name
// cannot see that, because its rooted-path rule silently discards a ".."
// that follows a leading "/" ("/../escape.txt" cleans to "/escape.txt").
// Walking the components and tracking the depth below destDir gives
// exactly extraction's answer: the entry escapes when the depth would go
// negative at any point.
//
// It deliberately does NOT refuse every name that merely CONTAINS a ".."
// component. Measured against extractTarball: "a/../b", "./a/../b" and
// "a/b/../../c" all extract successfully, because their net ascent never
// leaves destDir, so refusing them would refuse legitimate uploads.
func tarEntryEscapes(name string) bool {
	depth := 0
	for _, comp := range strings.Split(strings.TrimPrefix(name, "./"), "/") {
		switch comp {
		case "", ".":
			continue
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}

// tarSimKind is what the extraction simulator believes sits at one
// normalized, destination-relative path at a given point in the archive.
type tarSimKind int

const (
	tarSimNone tarSimKind = iota
	tarSimDir
	tarSimReg
	tarSimSymlink
)

// tarExtractSim replays, header by header and in archive order, the
// filesystem state extractTarball actually builds, so a pre-claim check
// can refuse exactly the archives extraction cannot write and no others
// (review findings 1452, 1456 and 1457).
//
// Every rule below was derived by measuring extractTarball against a real
// temporary directory for the full cross product of (declared kind at a
// path) x (entry kind written there) and (declared kind of an ancestor) x
// (entry kind written under it); TestTarExtractSimParity_SamePath and
// TestTarExtractSimParity_Ancestor keep that cross product green.
//
// The three syscalls extraction uses behave as follows, and the simulator
// mirrors each one:
//   - os.MkdirAll succeeds on a path that already resolves THROUGH symlinks
//     to a directory, and fails (ENOTDIR, or EEXIST for a dangling symlink)
//     on anything else that already exists.
//   - os.Create follows symlinks, so it fails (EISDIR) only when the path
//     resolves to a directory; through a dangling symlink it creates the
//     target instead of failing.
//   - os.Symlink never follows and never replaces: it fails (EEXIST) when
//     anything at all already occupies the path, and it has no MkdirAll of
//     its own, so it also fails (ENOENT/ENOTDIR) unless the parent already
//     resolves to a directory.
type tarExtractSim struct {
	kinds map[string]tarSimKind
	links map[string]string
}

func newTarExtractSim() *tarExtractSim {
	return &tarExtractSim{kinds: map[string]tarSimKind{}, links: map[string]string{}}
}

// kindOf reports what sits at p. The empty path is the destination
// directory itself, which always exists.
func (s *tarExtractSim) kindOf(p string) tarSimKind {
	if p == "" || p == "." {
		return tarSimDir
	}
	return s.kinds[p]
}

// resolveLink returns the destination-relative path the symlink at p
// names. ok is false for an absolute target, an escaping target, or a p
// that is not a symlink: in each case extraction cannot descend there.
func (s *tarExtractSim) resolveLink(p string) (string, bool) {
	link, ok := s.links[p]
	if !ok || path.IsAbs(link) {
		return "", false
	}
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	resolved := path.Clean(path.Join(parent, strings.TrimSuffix(link, "/")))
	if resolved == "." {
		return "", true
	}
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", false
	}
	return resolved, true
}

// resolvesToDir reports whether p, after following every symlink in the
// chain, is a directory extraction can descend into right now. A dangling
// symlink, a symlink chain that ends at a regular file, and a path nothing
// has created yet are all false.
func (s *tarExtractSim) resolvesToDir(p string) bool {
	for hops := 0; hops <= maxDockerfileSymlinkHops; hops++ {
		switch s.kindOf(p) {
		case tarSimDir:
			return true
		case tarSimSymlink:
			next, ok := s.resolveLink(p)
			if !ok {
				return false
			}
			p = next
		default:
			return false
		}
	}
	return false
}

// followDirLinks walks a chain of symlinks from p and returns the last
// path it names, so an ancestor symlink can be replaced by the real
// directory it stands for.
func (s *tarExtractSim) followDirLinks(p string) string {
	for hops := 0; hops < maxDockerfileSymlinkHops; hops++ {
		if s.kindOf(p) != tarSimSymlink {
			return p
		}
		next, ok := s.resolveLink(p)
		if !ok {
			return p
		}
		p = next
	}
	return p
}

// realPath rewrites p so that every ANCESTOR symlink is replaced by what
// it resolves to, which is the path the kernel actually operates on when
// extraction writes p (review finding 1457's group C: two literal entry
// names that resolve to ONE real path must be seen as one path, or a
// contradiction between them is invisible). The final component is never
// followed, because none of the three syscalls follows it for creation.
func (s *tarExtractSim) realPath(p string) string {
	components := splitDockerfileComponents(p)
	if len(components) < 2 {
		return p
	}
	built := ""
	for _, comp := range components[:len(components)-1] {
		candidate := joinDockerfileComponent(built, comp)
		// Only an ancestor symlink that ALREADY resolves to an existing
		// directory is followed. A dangling one, or one that ends at a
		// regular file, is left in place on purpose: the kernel cannot
		// descend through it either, and os.MkdirAll fails on the symlink
		// itself (EEXIST for a dangling link, ENOTDIR for a link to a
		// file) rather than creating anything at the target.
		if s.kindOf(candidate) == tarSimSymlink && s.resolvesToDir(candidate) {
			if resolved, ok := s.resolveLink(candidate); ok {
				built = s.followDirLinks(resolved)
				continue
			}
		}
		built = candidate
	}
	return joinDockerfileComponent(built, components[len(components)-1])
}

// mkdirAll mirrors os.MkdirAll: it reports false exactly when os.MkdirAll
// would fail, and otherwise records every directory it had to create.
func (s *tarExtractSim) mkdirAll(p string) bool {
	if p == "" || p == "." {
		return true
	}
	if s.kindOf(p) != tarSimNone {
		return s.resolvesToDir(p)
	}
	if !s.mkdirAll(path.Dir(p)) {
		return false
	}
	s.kinds[p] = tarSimDir
	return true
}

// applyDir mirrors extractTarball's tar.TypeDir case: one os.MkdirAll.
func (s *tarExtractSim) applyDir(name, key string) error {
	if !s.mkdirAll(key) {
		return fmt.Errorf("cannot create directory for entry %q: %q is not a directory extraction can create", name, key)
	}
	return nil
}

// applyReg mirrors extractTarball's tar.TypeReg case: os.MkdirAll on the
// parent, then os.Create on the path itself.
func (s *tarExtractSim) applyReg(name, key string) error {
	if !s.mkdirAll(path.Dir(key)) {
		return fmt.Errorf("cannot create parent directory for entry %q: %q is not a directory extraction can create", name, path.Dir(key))
	}
	if s.resolvesToDir(key) {
		return fmt.Errorf("entry %q is a regular file where the archive already puts a directory at %q", name, key)
	}
	if s.kindOf(key) == tarSimNone {
		s.kinds[key] = tarSimReg
	}
	return nil
}

// applySymlink mirrors extractTarball's tar.TypeSymlink case: one
// os.Symlink, with no MkdirAll of its own and no replacement of an
// existing path.
func (s *tarExtractSim) applySymlink(name, key, link string) error {
	if s.kindOf(key) != tarSimNone {
		return fmt.Errorf("entry %q is a symlink at %q, which the archive already declares", name, key)
	}
	if !s.resolvesToDir(path.Dir(key)) {
		return fmt.Errorf("entry %q is a symlink whose parent directory %q does not exist when extraction reaches it", name, path.Dir(key))
	}
	s.kinds[key] = tarSimSymlink
	s.links[key] = link
	return nil
}

// apply replays one header against the simulated tree, returning the
// refusal extraction itself would reach. An entry type extraction's switch
// does not handle (a hardlink, a device, a fifo) is skipped here too,
// because extraction skips it silently and so can never fail on it.
func (s *tarExtractSim) apply(name string, typeflag byte, linkname string) error {
	key := s.realPath(normalizeTarEntryName(name))
	switch typeflag {
	case tar.TypeDir:
		return s.applyDir(name, key)
	case tar.TypeReg:
		return s.applyReg(name, key)
	case tar.TypeSymlink:
		return s.applySymlink(name, key, linkname)
	}
	return nil
}
