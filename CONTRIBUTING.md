# Contributing

Thanks for contributing to the RunOS cluster agent.

## Prerequisites

- **Go 1.24+** (see `go.mod`). CGO must be enabled for the SQLite driver
  (`CGO_ENABLED=1`); `make build` sets this for you.
- **Docker Buildx** if you build the container image locally.

## First, install the git hooks

Run this once per clone, right after you clone:

```sh
make hooks       # sets core.hooksPath to the tracked .githooks/ directory
```

`.git/hooks` is not tracked, so a hook that is not installed is a hook nobody
has. The `pre-commit` hook runs `leakcheck` over your staged diff and blocks a
commit that would publish a credential or a new internal identifier. See
[The leak gate](#the-leak-gate) below.

## Build and test

```sh
make build       # build a local binary stamped with the version
make test        # go test -race ./...
make vet         # go vet ./...
make image       # build the multiarch image locally (no push)
make leakcheck   # public-repo leak gate over every tracked file
```

Before opening a PR, run the same gates the release pipeline runs:

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .        # must print nothing
make leakcheck    # must print "leakcheck: clean"
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
  comments, or docs. This includes named lab or rented test machines, and
  pasted terminal output that carries a real address. Use obvious placeholders:
  `myacct`, `mycluster`, `myapp`, `myosid`, `app-ab12c`, `harbor.example.com`,
  `acme`. For addresses, use the ranges that exist for exactly that purpose:
  `192.0.2.0/24`, `198.51.100.0/24` and `203.0.113.0/24` (RFC 5737), and
  `2001:db8::/32` (RFC 3849).

### The leak gate

`scripts/leakcheck.py` enforces the second rule. It runs in three places: the
`pre-commit` hook (staged diff only, fast), `make leakcheck` (on demand), and
`scripts/release.sh` (whole tree, and it cannot be skipped).

```sh
make leakcheck          # scan every tracked file
make leakcheck-staged   # scan only what is staged
make leakcheck-test     # test the checker itself
make leakcheck-update   # ratchet the baseline down after removing an identifier
```

It has two severities.

- **Credentials** hard fail, always. They can never be baselined.
- **Internal identifiers** are ratcheted, like a knip dead-code baseline.
  `scripts/leakcheck.baseline` records what this repo has already published, so
  existing work is not blocked. A NEW identifier fails the gate.

What counts as an internal identifier: the machine names and account ids listed
in `scripts/leakcheck.config`, and any IP address literal outside the
documentation, loopback, link-local, unspecified, broadcast and well-known
multicast ranges. Addresses are allow-listed rather than deny-listed because you
cannot tell a real address from an invented one by reading it. A project
constant such as a service CIDR is absorbed into the baseline once and never
asked about again.

**Do not hand-add a line to `scripts/leakcheck.baseline` to get a commit
through.** A line in that file is a record of a leak that already shipped, not a
licence to add another. Remove the identifier from the source instead, then run
`make leakcheck-update` so the baseline shrinks.

The pre-commit hook can be skipped in a genuine emergency with
`git commit --no-verify`, and it says so when it fires. That does not get the
change released: the release gate runs the same checker over the whole tree.

`scripts/leakcheck.py`, `scripts/leakcheck.config` (above its per-repo excludes)
and `scripts/leakcheck_test.py` are identical in every public RunOS repo. Do not
fork them here. Fix the checker in one place, bump `LEAKCHECK_VERSION`, and copy
it across so the drift shows in a diff.

## Releasing

Releases are cut on a `v*` git tag and published by GitHub Actions; see
[README.md](README.md) and [SECURITY.md](SECURITY.md) for the pipeline and trust
model. The canonical path is [`scripts/release.sh`](scripts/release.sh) (fronted
by `make release`), which runs preflight checks, a fail-closed public-repo
sensitivity scan, the leak gate, and the build/vet/test gates before tagging and
pushing. Review
the diff and confirm no real account/cluster/app identifiers are present before
you release.

1. Add a `## vX.Y.Z` section to [CHANGELOG.md](CHANGELOG.md) (the release uses it
   as the GitHub release notes; the script refuses to release a version with no
   matching section).
2. Run `make release RELEASE_VERSION=vX.Y.Z` (add `CHECK=1` for a no-side-effect
   dry run that stops before any tag/push).
