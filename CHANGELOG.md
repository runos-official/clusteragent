# Changelog

All notable changes to the RunOS Cluster Agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v1.1.4

### Fixed
- **A panic in any instruction handler can no longer crash the agent pod.** The
  inbound-instruction dispatch (`go handleInstruction`) had no recover boundary
  anywhere in the binary, so a single handler panic (a control-plane payload
  parse, a client-go / SQL / serialization edge, or any future handler) would
  unwind the goroutine and CrashLoopBackOff the whole per-cluster control surface
  (uploads, webhooks, builds, SQL, Harbor). Dispatch now goes through
  `safeHandleInstruction`, which recovers, logs the value + stack, replies with an
  error for the instruction's tag (so the caller is not left hanging), and lets
  the stream keep serving. Mirrors the node agent's existing guard.

## v1.1.3

Security hardening (audit follow-ups), with regression tests.

### Security
- **SSRF guard on the web-request handlers.** `WEB_REQUEST` and
  `WEB_REQUEST_FOLLOW` now refuse to connect to loopback, link-local, or cloud
  instance-metadata (`169.254.169.254`) addresses, and pin the dial to the
  validated IP so DNS cannot rebind to a blocked address between the check and
  the connection. The check lives in the dialer, so it also covers every redirect
  hop (a vetted URL that 3xx-redirects to the metadata IP is blocked). In-cluster
  private (RFC1918) targets stay allowed and `allowInsecure` still controls TLS
  verification only. Mirrors the node agent's guard. Closes the path by which a
  single inbound instruction could exfiltrate cloud IAM credentials.
- **Read-only SQL connections hard-block writes.** With `readWrite=false`, a
  non-read statement (including comment-/whitespace-prefixed writes, `SET`, and
  DDL) is refused before execution rather than routed to the write path. This is
  the authoritative gate for MySQL (whose `SET SESSION READ ONLY` does not block
  autocommit DML) and defense-in-depth for Postgres.

### Fixed
- **`PullArchive` size cap.** Streaming a CLI-archive layer out of Harbor is now
  bounded to the layer's advertised size (a descriptor that streams more than it
  claims is rejected) and to a 1 GiB hard ceiling, so a compromised or corrupt
  registry layer cannot fill disk/memory unbounded.

## v1.1.2

Reliability + robustness pass (from an audit), plus regression tests pinning the
agent's defensive logic.

### Fixed
- **Bootstrap no longer crashes the pod on a transient error during cluster
  creation.** The startup chain (k8s client, runos-config ConfigMap, TLS secret,
  credential generation, initial connect) was a series of `log.Fatalf`, so any
  transient hiccup at the most fragile moment (API server warming up, a secret not
  yet propagated by the installer, Nodeward briefly unreachable, DNS not ready)
  turned into CrashLoopBackOff with a raw Go fatal. It now retries transients with
  per-step timeouts and throttled log lines; only a malformed cert already at rest
  is fatal (with a `kubectl delete secret` remediation hint).
- **Reconnect is now indefinite** with capped exponential backoff (was a hard exit
  after 10 attempts, which required a pod restart for any control-plane outage
  longer than ~10 minutes). Disconnection is surfaced via the health endpoint
  instead of exiting.
- **The upload + liveness webhook servers can no longer kill the agent** — they log
  and retry their bind on failure instead of `log.Fatalf`, so the :8081 upload
  server can't sever the gRPC control link.
- **`WEB_REQUEST_FOLLOW`** no longer panics on a malformed redirect/login URL
  (unchecked `http.NewRequest` error) and returns the real final HTTP status (was
  hardcoded `"200 OK"`).
- **Context-bounded** the git clone/fetch shell-outs and several previously
  unbounded k8s/SQL calls (secret writes, pod listing with a server-side cap, job
  delete, schema introspection) so a hung remote/API can't wedge a handler.

### Tests
- Pin the retryable-vs-fatal bootstrap classification + the backoff schedule, the
  web-request nil-guard + real-status, the SQL read/write classification incl. the
  comment/whitespace/SET/CTE bypass cases, the VCS path-traversal guard (incl.
  sibling-prefix escape), and BuildKit credential redaction.

## v1.1.1

- Fix: datastore tables are now correctly prefixed `cluster_agent_` in the shared
  `runos` database. The GORM models' explicit `TableName()` returned unprefixed
  names, which overrides the `NamingStrategy` table prefix, so migrations created
  bare tables (e.g. `buildkit_jobs`). `TableName()` now returns the full prefixed
  name (`cluster_agent_buildkit_jobs`, ...), with a regression test over the
  migrated schema. No data migration: the agent re-provisions the prefixed tables
  on the system Postgres; any bare tables from v1.1.0 are orphaned and can be
  dropped.

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
