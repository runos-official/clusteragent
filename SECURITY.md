# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in any RunOS product or service,
including the cluster agent, API, web console, or infrastructure, please report
it responsibly.

**Email:** security@runos.com

Please include:
- A description of the vulnerability
- Steps to reproduce
- Potential impact

We will acknowledge reports within 8 hours and aim to provide a fix or
mitigation plan within 3 days for critical issues.

## Security Practices

- **No hardcoded secrets.** The repository contains no credentials, tokens, or
  private keys. The cluster agent reads everything it needs at runtime from
  Kubernetes Secrets and ConfigMaps (Harbor credentials, the agent's mTLS
  certificate, tenant database connection parameters), never from the image or
  source.
- **Read-only SQL by default.** `SQL_QUERY` execution sets the database session
  read-only unless the caller explicitly requests read-write, and the agent
  classifies each statement before routing it. A query is sent to the read path;
  a write requires the explicit read-write flag, so an unintended write is
  rejected by the database rather than silently executed.
- **In-cluster TLS.** The control-plane link is a mutually authenticated (mTLS)
  gRPC stream. The agent dials out and authenticates with its own dedicated
  mTLS certificate (provisioned at bootstrap and rotated automatically before
  expiry); the platform never needs an inbound port into the cluster.
- **Least-exposure build pods.** Each build runs in an ephemeral `buildkitd`
  pod created for exactly one build and deleted afterward. Build pods do not
  mount a service-account token, so a privileged build cannot reach the
  Kubernetes API with cluster credentials.
- **Credential redaction in logs.** Build command lines and BuildKit output are
  redacted (`user:pass@host` becomes `***:***@host`) before they reach logs or
  the build record.
- **Short-lived presigned tokens.** CLI deploy/pull use short-lived upload and
  download tokens minted per operation and stored in the local datastore with
  an expiry; they are not long-lived API keys.
- **Conservative pod security context.** The agent Deployment sets a
  `RuntimeDefault` seccomp profile, drops all Linux capabilities, and forbids
  privilege escalation (`allowPrivilegeEscalation: false`). It does **not** set
  `runAsNonRoot` or `readOnlyRootFilesystem`: the agent writes to the `/data`
  PVC and shells out to `kubectl`/`helm`/`buildctl`, so those two controls
  would break it and are intentionally omitted.

### RBAC scope (cluster-admin-equivalent, by design)

The cluster agent runs under a ClusterRole that is, in practice,
**cluster-admin-equivalent**. This is a deliberate and currently necessary
trade-off, not an oversight:

- The agent's whole job is to execute control-plane-issued operations against
  the cluster on the operator's behalf: applying and deleting arbitrary custom
  resources, running arbitrary `kubectl` and `helm` commands, creating build
  pods, and managing Secrets/ConfigMaps across namespaces. The set of objects
  and verbs it may touch is open-ended because the instructions it receives are
  open-ended.
- Because the operations are not known ahead of time, a tightly scoped RBAC
  policy cannot enumerate them without breaking legitimate deploys. The
  security boundary that matters here is therefore the **mutually
  authenticated (mTLS) control-plane link** that decides *what* the agent is
  told to do, plus the per-build pod isolation described above, rather than the
  in-cluster RBAC grant.
- **Operator implications.** Anyone who can issue instructions over the trusted
  control-plane stream can effectively act as cluster-admin on this cluster.
  Protect the control-plane credentials accordingly and treat the agent's
  namespace as a high-trust boundary.
- **Future hardening.** Narrowing this ClusterRole (e.g. per-tenant namespaced
  roles, an allowlist of resource kinds/verbs, or admission-webhook gating of
  the instructions the agent will execute) is tracked as future work. It is not
  done today because doing it safely requires constraining the instruction set
  first.

## Release Integrity & Trust Model

The container image a cluster runs is cryptographically tied to the artifact
this repository's release pipeline produced.

1. **Keyless build-provenance attestation.** The release workflow
   (`.github/workflows/release.yml`) runs `actions/attest-build-provenance`,
   which issues a keyless Sigstore attestation for every released image. The
   attestation binds the image digest to this workflow's OIDC identity. No
   long-lived signing secret is stored, which is safe for a public repo. Verify
   a released image with:

   ```sh
   gh attestation verify oci://ghcr.io/runos-official/clusteragent:<v> \
     --repo runos-official/clusteragent
   ```

2. **Stable vs. release-candidate tags.** Only a stable (non-prerelease) build
   moves the `:latest` tag. A pre-release tag with a semver suffix
   (for example `v0.18.0-rc.1`) is published and pinnable by its exact version
   and flagged a GitHub prerelease, but never tagged `:latest`, so normal
   consumers are not silently moved onto a candidate build.

### What attestation does NOT guarantee (accepted limitation)

Attestation proves an artifact *came from this workflow*. It does **not** prove
the artifact is benign. A compromised build, malicious code merged into the
release branch, or a subverted workflow/runner would produce an
evil-but-validly-attested image that passes verification. The attestation is
blind to this class of attack.

The only defense against a subverted build is repo-side access control: keeping
malicious code and workflow edits out in the first place. The two layers below
are that defense.

### Codified in-repo: pinned action SHAs

Every `uses:` in the release workflow is pinned to a full 40-character commit
SHA with a trailing version comment (for example
`actions/attest-build-provenance@a2bbfa25375fe432b6a289bc6b6cd05ecd0c4c32 # v4.1.0`).
A moved tag on a third-party action therefore cannot silently swap in new code.
**Re-confirm this on every workflow edit**, and pin any newly added action the
same way before merging.

### Admin-only hardening checklist (human/admin action required)

These controls cannot be enforced by files in this repo. A GitHub org/repo
admin must apply them in repository settings; they are the actual mitigation for
the build-compromise limitation above:

- [ ] **Branch protection** on the release branch: require pull requests, block
  direct pushes and force-pushes.
- [ ] **Required PR review**: at least one approving review before merge.
- [ ] **Restrict workflow edits**: limit who can modify files under
  `.github/workflows/` and tighten the repo's Actions permissions.
- [ ] **Restrict tag push and release publishing**: the release workflow
  triggers on `v*` tag pushes, so control over who can push tags and publish
  releases is control over what gets attested and shipped.
