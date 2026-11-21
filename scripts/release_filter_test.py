#!/usr/bin/env python3
"""Tests for the embedded PLACEHOLDER_FILTER in scripts/release.sh.

leakcheck.py's own CREDENTIAL_RE is tested in-process by leakcheck_test.py.
This file tests the OTHER copy: the sensitivity-scan filter release.sh embeds
in a heredoc and runs over the deploy payload diff. The two are meant to stay
"kept in step" (release.sh's own comment says so), but nothing enforced that
until now; a drift here would only surface at release time, against a real
payload, which is the wrong place to learn a format was never added.

This extracts the filter's actual source out of release.sh and runs it the
same way release.sh does: pipe candidate lines into `python3 -c "$FILTER"` on
stdin, one line flagged per line of output. It never hand-copies the regex, so
editing release.sh's filter and forgetting leakcheck.py (or vice versa) shows
up here as a real failure, not a silent pass.

PUBLIC REPO: as in leakcheck_test.py, no real credential-shaped literal is
written here. Every fixture is assembled at run time from fragments.

Run with: python3 scripts/release_filter_test.py
"""

from __future__ import annotations

import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RELEASE_SH = os.path.join(HERE, "release.sh")

FAILURES = []
CHECKS = 0


def extract_filter() -> str:
    """Pull the PYFILTER heredoc body out of release.sh."""
    with open(RELEASE_SH, "r", encoding="utf-8") as fh:
        text = fh.read()
    match = re.search(r"<<'PYFILTER'[^\n]*\n(.*?)\nPYFILTER\n", text, re.DOTALL)
    if not match:
        sys.stderr.write("release_filter_test: could not find the PYFILTER heredoc in release.sh\n")
        sys.exit(2)
    return match.group(1)


FILTER_SRC = extract_filter()


def run_filter(line: str) -> str:
    """Feed one line to the real filter on stdin, exactly as release.sh does."""
    proc = subprocess.run(
        [sys.executable, "-c", FILTER_SRC],
        input=line if line.endswith("\n") else line + "\n",
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    return proc.stdout


def expect_flagged(label: str, line: str) -> None:
    global CHECKS
    CHECKS += 1
    out = run_filter(line)
    if out.strip():
        print("  PASS  flagged    %s" % label)
        return
    FAILURES.append(label)
    print("  FAIL  flagged    %s (filter produced no output)" % label)


def expect_not_flagged(label: str, line: str) -> None:
    global CHECKS
    CHECKS += 1
    out = run_filter(line)
    if not out.strip():
        print("  PASS  not-flagged %s" % label)
        return
    FAILURES.append(label)
    print("  FAIL  not-flagged %s (filter output: %r)" % (label, out))


# ---------------------------------------------------------------------------
# Fragments. Nothing below is a matchable credential literal.
# ---------------------------------------------------------------------------
RUNOS_PAT_PREFIX = "runos" + "_pat_"
RUNOS_PAT_UUID_ID = "a1b2c3d4-e5f6-47a8-91bc-" + "ef1234567890"
RUNOS_PAT_LEGACY_ID = "AbCdEfGhJk"
RUNOS_PAT_LEGACY_TAIL = "MnPqRsTuVwXyZ23456789" + "AbCdEfGhJkMn"

RUNOS_KEY_PREFIX = "runos" + "_key_"
RUNOS_KEY_UUID_ID = RUNOS_PAT_UUID_ID
SECRET43 = ("Ab3xK9zQ7mN2pR5tV8wY-Cd1Ef4Gh7" + "Jk0Mn_Pq2x1Yz")[:43]

CS_PREFIX = "c" + "s_"
CS_SECRET43 = ("Qw7e-Rt9y_Ui2o" + "Pa5s-Df8g_Hj1k" + "Lz4x-Cv6b_Nm3q" + "Zx")[:43]

AWS_KEY_ID = "AKIA" + "IOSFODNN7EXAMPLE"


def main() -> int:
    print("release.sh filter tests")
    print()
    print("MUST FLAG")

    expect_flagged(
        "runos_pat_ still flagged (legacy id)",
        "+runos login --api-key %s%s.%s" % (RUNOS_PAT_PREFIX, RUNOS_PAT_LEGACY_ID, RUNOS_PAT_LEGACY_TAIL),
    )
    expect_flagged(
        "runos_pat_ with a canonical UUID id",
        "+export X=%s%s.%s" % (RUNOS_PAT_PREFIX, RUNOS_PAT_UUID_ID, SECRET43),
    )
    expect_flagged(
        "runos_key_ Account API Key",
        "+export RUNOS_API_KEY=%s%s.%s" % (RUNOS_KEY_PREFIX, RUNOS_KEY_UUID_ID, SECRET43),
    )
    expect_flagged(
        "Roid cs_ OAuth client secret",
        "+Authorization: Bearer %s%s" % (CS_PREFIX, CS_SECRET43),
    )
    expect_flagged("AWS access key id (regression)", "+aws_access_key_id = %s" % AWS_KEY_ID)

    print()
    print("MUST NOT FLAG")

    expect_not_flagged(
        "runos_pat_ prefix named in prose",
        "+New personal PATs still use the %s prefix." % RUNOS_PAT_PREFIX,
    )
    expect_not_flagged(
        "runos_key_ prefix named in prose",
        "+New Account API Keys use the %s prefix." % RUNOS_KEY_PREFIX,
    )
    expect_not_flagged(
        "runos_key_ documentation placeholder",
        "+%s<credentialId>.<secret>" % RUNOS_KEY_PREFIX,
    )
    expect_not_flagged(
        "cs_ prefix named in prose, no secret after it",
        "+the %s prefix identifies an OAuth client secret" % CS_PREFIX,
    )
    expect_not_flagged(
        "cs_ prefix followed by too few characters",
        "+token %s%s" % (CS_PREFIX, CS_SECRET43[:20]),
    )
    expect_not_flagged(
        "env var name as a quoted value (1.0.1 placeholder filter, regression)",
        '+"password": "POSTGRES_PASSWORD",',
    )
    expect_not_flagged(
        "hyphenated lowercase placeholder (1.0.1 placeholder filter, regression)",
        '+refreshToken: "valid-refresh-token",',
    )

    print()
    print("%d checks, %d failures" % (CHECKS, len(FAILURES)))
    if FAILURES:
        for name in FAILURES:
            print("  failed: %s" % name)
        return 1
    print("release.sh filter tests passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
