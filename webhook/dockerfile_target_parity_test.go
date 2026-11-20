package webhook

import (
	"archive/tar"
	"fmt"
	"testing"

	"github.com/runos-official/clusteragent/agentstream"
)

// dockerfileParityCase is one row of review finding 1311's mandated parity
// table: a tarball shape plus a dockerfile value, with the outcome BOTH
// boundaries must reach. The preflight side folds checkTarballContainment
// and validateDockerfileTarget together, matching how HandleCLIDeployUpload
// actually runs them before any claim; the build side extracts for real and
// then resolves the target through splitTarballDockerfile, matching what
// the asynchronous build does after a claim.
type dockerfileParityCase struct {
	name       string
	entries    []tarEntry
	dockerfile string
	wantAccept bool
}

// symlinkChain returns `hops` symlink entries sym1 -> sym2 -> ... ->
// sym<hops> -> target.txt (a regular file), for review finding 1311's C5:
// a chain longer than maxDockerfileSymlinkHops must refuse on both
// boundaries now that they share one hop budget.
func symlinkChain(hops int) []tarEntry {
	entries := []tarEntry{{name: "target.txt", typeflag: tar.TypeReg, body: "FROM alpine"}}
	for i := hops; i >= 1; i-- {
		target := "target.txt"
		if i < hops {
			target = fmt.Sprintf("sym%d", i+1)
		}
		entries = append(entries, tarEntry{name: fmt.Sprintf("sym%d", i), typeflag: tar.TypeSymlink, linkname: target})
	}
	return entries
}

// TestDockerfileTargetParity is THE closure test for review findings 1311
// and 1315: for every row, the pre-claim preflight (checkTarballContainment
// + validateDockerfileTarget) and the post-claim build path (extractTarball
// + splitTarballDockerfile) must reach the SAME accept/refuse outcome. A row
// where they disagree fails the test outright, before the wantAccept
// comparison even runs.
func TestDockerfileTargetParity(t *testing.T) {
	cases := []dockerfileParityCase{
		{
			name:       "plain regular target",
			entries:    []tarEntry{{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"}},
			dockerfile: "Dockerfile",
			wantAccept: true,
		},
		{
			name: "contained symlink as the final component",
			entries: []tarEntry{
				{name: "target.txt", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "link", typeflag: tar.TypeSymlink, linkname: "target.txt"},
			},
			dockerfile: "link",
			wantAccept: true,
		},
		{
			name:       "basename beginning with two dots",
			entries:    []tarEntry{{name: "..Containerfile", typeflag: tar.TypeReg, body: "FROM alpine"}},
			dockerfile: "..Containerfile",
			wantAccept: true,
		},
		{
			name:       "absolute symlink as the target",
			entries:    []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			dockerfile: "link",
			wantAccept: false,
		},
		{
			name:       "target that escapes the root",
			entries:    []tarEntry{{name: "link", typeflag: tar.TypeSymlink, linkname: "../outside/secret"}},
			dockerfile: "link",
			wantAccept: false,
		},
		{
			// C1: resolveDockerfileTarget (and fsDockerfileLookup) now
			// resolve a symlink at ANY path component, not only the final
			// one: "src" -> "packages/app" is a directory alias, and
			// "src/Dockerfile" must resolve through it exactly like the
			// real, already-extracted filesystem does.
			name: "C1 symlinked directory component",
			entries: []tarEntry{
				{name: "packages/app/Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "src", typeflag: tar.TypeSymlink, linkname: "packages/app"},
			},
			dockerfile: "src/Dockerfile",
			wantAccept: true,
		},
		{
			name: "C2 traversing entry name elsewhere, configured dockerfile",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
			},
			dockerfile: "Dockerfile",
			wantAccept: false,
		},
		{
			name: "C2 traversing entry name elsewhere, empty default",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
			},
			dockerfile: "",
			wantAccept: false,
		},
		{
			name: "C3 absolute symlink elsewhere, configured dockerfile",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			},
			dockerfile: "Dockerfile",
			wantAccept: false,
		},
		{
			name: "C3 absolute symlink elsewhere, empty default",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			},
			dockerfile: "",
			wantAccept: false,
		},
		{
			name: "C4 escaping symlink elsewhere, configured dockerfile",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "evil", typeflag: tar.TypeSymlink, linkname: "../outside/secret"},
			},
			dockerfile: "Dockerfile",
			wantAccept: false,
		},
		{
			name: "C4 escaping symlink elsewhere, empty default",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "evil", typeflag: tar.TypeSymlink, linkname: "../outside/secret"},
			},
			dockerfile: "",
			wantAccept: false,
		},
		{
			// C5: 11 symlink hops, one over maxDockerfileSymlinkHops (10).
			name:       "C5 chain longer than the hop limit",
			entries:    symlinkChain(11),
			dockerfile: "sym1",
			wantAccept: false,
		},
		{
			// C6: the link's own target string ends in "/", which POSIX
			// requires to land on a directory; it names a regular file.
			name: "C6 trailing slash link target to a regular file",
			entries: []tarEntry{
				{name: "target.txt", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "link", typeflag: tar.TypeSymlink, linkname: "target.txt/"},
			},
			dockerfile: "link",
			wantAccept: false,
		},
		{
			// C7: the archive declares "target" as a directory and then,
			// later, as a regular file - extraction's MkdirAll followed by
			// os.Create on the same real path fails outright.
			name: "C7 directory then regular file at the same path",
			entries: []tarEntry{
				{name: "target", typeflag: tar.TypeDir},
				{name: "target", typeflag: tar.TypeReg, body: "FROM alpine"},
			},
			dockerfile: "target",
			wantAccept: false,
		},
		{
			// Finding 1451 instance 1 (regression against f39f58c): the
			// dockerfile's ANCESTOR component is a symlink to a directory.
			// extraction writes "link/Dockerfile" THROUGH the symlink, so
			// the real file lands at "real/Dockerfile"; the tar-based
			// lookup must register the entry under that resolved path too.
			name: "finding 1451(1) symlinked directory ancestor",
			entries: []tarEntry{
				{name: "real", typeflag: tar.TypeDir},
				{name: "link", typeflag: tar.TypeSymlink, linkname: "real"},
				{name: "link/Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
			},
			dockerfile: "link/Dockerfile",
			wantAccept: true,
		},
		{
			// Finding 1451 instance 2: an absolute tar entry name.
			// extraction's filepath.Join(destDir, header.Name) absorbs the
			// leading slash instead of treating it as a jump to the real
			// filesystem root, so the entry lands at destDir/abs/Dockerfile.
			name:       "finding 1451(2) absolute tar entry name",
			entries:    []tarEntry{{name: "/abs/Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"}},
			dockerfile: "abs/Dockerfile",
			wantAccept: true,
		},
		{
			// Finding 1452 instance 1: C7's shape declared AWAY from the
			// configured dockerfile target, so the resolver's own walk
			// never reaches it; must still refuse archive-wide.
			name: "finding 1452(1) directory then regular file elsewhere",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "boom", typeflag: tar.TypeDir},
				{name: "boom", typeflag: tar.TypeReg, body: "x"},
			},
			dockerfile: "Dockerfile",
			wantAccept: false,
		},
		{
			// Finding 1452 instance 2: a regular file with an entry
			// declared underneath it - extraction's os.MkdirAll on the
			// parent fails outright ("not a directory").
			name: "finding 1452(2) regular file with an entry declared underneath it",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "a", typeflag: tar.TypeReg, body: "x"},
				{name: "a/b", typeflag: tar.TypeReg, body: "y"},
			},
			dockerfile: "Dockerfile",
			wantAccept: false,
		},
		{
			// Finding 1452's must-still-pass repeat: regular over regular
			// at the same path extracts fine (os.Create just overwrites).
			name: "finding 1452 repeat: regular over regular must still pass",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "dup", typeflag: tar.TypeReg, body: "first"},
				{name: "dup", typeflag: tar.TypeReg, body: "second"},
			},
			dockerfile: "Dockerfile",
			wantAccept: true,
		},
		{
			// Finding 1452's must-still-pass repeat: directory over
			// directory at the same path extracts fine (os.MkdirAll is
			// idempotent).
			name: "finding 1452 repeat: directory over directory must still pass",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "dup", typeflag: tar.TypeDir},
				{name: "dup", typeflag: tar.TypeDir},
			},
			dockerfile: "Dockerfile",
			wantAccept: true,
		},
		{
			// Finding 1452's must-still-pass repeat: a symlink declared
			// and then a regular entry at the SAME path (os.Create follows
			// the existing symlink and truncates its target - no error;
			// the reverse order, regular then symlink, does fail
			// extraction with EEXIST, but that order is not in the
			// mandated must-pass list and is left as ordinary last-one-
			// wins, unaffected by this story's new conflict rule either
			// way since it never touches dockerfileEntryDir).
			name: "finding 1452 repeat: symlink over regular must still pass",
			entries: []tarEntry{
				{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
				{name: "dup", typeflag: tar.TypeSymlink, linkname: "Dockerfile"},
				{name: "dup", typeflag: tar.TypeReg, body: "first"},
			},
			dockerfile: "Dockerfile",
			wantAccept: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildTarGz(t, tc.entries)

			preflightErr := checkTarballContainment(data)
			if preflightErr == nil {
				preflightErr = validateDockerfileTarget(data, tc.dockerfile)
			}

			dest := t.TempDir()
			buildErr := extractTarball(data, dest)
			if buildErr == nil {
				_, _, buildErr = splitTarballDockerfile(dest, tc.dockerfile)
			}

			preflightOK := preflightErr == nil
			buildOK := buildErr == nil
			if preflightOK != buildOK {
				t.Fatalf("preflight and build disagree: preflight accept=%v (err=%v), build accept=%v (err=%v)",
					preflightOK, preflightErr, buildOK, buildErr)
			}
			if preflightOK != tc.wantAccept {
				t.Fatalf("accept=%v, want %v (preflightErr=%v, buildErr=%v)", preflightOK, tc.wantAccept, preflightErr, buildErr)
			}
		})
	}
}

// TestHandleCLIDeployUpload_DockerfileTargetRefusalIsPreClaim pins review
// finding 1311's refused shape class "target": a configured dockerfile the
// preflight resolver refuses must leave the token unclaimed, push nothing,
// and start no build - matching every other pre-claim refusal in this
// handler.
func TestHandleCLIDeployUpload_DockerfileTargetRefusalIsPreClaim(t *testing.T) {
	token := legacyUploadToken()
	token.Dockerfile = "apps/api/Dockerfile.missing"
	h := &uploadHarness{t: t, token: token, buildkitConfig: &agentstream.BuildKitConfig{}}
	h.install()

	data := buildTarGz(t, []tarEntry{{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"}})
	w := doUpload(t, data)

	h.assertTerminalRefusal(t)
	if w.Code == 202 {
		t.Fatalf("status = %d, want a refusal status, not 202", w.Code)
	}
}

// TestHandleCLIDeployUpload_ContainmentRefusalIsPreClaim pins review
// finding 1311's refused shape class "containment": an archive-wide
// containment violation (here, a traversing entry name) must refuse before
// claim too, even though the configured dockerfile itself is perfectly
// valid.
func TestHandleCLIDeployUpload_ContainmentRefusalIsPreClaim(t *testing.T) {
	token := legacyUploadToken()
	token.Dockerfile = "Dockerfile"
	h := &uploadHarness{t: t, token: token, buildkitConfig: &agentstream.BuildKitConfig{}}
	h.install()

	data := buildTarGz(t, []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "../escape.txt", typeflag: tar.TypeReg, body: "pwned"},
	})
	w := doUpload(t, data)

	h.assertTerminalRefusal(t)
	if w.Code == 202 {
		t.Fatalf("status = %d, want a refusal status, not 202", w.Code)
	}
}
