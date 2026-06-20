package instructions

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// vcsWorkdirCache holds the workdir paths populated by VCS_FETCH_SOURCE so
// that VCS_BUILD can find them later. Keys are "<osid>:<sha>" strings;
// values are the absolute path on disk plus the time the entry was written.
//
// A 30-minute TTL janitor sweeps abandoned entries every 5 minutes. That's
// well above any plausible build duration and short enough that orphaned
// workdirs from interrupted deploys don't accumulate forever.
//
// In-memory only: a cluster-agent restart loses the cache, which means an
// in-flight VCS_BUILD that was about to start would fail to find its workdir.
// Conductor's orchestration sees that as a build failure and surfaces it
// like any other failure. Acceptable for v1; if needed later, persist via
// the datastore.

type vcsWorkdirEntry struct {
	path      string
	createdAt time.Time
	// Build paths resolved by VCS_FETCH_SOURCE from the committed runos.yaml.
	// Cached here so VCS_BUILD knows where to point BuildKit's `context=` /
	// `dockerfile=` / `filename=` flags without re-parsing the yaml. Empty
	// strings on a v0 entry mean "use the workdir + Dockerfile defaults" so
	// the legacy single-app shape still works without touching anything.
	contextPath        string
	dockerfileDir      string
	dockerfileFilename string
}

const (
	vcsWorkdirTTL        = 30 * time.Minute
	vcsWorkdirSweepEvery = 5 * time.Minute
)

var vcsWorkdirCache sync.Map

// vcsWorkdirJanitorStarted ensures the background janitor goroutine is
// launched exactly once across all imports, even though the cache itself is
// usable before it starts.
var vcsWorkdirJanitorStarted sync.Once

// startVcsWorkdirJanitor launches the TTL sweeper. Idempotent; safe to call
// from multiple init paths.
func startVcsWorkdirJanitor() {
	vcsWorkdirJanitorStarted.Do(func() {
		go func() {
			ticker := time.NewTicker(vcsWorkdirSweepEvery)
			defer ticker.Stop()
			for range ticker.C {
				sweepVcsWorkdirCache()
			}
		}()
	})
}

func vcsWorkdirCacheKey(osid, sha string) string {
	return osid + ":" + sha
}

// VcsWorkdirBuildPaths is what VCS_BUILD needs from the cache to wire
// BuildKit correctly: the workdir on disk plus the resolved build context
// and Dockerfile location. All paths are absolute. dockerfileDir empty
// means "same as contextPath"; dockerfileFilename empty means "Dockerfile".
type VcsWorkdirBuildPaths struct {
	Workdir            string
	ContextPath        string
	DockerfileDir      string
	DockerfileFilename string
}

// vcsWorkdirCachePut records the workdir path and resolved build paths for a
// {osid, sha} pair. Overwrites any existing entry — re-running
// VCS_FETCH_SOURCE for the same pair (e.g. retry after a crash) leaves the
// most recent entry pointing at the most recent workdir.
func vcsWorkdirCachePut(osid, sha string, paths VcsWorkdirBuildPaths) {
	startVcsWorkdirJanitor()
	vcsWorkdirCache.Store(vcsWorkdirCacheKey(osid, sha), vcsWorkdirEntry{
		path:               paths.Workdir,
		createdAt:          time.Now(),
		contextPath:        paths.ContextPath,
		dockerfileDir:      paths.DockerfileDir,
		dockerfileFilename: paths.DockerfileFilename,
	})
}

// vcsWorkdirCacheGet returns the cached build paths for a {osid, sha} pair,
// or ok=false if the entry is missing or expired.
func vcsWorkdirCacheGet(osid, sha string) (VcsWorkdirBuildPaths, bool) {
	raw, ok := vcsWorkdirCache.Load(vcsWorkdirCacheKey(osid, sha))
	if !ok {
		return VcsWorkdirBuildPaths{}, false
	}
	entry, ok := raw.(vcsWorkdirEntry)
	if !ok {
		return VcsWorkdirBuildPaths{}, false
	}
	if time.Since(entry.createdAt) > vcsWorkdirTTL {
		return VcsWorkdirBuildPaths{}, false
	}
	return VcsWorkdirBuildPaths{
		Workdir:            entry.path,
		ContextPath:        entry.contextPath,
		DockerfileDir:      entry.dockerfileDir,
		DockerfileFilename: entry.dockerfileFilename,
	}, true
}

// vcsWorkdirCacheDeleteIfPath clears the cache entry for {osid, sha} only
// when it still points at expectedPath. Used by VCS_BUILD cleanup so a
// concurrent VCS_FETCH_SOURCE that landed a fresher entry between fetch
// and build isn't clobbered. The caller is responsible for removing the
// disk path itself; this function only touches the cache.
func vcsWorkdirCacheDeleteIfPath(osid, sha, expectedPath string) {
	key := vcsWorkdirCacheKey(osid, sha)
	raw, ok := vcsWorkdirCache.Load(key)
	if !ok {
		return
	}
	entry, ok := raw.(vcsWorkdirEntry)
	if !ok {
		return
	}
	if entry.path != expectedPath {
		return
	}
	vcsWorkdirCache.CompareAndDelete(key, entry)
}

// sweepVcsWorkdirCache walks the cache once and evicts any entry older than
// the TTL. Run periodically by the janitor; safe to invoke directly from
// tests.
func sweepVcsWorkdirCache() {
	now := time.Now()
	vcsWorkdirCache.Range(func(key, value any) bool {
		entry, ok := value.(vcsWorkdirEntry)
		if !ok {
			vcsWorkdirCache.Delete(key)
			return true
		}
		if now.Sub(entry.createdAt) <= vcsWorkdirTTL {
			return true
		}
		vcsWorkdirCache.Delete(key)
		if err := os.RemoveAll(entry.path); err != nil {
			log.Printf("vcs workdir cache: janitor failed to remove %s: %v", entry.path, err)
		}
		return true
	})
}

// vcsWorkdirPathFor returns a freshly-unique on-disk path for a {osid,
// sha} pair. Each call yields a different path: the {osid, sha} prefix
// stays human-recognisable in `ls /tmp` and in logs, but a random hex
// suffix scopes every fetch attempt to its own scratch dir. Concurrent
// deploys of the same SHA (e.g. a workflow_dispatch racing a push) and
// retries that follow a partial or aborted prior attempt therefore never
// share a directory and cannot collide on clone-in-progress state.
func vcsWorkdirPathFor(osid, sha string) string {
	shortSha := sha
	if len(sha) > 7 {
		shortSha = sha[:7]
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// crypto/rand only fails if the OS RNG is broken; fall back to a
		// timestamp-derived suffix so we still produce a unique-ish path
		// rather than crashing the whole instruction handler.
		log.Printf("vcs workdir cache: crypto/rand failed (%v); using timestamp suffix", err)
		ns := time.Now().UnixNano()
		for i := range 4 {
			rnd[i] = byte(ns >> (8 * i))
		}
	}
	name := osid + "-" + shortSha + "-" + hex.EncodeToString(rnd[:])
	return filepath.Join(os.TempDir(), "runos-vcs-build-"+name)
}
