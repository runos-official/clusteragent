// Package version exposes the cluster agent's build version.
//
// Version defaults to "dev" for local builds and is overwritten at release
// time via the linker:
//
//	-ldflags "-X github.com/runos-official/clusteragent/version.Version=<v>"
//
// The release pipeline (.github/workflows/release.yml) derives <v> from the
// pushed git tag, so a released image always reports the tag it was built from.
package version

// Version is the cluster agent's semantic version, injected at build time.
var Version = "dev"
