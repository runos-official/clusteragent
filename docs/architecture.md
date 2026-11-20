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
single-use claim (`MarkUploadTokenUsed`, an `UPDATE ... WHERE used = false
AND expires_at > <the database's real current instant>`) is the
concurrency boundary and runs immediately before the first external
mutation (the archive push, or the build start for a build-image token): of
any number of requests that reach it for the same token, at most one
succeeds. The expiry side of that `WHERE` clause is evaluated by the
database engine itself, by dialect: PostgreSQL's `CURRENT_TIMESTAMP` is
fixed at transaction start, not at the moment this statement actually
takes effect, so a wait for the row's lock can let a claim through after
the real deadline; `clock_timestamp()` is PostgreSQL's true wall-clock
function and is what the claim compares against instead. Every available
non-mutating check - token state, tarball extraction limits, the
BuildKit/Harbor configuration read, and the recheck itself - runs before
that claim, so a later failure never burns a token that was in fact valid.

A granted permit is re-checked again, not just once: immediately before
the claim, and again immediately before the archive push, against both
its own expiry (equality counts as expired) and the control stream's
current generation. The stream generation increments on every connect,
disconnect, and reconnect; a validated recheck response carries the
generation that was current when it arrived, so a disconnect and
reconnect between receiving the permit and acting on it is detected even
though the permit's payload carries no connection identity of its own and
a replacement connection may already be live again by the time the check
runs. Once a permit is granted and a request wins the claim, the archive
persist and build start that follow are not retried or rolled back by this
layer; a later deployment stage rechecks authority again before its own
mutation.

### Recheck before queued build/deploy stages

Archive persistence is not the only mutation a CLI or VCS deploy performs.
After an upload is admitted (or, for VCS, before anything is cloned), the
agent asks for a fresh, current permit again at every later stage that
changes something: queue admission (creating the build job row and
starting the asynchronous work), build start (creating the BuildKit pod and
pushing the image), and deployment patch (the first Kubernetes deployment
update). A standalone build (`apps build`, and every app-less `build_image`
upload) stops after build start and never requests a deployment-patch
permit.

Every one of these later stages uses a **job** authority
(`agentstream.JobAuthority`), never the upload binding's `bindingId`/
`uploadId`: a durable, non-secret job id that the control plane assigns
before the first stage recheck for that job and that the agent carries
forward, unchanged, through every later stage for the same job. For a CLI
deploy the durable job id travels on `CREATE_CLI_DEPLOY_TOKEN`'s
`durableJobId` field, stored on the token row as a sibling of the upload
binding (never inside it - the upload binding's own `jobId` stays `null`).
For a VCS deploy it travels on `VCS_FETCH_SOURCE` and `VCS_BUILD`'s
`conductorJobId` field, a sibling of the existing `sha`/`jobId` fields,
never a repurposing of either: `VcsBuildRequest.JobID` keeps its pre-story
meaning (the commit SHA, used only for the BuildKit row's display id and
image tag) and is never treated as authority. `VCS_FETCH_SOURCE` binds the
durable job id (and the scoped/legacy classification) to its workdir cache
entry; `VCS_BUILD` must present the exact same values again, or it refuses
without creating a job row - a cache entry from a different job, app, or
source identity can never satisfy a different request.

Both fields are optional, and gate every later-stage recheck purely by
their own presence, never by the request's `authorizationClassification`:
Conductor's rollout of the durable job id is independent of, and currently
behind, the scoped-archive-persist rollout story 338 already consumes. A
scoped CLI upload with no `durableJobId`, or a VCS deploy with no
`conductorJobId` (today's real shape for every production Conductor),
keeps working exactly as story 338 left it - archive_persist still
rechecks via the upload binding; the later stages simply have no job
authority to recheck against yet and run unchecked until Conductor starts
sending one.

A build job row (`cluster_agent_buildkit_jobs`) persists an explicit
`authorization_classification` (`legacy` by default) and a non-secret
`authority_job_id`, exactly as the upload-token table persists its own
binding. Both are set once, at row creation, and are immutable afterwards:
a status update or the startup/restart cleanup below changes only status,
timestamps, and the failure log, never this attribution. An existing
pre-story row migrates to explicit `legacy`.

Queue admission is requested, and must permit, **before** the build job row
is created at all: a refusal leaves no job row and starts no asynchronous
work, so a scoped upload's already-pushed archive remains the only
completed effect for that attempt. A later stage's refusal (build start,
deployment patch) is unconditionally terminal for that job - the row
already exists by then - and marks it failed with one generic, non-secret
reason. Nothing is retried or rolled back once a stage's own mutation has
begun: a revocation after a build starts does not delete the pod or image
it already produced, and a revocation after the first Kubernetes update
attempt does not promise a rollback. The next stage always needs its own
fresh decision; no permit from an earlier stage, job, or retry authorizes
a later one. A VCS retry of the same commit always mints a fresh BuildKit
row id and therefore always requests fresh decisions - there is no cached
permit to replay. An explicit `legacy` job never calls any of these
rechecks, at any stage, matching the upload path's own no-fallback rule in
both directions.

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
