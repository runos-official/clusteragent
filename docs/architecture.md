# Architecture

This document is the in-depth companion to the [README](../README.md). It
describes how the cluster agent is put together: the control-plane link, the
package layout, and the full set of instructions it handles.

## The control-plane link

The agent runs as one Deployment in the `runos` namespace and maintains a
single, long-lived, mutually-authenticated (mTLS) gRPC bidirectional stream to
the RunOS control plane. The agent dials out and authenticates with its own
client certificate; the platform never opens a connection to the cluster. If the
link drops, the agent reconnects with backoff. Each inbound instruction is
dispatched to a handler, and the handler's result is sent back over the same
stream.

```
            mTLS gRPC stream (agent dials out)
  RunOS control plane  <───────────────────────────►  cluster agent (runos ns)
                                                             │
                                                             ├─ instructions/  (dispatch)
                                                             ├─ buildkitclient ──► ephemeral buildkitd pod ──► Harbor (runos-apps)
                                                             ├─ harborclient  ──► Harbor (runos-archives)
                                                             ├─ sqlwrapper    ──► tenant Postgres / MySQL
                                                             ├─ certcache     ──► Traefik cert Secret
                                                             ├─ dns01         ──► cert-manager DNS01 webhook
                                                             ├─ webhook        (health + presigned uploads)
                                                             └─ datastore      (shared PostgreSQL: jobs, logs, tokens)
```

## How a build works

Each container build starts a fresh, ephemeral `buildkitd` pod, drives it with
`buildctl`, imports/exports layer cache from the in-cluster Harbor registry, and
pushes the result. There is no long-lived build daemon: the per-build pod is
created, used for exactly one build, and deleted, which removes the
shared-daemon wedge class entirely. App images go to the `runos-apps` Harbor
project; CLI deploy archives are stored as single-layer OCI artifacts under
`runos-archives`.

## Upload token authorization

The datastore hashes each upload token and stores its expiry, purpose, and
single-use state. The datastore never stores the raw upload token.

Each token has an explicit `legacy` or `scoped` authorization classification.
Existing rows migrate to `legacy`. Pull and app-less image-build tokens also
remain `legacy` because the scoped upload contract does not cover them.

A `scoped` CLI upload stores the complete immutable
`runos.upload-authority-binding.v1` payload. The payload binds one verified
credential reference to the account, cluster, existing app, upload identity,
`apps.deploy` action, scope, decision, and expiry. Reads reject missing,
malformed, mismatched, unsupported, or secret-shaped binding data.

Token consumption changes only the existing single-use state. Later deployment
stages use separate job authority and do not modify the upload binding.

### Recheck before archive persistence

A `scoped` upload token is not enough by itself to persist an app's source
archive. Before the agent pushes anything to Harbor, it asks the control
plane for one fresh, current, correlated permit over the authenticated mTLS
stream (`agentstream.RequestStageRecheck`, the published
`runos.deployment-stage-request.v1` / `runos.deployment-stage-response.v1`
contract). The request carries the stored binding's `bindingId`/`uploadId`
and the `archive_persist` stage; it carries no bearer credential and no raw
upload token. Only an exact `permit` response - matching authority, stage,
and request correlation, within its own short issue/expiry window - lets the
upload continue. A `deny`, `needs`, `unavailable`, unknown outcome, or any
transport/validation failure (malformed reply, wrong binding, stale or
replayed proof, disconnect, timeout, recheck outage) refuses the upload with
a generic response that reveals nothing about the account, cluster, app,
binding, or credential involved.

An explicit `legacy` token never calls the recheck: it keeps its historical
authorization semantics. There is no fallback in either direction - a scoped
token that gets no permit does not retry as legacy, and a legacy token is
never promoted to a scoped check. A stored binding missing on a `scoped`
classification is treated as invalid, never as legacy.

**Decision boundary.** The recheck call is the last point that can still
refuse before anything changes. **Claim boundary.** The token's atomic
single-use claim (`MarkUploadTokenUsed`, an `UPDATE ... WHERE used = false`)
is the concurrency boundary and runs immediately before the first external
mutation (the archive push, or the build start for a build-image token): of
any number of requests that reach it for the same token, at most one
succeeds. Every available non-mutating check - token state, tarball
extraction limits, the BuildKit/Harbor configuration read, and the recheck
itself - runs before that claim, so a later failure never burns a token that
was in fact valid. Once a permit is granted and a request wins the claim,
the archive persist and build start that follow are not retried or rolled
back by this layer; a later deployment stage rechecks authority again before
its own mutation.

### Build-image registry identity

A `build_image` token can carry `harborOsid`: the Harbor service identity the
control plane verified and locked before minting the token. The agent
persists it on the token row alongside the target repo and tags. At upload
time, before any build starts and before any push, the agent compares that
persisted identity against the Harbor the shared BuildKit configuration
names at that exact moment (the same configuration read the push itself
uses, never a second one). A mismatch, or a configuration that names no
Harbor, refuses the upload with a named reason and starts no build and
pushes nothing. A `build_image` request that predates this field (empty
persisted identity) is an explicit legacy branch: it skips the comparison
and keeps the historical behavior unconditionally.

## Package map

| Package | Role |
|---|---|
| `agentstream` | Maintains the mTLS gRPC stream; dials, reconnects, dispatches instructions, sends replies. |
| `agentstream/instructions` | The instruction handlers (the work the platform dispatches). |
| `agentstream/l2sec` | Generated gRPC/protobuf types for the L2Sec transport. |
| `buildkitclient` | Ephemeral per-build BuildKit pods; build, cache, push, build-auth staging, startup sweep. |
| `harborclient` | Push/pull/list CLI deploy archives as OCI artifacts in Harbor. |
| `sqlwrapper` | SQL query/schema execution with read-only enforcement and connection pooling. |
| `certcache` | Mirrors the cluster-domain certificate into the Traefik Secret. |
| `dns01` | cert-manager ACME DNS01 solver webhook. |
| `webhook` | Local HTTP server: health probe, presigned tarball uploads, CLI deploy/pull, OSID generation. |
| `datastore` | Shared PostgreSQL storage for build jobs, logs, arguments, one-shot job audit, upload tokens, and schema cache. |
| `commons` | Dependency-free helpers (JSON+base64 message codec). |
| `version` | Build version, injected at release time via `-ldflags`. |

## Instruction catalog

Instructions are dispatched by type over the stream (see
`agentstream/instructions/instructions.go`). They arrive only over the
authenticated mTLS stream from the control plane; see [SECURITY.md](../SECURITY.md)
for the trust model and what this command surface means for the agent's
permissions.

| Instruction | What it does |
|---|---|
| `VCS_FETCH_SOURCE` | Clone/checkout source for a VCS deploy and resolve the build context + Dockerfile path. |
| `VCS_BUILD` | Build an image from fetched source via an ephemeral BuildKit pod and push to Harbor. |
| `HARBOR_IMAGE_EXISTS` | Check whether an image ref already exists in Harbor (skip rebuilds). |
| `CREATE_CLI_DEPLOY_TOKEN` | Mint a short-lived presigned upload token for a CLI deploy. |
| `CREATE_CLI_PULL_TOKEN` | Mint a short-lived presigned download token for pulling a stored archive. |
| `LIST_CLI_ARCHIVES` | List the CLI deploy archives stored in Harbor for an app. |
| `LIST_BUILD_JOBS` | List build jobs recorded in the local datastore. |
| `LIST_BUILD_LOGS` | Return the captured build log for a job. |
| `SQL_QUERY` | Execute a SQL query (read-only unless read-write is requested). |
| `SQL_SCHEMA` | Introspect and return a database's schema. |
| `RUN_ONESHOT_JOB` | Run a single command in a Kubernetes Job from an app's image. |
| `ONESHOT_JOB_STATUS` | Report a one-shot job's status and exit code. |
| `CLEANUP_ONESHOT_JOB` | Tear down a completed one-shot job. |
| `LIST_INGRESSES` | List ingresses in the cluster. |
| `LIST_CERTIFICATES` | List certificates managed in the cluster. |
| `POD_VIEWER` | Return pod stats/status for inspection. |
| `PUT_SECRET_FILE` | Write a secret file into a target Secret. |
| `WEB_REQUEST` / `WEB_REQUEST_FOLLOW` | Proxy a web request (and follow redirects) from inside the cluster. |
