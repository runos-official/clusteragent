# Contributing

Thanks for contributing to the RunOS cluster agent.

## Prerequisites

- **Go 1.24+** (see `go.mod`). CGO must be enabled for the SQLite driver
  (`CGO_ENABLED=1`); `make build` sets this for you.
- **Docker Buildx** if you build the container image locally.

## Build and test

```sh
make build       # build a local binary stamped with the version
make test        # go test -race ./...
make vet         # go vet ./...
make image       # build the multiarch image locally (no push)
```

Before opening a PR, run the same gates the release pipeline runs:

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .        # must print nothing
```

## Code conventions

- **Idiomatic Go.** Follow Effective Go and the Go Code Review Comments.
  Initialisms stay all-caps in exported names (`URL`, `ID`, `JSON`, `OSID`,
  `SQL`), camelCase for unexported. Handle errors explicitly.
- **Doc comments on exported symbols.** Every exported type, function, and const
  block gets a doc comment that leads with the symbol name. Every package has a
  leading `// Package <name> ...` comment.
- **Keep packages focused.** Business logic lives in the relevant package; the
  instruction handlers in `agentstream/instructions` stay thin and delegate.
- **Table-driven tests.** Use `t.Run(tc.name, ...)` for any function with more
  than two cases. Use `t.Helper()` in test helpers, `t.Cleanup(fn)` over
  `defer`, `t.TempDir()` for filesystem fixtures, and `t.Setenv` for env
  overrides. Prefer extracting and testing a pure helper over standing up live
  infrastructure. Tests for security-sensitive logic (the read-only SQL gate,
  DNS-1123 name sanitization, path-traversal checks) are mandatory.
- **Named constants for magic numbers.** Timeouts, size caps, and ports live as
  named constants near the top of the package, not inline at call sites.

## Public-repo rules

This is a public repository. Two hard rules:

- **No secrets.** Never commit credentials, tokens, private keys, or mTLS
  material. The agent reads all secrets at runtime from Kubernetes Secrets and
  ConfigMaps. The release script enforces a deterministic secret-pattern scan
  over the release payload and aborts on any hit.
- **No real identifiers.** Never commit real account IDs, cluster IDs, app IDs,
  OSIDs, customer names, internal hostnames, or IPs into code, tests, fixtures,
  comments, or docs. Use obvious placeholders: `myacct`, `mycluster`, `myapp`,
  `myosid`, `app-ab12c`, `harbor.example.com`, `acme`.

## Releasing

Releases are cut on a `v*` git tag and published by GitHub Actions; see
[README.md](README.md) and [SECURITY.md](SECURITY.md) for the pipeline and trust
model. The canonical path is [`scripts/release.sh`](scripts/release.sh) (fronted
by `make release`), which runs preflight checks, a fail-closed public-repo
sensitivity scan, and the build/vet/test gates before tagging and pushing. Review
the diff and confirm no real account/cluster/app identifiers are present before
you release.

1. Add a `## vX.Y.Z` section to [CHANGELOG.md](CHANGELOG.md) (the release uses it
   as the GitHub release notes; the script refuses to release a version with no
   matching section).
2. Run `make release RELEASE_VERSION=vX.Y.Z` (add `CHECK=1` for a no-side-effect
   dry run that stops before any tag/push).
