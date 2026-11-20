package webhook

import (
	"archive/tar"
	"fmt"
	"testing"
)

// assertBoundariesAgree runs BOTH boundaries on the same bytes - the
// pre-claim preflight (checkTarballContainment then validateDockerfileTarget)
// and the post-claim build path (extractTarball into a temporary directory
// then splitTarballDockerfile) - and fails when one accepts what the other
// refuses. It is the same contract TestDockerfileTargetParity enforces per
// hand-written row; the generative tables below use it to cover whole cross
// products instead of named shapes, which is what keeps review findings
// 1452/1456/1457's defect CLASS closed rather than just its listed
// instances.
func assertBoundariesAgree(t *testing.T, entries []tarEntry, dockerfile string) {
	t.Helper()
	data := buildTarGz(t, entries)

	preflightErr := checkTarballContainment(data)
	if preflightErr == nil {
		preflightErr = validateDockerfileTarget(data, dockerfile)
	}

	dest := t.TempDir()
	buildErr := extractTarball(data, dest)
	if buildErr == nil {
		_, _, buildErr = splitTarballDockerfile(dest, dockerfile)
	}

	if (preflightErr == nil) != (buildErr == nil) {
		t.Fatalf("preflight and build disagree: preflight accept=%v (err=%v), build accept=%v (err=%v)",
			preflightErr == nil, preflightErr, buildErr == nil, buildErr)
	}
}

// simShape is one tar entry shape the generative tables below declare.
// "sym2dir", "sym2reg" and "sym2nil" exist because extraction's three
// syscalls each treat a symlink differently depending on what it resolves
// to, and a dangling link is a third case again.
type simShape struct {
	name     string
	typeflag byte
	linkname string
}

var simShapes = []simShape{
	{name: "dir", typeflag: tar.TypeDir},
	{name: "reg", typeflag: tar.TypeReg},
	{name: "sym2dir", typeflag: tar.TypeSymlink, linkname: "realdir"},
	{name: "sym2reg", typeflag: tar.TypeSymlink, linkname: "Dockerfile"},
	{name: "sym2nil", typeflag: tar.TypeSymlink, linkname: "nothing-here"},
}

// simBase is present in every generative archive: the configured
// dockerfile target, and a real directory for "sym2dir" to point at.
func simBase() []tarEntry {
	return []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "realdir", typeflag: tar.TypeDir},
	}
}

func simEntry(name string, s simShape) tarEntry {
	e := tarEntry{name: name, typeflag: s.typeflag, linkname: s.linkname}
	if s.typeflag == tar.TypeReg {
		e.body = "x"
	}
	return e
}

// TestTarExtractSimParity_SamePath covers the whole cross product of
// "what the archive already declares at a path" x "what it then declares
// at the SAME path". Review finding 1452 named one cell of this table,
// finding 1457's group A named three more, and the cycle-1 rework found
// two by hand; this covers all 25 at once so no further cell can be a
// repeat.
func TestTarExtractSimParity_SamePath(t *testing.T) {
	for _, first := range simShapes {
		for _, second := range simShapes {
			t.Run(fmt.Sprintf("%s_then_%s", first.name, second.name), func(t *testing.T) {
				entries := append(simBase(), simEntry("dup", first), simEntry("dup", second))
				assertBoundariesAgree(t, entries, "Dockerfile")
			})
		}
	}
}

// TestTarExtractSimParity_Ancestor covers the whole cross product of
// "what the archive declares at an ancestor path" x "what it then declares
// underneath it". Review finding 1452's instance 2 named one cell and
// finding 1457's instance 5 another; this covers all 15.
func TestTarExtractSimParity_Ancestor(t *testing.T) {
	childShapes := []simShape{
		{name: "dir", typeflag: tar.TypeDir},
		{name: "reg", typeflag: tar.TypeReg},
		{name: "sym", typeflag: tar.TypeSymlink, linkname: "../Dockerfile"},
	}
	for _, anc := range simShapes {
		for _, child := range childShapes {
			t.Run(fmt.Sprintf("ancestor_%s_child_%s", anc.name, child.name), func(t *testing.T) {
				entries := append(simBase(), simEntry("anc", anc), simEntry("anc/child", child))
				assertBoundariesAgree(t, entries, "Dockerfile")
			})
		}
	}
}

// TestTarExtractSimParity_Names covers entry-name and symlink-target
// shapes where the two boundaries used two different normalizations of
// the same header name (review finding 1456). The "a/../b" family is here
// to prove the fix is not over-broad: those names extract successfully,
// so refusing any name that merely CONTAINS a ".." component - which is
// what finding 1456 suggested - would refuse legitimate uploads.
func TestTarExtractSimParity_Names(t *testing.T) {
	names := []string{
		"a/../b",
		"./a/../b",
		"a/b/../../c",
		"a/../../escape",
		"/../escape.txt",
		"/../../escape.txt",
		"//../escape.txt",
		"/abs/Dockerfile",
		"..Containerfile",
		"a/..b/c",
	}
	for _, name := range names {
		t.Run("name="+name, func(t *testing.T) {
			entries := append(simBase(), tarEntry{name: name, typeflag: tar.TypeReg, body: "y"})
			assertBoundariesAgree(t, entries, "Dockerfile")
		})
	}

	links := []struct{ name, linkname string }{
		{"/a/link", "../../../etc/passwd"},
		{"a/link", "../Dockerfile"},
		{"a/b/link", "../../Dockerfile"},
		{"a/b/link", "../../../Dockerfile"},
		{"realdir/link", "../Dockerfile"},
		{"link", "realdir"},
	}
	for _, l := range links {
		t.Run("sym="+l.name+"->"+l.linkname, func(t *testing.T) {
			entries := append(simBase(), tarEntry{name: l.name, typeflag: tar.TypeSymlink, linkname: l.linkname})
			assertBoundariesAgree(t, entries, "Dockerfile")
		})
	}
}

// TestTarExtractSimParity_Chains covers forward references, implicitly
// created directories and symlink chains: the cases where a correct
// pre-claim answer depends on replaying the archive IN ORDER, because
// extraction itself is order-dependent.
func TestTarExtractSimParity_Chains(t *testing.T) {
	df := tarEntry{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"}
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"target declared after the link, then a directory at the link", []tarEntry{df,
			{name: "link", typeflag: tar.TypeSymlink, linkname: "later"},
			{name: "later", typeflag: tar.TypeDir},
			{name: "link", typeflag: tar.TypeDir}}},
		{"target declared after the link, then a child of the link", []tarEntry{df,
			{name: "link", typeflag: tar.TypeSymlink, linkname: "later"},
			{name: "later", typeflag: tar.TypeDir},
			{name: "link/child", typeflag: tar.TypeReg, body: "y"}}},
		{"target declared after the conflicting entry", []tarEntry{df,
			{name: "link", typeflag: tar.TypeSymlink, linkname: "later"},
			{name: "link", typeflag: tar.TypeDir},
			{name: "later", typeflag: tar.TypeDir}}},
		{"link to an implicitly created directory", []tarEntry{df,
			{name: "a/b/f", typeflag: tar.TypeReg, body: "y"},
			{name: "link", typeflag: tar.TypeSymlink, linkname: "a/b"},
			{name: "link/child", typeflag: tar.TypeReg, body: "z"}}},
		{"directory at a link to an implicitly created directory", []tarEntry{df,
			{name: "a/b/f", typeflag: tar.TypeReg, body: "y"},
			{name: "link", typeflag: tar.TypeSymlink, linkname: "a/b"},
			{name: "link", typeflag: tar.TypeDir}}},
		{"chain of links to a directory, child underneath", []tarEntry{df,
			{name: "realdir", typeflag: tar.TypeDir},
			{name: "l1", typeflag: tar.TypeSymlink, linkname: "realdir"},
			{name: "l2", typeflag: tar.TypeSymlink, linkname: "l1"},
			{name: "l2/child", typeflag: tar.TypeReg, body: "z"}}},
		{"chain of links to a directory, directory at the chain", []tarEntry{df,
			{name: "realdir", typeflag: tar.TypeDir},
			{name: "l1", typeflag: tar.TypeSymlink, linkname: "realdir"},
			{name: "l2", typeflag: tar.TypeSymlink, linkname: "l1"},
			{name: "l2", typeflag: tar.TypeDir}}},
		{"chain of links ending at a regular file, child underneath", []tarEntry{df,
			{name: "l1", typeflag: tar.TypeSymlink, linkname: "Dockerfile"},
			{name: "l2", typeflag: tar.TypeSymlink, linkname: "l1"},
			{name: "l2/child", typeflag: tar.TypeReg, body: "z"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertBoundariesAgree(t, c.entries, "Dockerfile")
		})
	}
}

// TestTarExtractSimParity_DockerfileValues covers the OTHER input both
// boundaries normalize independently: the configured dockerfile value
// itself. Review finding 1456's defect class is "the two boundaries use
// two different normalizations of the same input", so this axis is
// checked too rather than assumed; it is in full agreement today, and
// this test is what keeps it that way.
func TestTarExtractSimParity_DockerfileValues(t *testing.T) {
	entries := []tarEntry{
		{name: "Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "sub/Dockerfile", typeflag: tar.TypeReg, body: "FROM alpine"},
		{name: "realdir", typeflag: tar.TypeDir},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "Dockerfile"},
	}
	values := []string{
		"", "   ", ".", "..", "Dockerfile", "./Dockerfile", "Dockerfile/",
		"sub/../Dockerfile", "sub/Dockerfile", "a/../../Dockerfile",
		"/Dockerfile", "//Dockerfile", "realdir", "link", "link/",
		"sub/", "nonexistent", "sub/./Dockerfile", "./sub/../Dockerfile",
	}
	for _, value := range values {
		t.Run("value="+value, func(t *testing.T) {
			assertBoundariesAgree(t, entries, value)
		})
	}
}
