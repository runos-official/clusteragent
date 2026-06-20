# RunOS Cluster Agent

The cluster agent connects your Kubernetes cluster to the [RunOS](https://runos.com)
platform. It runs as a single pod inside your cluster, on your hardware, and is
how RunOS builds and ships your applications, manages their databases, and keeps
your cluster's TLS certificates valid.

It only ever dials **out** to the RunOS control plane over an encrypted,
mutually-authenticated link, so there is no inbound port to open and nothing for
the platform to reach into. A cluster behind NAT or a firewall works without any
exposed ingress.

## What it does for you

- **Builds and ships your apps.** Turns your source or a Git commit into a
  container image and rolls it out, each build runs in its own throwaway
  builder, so builds stay isolated.
- **Runs database migrations** as one-off jobs, and runs SQL against your
  databases (read-only by default).
- **Keeps your TLS certificates issued and renewed** for your cluster's domain.
- **Serves deploys** initiated from the RunOS CLI or console.

## How it works

The cluster agent holds one long-lived, mutually-authenticated (mTLS) connection
to the RunOS control plane. The control plane sends it instructions ("build this
image", "run this migration", "issue this certificate") and the agent carries
them out inside your cluster, then reports back. Everything happens over that one
authenticated, outbound link, the platform cannot reach your cluster any other
way.

```
        mTLS link (agent dials out, no inbound port)
  RunOS control plane  <───────────────────────────►  cluster agent
                                                       (in your cluster)
```

If you want the full picture, the transport, the complete instruction set, and
the package layout, see [docs/architecture.md](docs/architecture.md).

## Installing

You don't install the cluster agent by hand. RunOS deploys it into your cluster
(one Deployment in the `runos` namespace) automatically when your cluster is
first configured, and updates it for you. Operators managing the manifest
directly can find the rendered Kubernetes manifest on each
[GitHub release](https://github.com/runos-official/clusteragent/releases).

## Security

The agent talks to the control plane over mutual TLS only, holds no inbound
listener for control traffic, and reads all credentials from Kubernetes Secrets
at runtime (none are baked into the image). Released images carry a keyless
Sigstore build-provenance attestation, so you can verify any image came from
this repository's pipeline:

```sh
gh attestation verify oci://ghcr.io/runos-official/clusteragent:<version> \
  --repo runos-official/clusteragent
```

Because the agent acts on your cluster on the control plane's behalf, it runs
with broad in-cluster permissions. See [SECURITY.md](SECURITY.md) for the trust
model, what that means for you, and how to report a vulnerability.

## Documentation

- [docs/architecture.md](docs/architecture.md) — how it works, in depth
- [docs/cluster-operations.md](docs/cluster-operations.md) — operating the agent
- [docs/certificate-management.md](docs/certificate-management.md) — certificates and DNS01
- [SECURITY.md](SECURITY.md) — security model and reporting
- [CONTRIBUTING.md](CONTRIBUTING.md) — building, testing, and releasing
- [CHANGELOG.md](CHANGELOG.md) — release history

## License

The RunOS cluster agent is **source-available** under the
[Elastic License 2.0](LICENSE): the source is published for transparency and
security review, not as open source. Use is subject to the license terms. See
[LICENSE](LICENSE) and [NOTICE](NOTICE). Copyright 2026 RunOS.
