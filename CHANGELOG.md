# Changelog

All notable changes to the RunOS Cluster Agent are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The release pipeline extracts the section matching the pushed tag (`## vX.Y.Z`)
as the GitHub release notes, so every released version needs a section here.

## v1.2.0

### Added
- **Build cache on network storage.** Each build pod's scratch space
  (`/var/lib/buildkit`) can now come from a LINSTOR volume instead of the
  node's disk. Two new `build-settings` ConfigMap keys drive it:
  `buildCacheStorageClass` (empty = the node's disk, the default) and
  `buildCacheSizeGb` (default 50). Conductor sends the resolved StorageClass
  name rather than a mode flag, so there is no way to be in "distributed" mode
  with nothing to ask for; a blank or whitespace-only value falls back to the
  node's disk.

  The distributed shape is a GENERIC EPHEMERAL VOLUME, not a managed PVC:
  Kubernetes creates the claim with the pod and deletes it with the pod, so
  there is no volume pool to lease and no orphan to reclaim. Cache reuse is
  unaffected either way, because layer cache comes from the Harbor registry
  cache import/export, never from this directory.

### Changed
- **The build pod's `emptyDir` is now capped** at `buildCacheSizeGb` (default
  50GB). This is a behaviour change on existing clusters. The volume was
  previously unbounded and carried no `ephemeral-storage` request, so one
  large build could fill a node's root filesystem and take down every workload
  on that node. A build that exceeds the cap now gets its pod evicted, failing
  that one build instead.

### Added (build pipeline, no runtime effect)
- **Leak gate on this public repo.** `scripts/leakcheck.py` blocks internal
  identifiers and credentials from reaching a release. Credential shapes hard-fail
  and can never be baselined; internal identifiers (lab machine names, account
  ids, address literals outside the documentation ranges) ratchet against
  `scripts/leakcheck.baseline`, so an existing finding passes and a new one fails.
  Wired into a pre-commit hook (staged diff), `make leakcheck` (whole tree), a CI
  workflow, and `scripts/release.sh`, where it cannot be skipped.

### Fixed (build pipeline, no runtime effect)
- The release script's own sensitivity floor carried a pre-1.0.1 regex and flagged
  leakcheck's documentation comment, refusing every release in this repo. The floor
  now applies the same value-shape filter as `leakcheck.py`.
- The leak gate's own test fixtures assembled real lab addresses and a real cluster
  id at run time, so the checker could not see them and the file that proves the
  gate works was the one place still leaking. Replaced with stand-ins outside every
  allow-listed range, so the catch assertions still fail if the gate breaks.


## v1.1.6

Finalizes v1.1.6-rc.1 + v1.1.6-rc.2 (dev-verified via cluster pin on tc5-gan).
Ships the read-only cluster AI assistant's in-cluster execution path.

### Added
- **`RUN_READONLY_KUBECTL` instruction** for the conductor cluster assistant.
  Runs an arbitrary kubectl argv inside a dedicated `runos-assistant-reader-exec`
  pod whose ServiceAccount is a read-only identity (`runos-assistant-reader`:
  get/list/watch, no Secrets, no write verbs), so Kubernetes RBAC is the sole
  guardrail and the LLM's argv needs no allow-listing. That pod holds no admin
  token, so a local-file read cannot escalate and a write is API-server denied.
  The agent lazily creates the SA + ClusterRole + kubeconfig ConfigMap + pod on
  first use, execs `["kubectl", ...args]` via the k8s exec API, and idle-reaps
  the shared pod after 15 minutes. Image `alpine/k8s:1.34.1` by default,
  overridable via `ASSISTANT_KUBECTL_IMAGE`.
- **Reader RBAC covers the full platform read surface** the assistant needs
  (previously 403 → the assistant reported resources as absent): CNPG backups
  (`barmancloud.cnpg.io`), GPU (`nvidia.com`), node feature discovery
  (`nfd.k8s-sigs.io`), volume snapshots (`snapshot.storage.k8s.io`), and the
  kubelet-served node subresources `nodes/metrics`, `nodes/stats`, `nodes/proxy`
  (get only). Still get/list/watch only, still no Secrets.

### Fixed
- **Reader RBAC now reconciles instead of sticking at first-write.** The
  ClusterRole rules and kubeconfig ConfigMap reconcile once per agent process,
  independent of whether the reader pod already exists, so a newer agent's rule
  set takes effect on upgrade instead of requiring a manual ClusterRole delete.
  The ClusterRole updates only on actual drift.

## v1.1.6-rc.2

Follow-up candidate to v1.1.6-rc.1 from the assistant's adversarial review:
makes the reader RBAC upgradeable and widens it to the operator groups the
assistant actually needs. Hidden prerelease for targeted dev verification
(cluster pin), not advertised.

### Fixed
- **Reader RBAC now reconciles instead of sticking at first-write.** The
  ClusterRole rules and the kubeconfig ConfigMap were create-if-missing, and the
  ensure ran only when the reader pod was absent, so a newer agent's rule set
  could never take effect on an existing cluster (it needed a manual ClusterRole
  delete). Both now reconcile, once per agent process, independent of pod
  presence, so an upgrade lands the new rules while a prior version's shared pod
  is still running. The ClusterRole updates only on actual drift, so a restart
  with unchanged rules writes nothing.

### Added
- **Reader RBAC covers the remaining platform read surface**, which previously
  returned 403 and made the assistant report resources as absent: CNPG backups
  (`barmancloud.cnpg.io`), GPU (`nvidia.com`), node feature discovery
  (`nfd.k8s-sigs.io`), volume snapshots (`snapshot.storage.k8s.io`), and the
  kubelet-served node subresources `nodes/metrics`, `nodes/stats`, `nodes/proxy`
  (get only; exec/attach need `create`, which stays ungranted). Still
  get/list/watch only, and still no Secrets.

## v1.1.6-rc.1

Candidate for the read-only cluster AI assistant (Phase 2). Hidden prerelease for
targeted dev verification (cluster pin), not advertised.

### Added
- **`RUN_READONLY_KUBECTL` instruction.** Runs an arbitrary kubectl argv on
  behalf of the conductor's cluster assistant and returns stdout/stderr/exit
  code. The argv is deliberately NOT allow-listed: it executes inside a
  dedicated `runos-assistant-reader-exec` pod whose ServiceAccount is a
  read-only identity (`runos-assistant-reader`: get/list/watch, no Secrets, no
  write verbs), so Kubernetes RBAC is the sole guardrail. That pod holds no
  admin token, so a local-file read cannot escalate and a write is denied by the
  API server. The cluster agent lazily creates the SA + ClusterRole + kubeconfig
  ConfigMap + pod on first use, execs `["kubectl", ...args]` into it via the k8s
  exec API, and idle-reaps the shared pod after 15 minutes. The kubectl image is
  `alpine/k8s:1.34.1` by default, overridable via `ASSISTANT_KUBECTL_IMAGE`.

## v1.1.5

Finalizes v1.1.5-rc.1 (VCS/CI deploy env-file resolution, dev-verified via
cluster pin) and raises the exec-sql read cap.

### Fixed
- **VCS/CI deploys resolve committed `env:` / `secretEnv:` file paths against
  the manifest's own directory, not the repo clone-root** (from v1.1.5-rc.1).
  Paths are anchored at the config yaml's directory, traversal outside the
  clone is rejected, and a committed-but-missing `env:` file fails the fetch
  loudly instead of shipping empty env (which silently dropped keys like a
  source-IP allowlist). A gitignored `secretEnv:` file absent on the checkout
  stays tolerated (secrets come from server state).

### Changed
- **VCS source-fetch carries the resolved env contract the conductor consumes**
  (from v1.1.5-rc.1): `resolvedEnvVars` / `resolvedSecretEnvVars` with explicit
  three-state present/absent semantics, dotenv-parsed in lockstep with the CLI
  parser.
- **exec-sql SELECT row cap raised from 10 to 100** (postgres and mysql). 10
  rows made read-only introspection awkward; 100 matches the ClickHouse
  exec-sql cap. Truncation behavior and the `truncated` flag are unchanged.

## v1.1.5-rc.1

Candidate for VCS/CI deploy env-file resolution. Hidden prerelease for targeted
dev verification (cluster pin), not advertised.

### Fixed
- **VCS/CI deploys resolve committed `env:` / `secretEnv:` file paths against
  the manifest's own directory, not the repo clone-root.** A monorepo app whose
  config yaml lives in a subdirectory had its referenced env files looked up at
  the clone-root, found nothing, and deployed with EMPTY env. Security teeth: an
  empty env drops keys like the source-IP allowlist (`ALLOWED_CIDRS`), silently
  disabling an in-app control with no error. Paths are now anchored at the config
  yaml's directory, traversal outside the clone is rejected, and a
  committed-but-missing `env:` file fails the fetch loudly instead of shipping
  empty. A gitignored `secretEnv:` file that is absent on the checkout is
  expected and tolerated (secrets come from server state).

### Changed
- **VCS source-fetch now carries the resolved env contract the conductor
  consumes**, mirroring a CLI deploy. The response ships `resolvedEnvVars` /
  `resolvedSecretEnvVars` with explicit three-state present/absent semantics:
  field omitted (no `env:`/`secretEnv:` key) -> conductor preserves live
  ConfigMap/Secret; field present, including empty `{}` -> conductor applies
  (full replace, an empty committed file legitimately clears it). The cluster
  agent holds the checkout and dotenv-parses the files (the conductor has no
  parser), with the parser kept byte-for-byte in lockstep with the CLI's so a
  committed `.config.env` is interpreted identically on both deploy paths.

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
