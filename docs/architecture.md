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
                                                             └─ datastore      (local SQLite: jobs, logs, tokens)
```

## How a build works

Each container build starts a fresh, ephemeral `buildkitd` pod, drives it with
`buildctl`, imports/exports layer cache from the in-cluster Harbor registry, and
pushes the result. There is no long-lived build daemon: the per-build pod is
created, used for exactly one build, and deleted, which removes the
shared-daemon wedge class entirely. App images go to the `runos-apps` Harbor
project; CLI deploy archives are stored as single-layer OCI artifacts under
`runos-archives`.

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
| `datastore` | Local SQLite: build jobs/logs/args, one-shot job audit, upload tokens, schema cache. |
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
