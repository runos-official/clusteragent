# Changelog

All notable changes to the RunOS Cluster Agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v1.1.0

Datastore moves to the cluster's system PostgreSQL; the agent is now stateless.

- Build jobs, logs, one-shot job records, the SQL schema cache, and single-use
  upload/pull tokens now persist in the RunOS control plane's system PostgreSQL
  instead of a local SQLite file. The agent discovers that database via a
  control-plane-maintained `runos-system-db` ConfigMap, self-provisions a `runos`
  database and role (storing the generated password in a Secret), and migrates
  its `cluster_agent_`-prefixed schema automatically.
- Self-healing connection: the datastore is reconciled in the background, so the
  agent never crashes if PostgreSQL is briefly unavailable, retries indefinitely,
  and reconnects and re-provisions automatically if the system database is moved
  to a different instance.
- Upload/pull tokens are now hashed at rest (SHA-256); the raw token is never
  stored.
- The agent is stateless: the `/data` PersistentVolume is gone.
- The binary is now built CGO-free with pure-Go drivers, so the multiarch image
  cross-compiles natively (no QEMU) and release builds are substantially faster.

## v1.0.0

First public release of the RunOS cluster agent.

- Source-available under the Elastic License 2.0.
- Published as a multiarch (`linux/amd64` + `linux/arm64`) container image to
  `ghcr.io/runos-official/clusteragent`, built by GitHub Actions on a `v*` tag
  with a keyless Sigstore build-provenance attestation. The rendered Kubernetes
  deploy manifest and a `checksums.txt` ship as release assets.
- Pre-release tags (`-rc.N`) publish a hidden release candidate: pushed and
  pinnable by exact version, never tagged `:latest`, and excluded from the
  "Latest release" pointer, so normal consumers keep getting the latest stable.
- Verify a release image with:
  `gh attestation verify oci://ghcr.io/runos-official/clusteragent:1.0.0 --repo runos-official/clusteragent`.

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
