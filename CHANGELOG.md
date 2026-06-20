# Changelog

All notable changes to the RunOS Cluster Agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v0.17.0

Baseline of the public release pipeline.

- Build and version are now driven by the git tag: `version.Version` is injected
  at build time via `-ldflags`, and the multiarch image is published to
  `ghcr.io/runos-official/clusteragent` by GitHub Actions on a `v*` tag push.
- Releases carry a keyless Sigstore build-provenance attestation bound to the
  image digest, plus the rendered deploy manifest and its checksum.
- Pre-release tags (`-rc.N`) publish a hidden release candidate: the image is
  pushed and pinnable by exact version, but is never tagged `:latest` and is
  flagged a GitHub prerelease, so normal consumers keep getting the latest
  stable while testers opt in by pinning the exact version.
