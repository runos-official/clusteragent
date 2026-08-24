#!/usr/bin/env python3
"""Tests for leakcheck.

This file is IDENTICAL in every public RunOS repo. Run it with:

    make leakcheck-test          (or: python3 scripts/leakcheck_test.py)

IMPORTANT, and it is the reason this file looks the way it does. This is a
PUBLIC repo, so the test must not itself publish the things it tests for. No
real internal identifier is ever written as a literal here. Every one is
assembled at run time from fragments that leakcheck cannot match, so this file
is clean when leakcheck scans the tree. Fixtures that need a harmless address
use the RFC 5737 documentation ranges.
"""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
CHECKER = os.path.join(HERE, "leakcheck.py")

FAILURES = []
CHECKS = 0


# ---------------------------------------------------------------------------
# Fragment assembly. Nothing below ever appears as a matchable literal.
# ---------------------------------------------------------------------------
def dotted(*octets) -> str:
    return ".".join(str(o) for o in octets)


# Configured machine names and account ids, split so leakcheck does not report
# its own test file. These add nothing that scripts/leakcheck.config does not
# already hold, and they carry no context beyond the bare token.
LAB_BOX = "ftb" + "1"
SECOND_BOX = "ftb" + "2"
RENTED_BOX = "fttb" + "3"
ACCOUNT_ID = "rj" + "wrn"

# The two addresses from the real incident: a line of pasted terminal output
# that shipped in cmd/preflight/tls_failure_kind_test.go. Assembled octet by
# octet so no dotted quad appears in this file.
PASTED_SRC = dotted(192, 168, 0, 226)
PASTED_DST = dotted(116, 203, 136, 98)
PASTED_ERROR = "read tcp %s:52618->%s:9191: i/o timeout" % (PASTED_SRC, PASTED_DST)
GLOBAL_V6 = ":".join(["2606", "4700", "4700"]) + "::" + "1111"

# Credential fixtures. Split so this file never holds a matchable credential
# shape: a credential finding is never baselineable, so a literal here would
# fail the gate on this repo for ever.
PRIVATE_KEY_HEADER = "-----BEGIN RSA " + "PRIVATE " + "KEY-----"
AWS_KEY_ID = "AKIA" + "IOSFODNN7EXAMPLE"
SECRET_ASSIGNMENT = "api" + "_key" + ' = "abcdefghijklmnopqrstuvwx"'


def run_checker(text: str, extra_args=()):
    """Write text to a temp file, scan it, return (exit code, stdout+stderr)."""
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, "fixture.txt")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(text)
        proc = subprocess.run(
            [sys.executable, CHECKER, "--no-baseline", "--paths", path] + list(extra_args),
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        return proc.returncode, proc.stdout


def expect_catch(label: str, text: str, token: str) -> None:
    global CHECKS
    CHECKS += 1
    code, out = run_checker(text)
    if code == 1 and token in out:
        print("  PASS  catch    %s" % label)
        return
    FAILURES.append(label)
    print("  FAIL  catch    %s (exit %d)\n%s" % (label, code, out))


def expect_clean(label: str, text: str) -> None:
    global CHECKS
    CHECKS += 1
    code, out = run_checker(text)
    if code == 0:
        print("  PASS  no-catch %s" % label)
        return
    FAILURES.append(label)
    print("  FAIL  no-catch %s (exit %d)\n%s" % (label, code, out))


def main() -> int:
    print("leakcheck tests")
    print()
    print("MUST CATCH")

    # The exact incident: the lab box name in a Go comment.
    expect_catch(
        "lab box name in a Go comment",
        "// (LINSTOR replicated-2 on %s's SAS spindles, under nested guests), so that\n" % LAB_BOX,
        LAB_BOX,
    )
    expect_catch(
        "lab box name in prose",
        "All three guests sat on ONE backing device: LINSTOR replicated-2 on %s's SAS spindles.\n" % LAB_BOX,
        LAB_BOX,
    )
    expect_catch("second lab box name", "// Measured on %s after a reset.\n" % SECOND_BOX, SECOND_BOX)
    expect_catch("rented box name with digits", "# provisioned %s in fsn1\n" % RENTED_BOX, RENTED_BOX)
    expect_catch("lab box name inside a hyphenated word", "host node-%s-a rebooted\n" % LAB_BOX, LAB_BOX)
    expect_catch("lab box name in mixed case", "Measured on %s.\n" % LAB_BOX.upper(), LAB_BOX)
    expect_catch("account id in a node name", 'want "n02-ede-%s" in the detail\n' % ACCOUNT_ID, ACCOUNT_ID)

    # The already-shipped pasted output, both addresses in one line.
    expect_catch("pasted source address in a shipped test comment", '// "the secure handshake FAILED: %s",\n' % PASTED_ERROR, PASTED_SRC)
    expect_catch("pasted destination address in the same line", '// "the secure handshake FAILED: %s",\n' % PASTED_ERROR, PASTED_DST)

    # Other ratcheted address shapes.
    expect_catch("RFC1918 address", "listen-address=%s\n" % dotted(10, 0, 0, 1), dotted(10, 0, 0, 1))
    expect_catch("public address", "resolver %s\n" % dotted(8, 8, 8, 8), dotted(8, 8, 8, 8))
    expect_catch("address inside a CIDR", "the pod range %s/16\n" % dotted(172, 25, 0, 0), dotted(172, 25, 0, 0))
    expect_catch("global IPv6 address", "nameserver %s\n" % GLOBAL_V6, GLOBAL_V6)

    # Credentials, which are never baselineable.
    expect_catch("AWS access key id", "aws_access_key_id = %s\n" % AWS_KEY_ID, AWS_KEY_ID)
    expect_catch("private key block", PRIVATE_KEY_HEADER + "\n", PRIVATE_KEY_HEADER)
    expect_catch("quoted secret assignment", SECRET_ASSIGNMENT + "\n", SECRET_ASSIGNMENT)

    print()
    print("MUST NOT CATCH")

    # RFC 5737 and RFC 3849. These exist so people can write about addresses.
    expect_clean("RFC 5737 TEST-NET-1", "gateway %s\n" % dotted(192, 0, 2, 1))
    expect_clean("RFC 5737 TEST-NET-2", "peer %s\n" % dotted(198, 51, 100, 7))
    expect_clean("RFC 5737 TEST-NET-3", "endpoint %s:51820\n" % dotted(203, 0, 113, 5))
    expect_clean("RFC 5737 TEST-NET-3 CIDR", "the LAN %s/24 overlaps\n" % dotted(203, 0, 113, 0))
    expect_clean("RFC 3849 documentation IPv6", "nameserver 2001:db8::1\n")

    # Loopback, link-local, unspecified, broadcast, well-known multicast.
    expect_clean("loopback", "dial %s:9191\n" % dotted(127, 0, 0, 1))
    expect_clean("systemd-resolved stub", "nameserver %s\n" % dotted(127, 0, 0, 53))
    expect_clean("cloud metadata link-local", "curl http://%s/latest\n" % dotted(169, 254, 169, 254))
    expect_clean("unspecified address", "bind %s:8080\n" % dotted(0, 0, 0, 0))
    expect_clean("limited broadcast", "send to %s\n" % dotted(255, 255, 255, 255))
    expect_clean("well-known multicast", "join %s\n" % dotted(224, 0, 0, 251))
    expect_clean("IPv6 loopback", "dial [::1]:9191\n")
    expect_clean("IPv6 unspecified", "listen on ::\n")
    expect_clean("IPv6 link-local", "fe80::1%eth0\n")
    expect_clean("IPv6 link-local multicast", "ff02::fb is mDNS\n")

    # Things that look numeric but are not addresses. These are the false
    # positives that would get the gate switched off.
    expect_clean("semver", "released v1.24.3 today\n")
    expect_clean("four-part product version", "Kubernetes 1.31.2 and containerd 1.7.20\n")
    expect_clean("go directive", "go 1.24.0\n")
    expect_clean("ISO date", "measured 2026-08-22 on hardware\n")
    expect_clean("dotted date", "measured 2026.08.22 on hardware\n")
    expect_clean("dotted config key", "runos set-config node.disk.size.gb 200\n")
    expect_clean("five dotted numbers", "sequence 1.2.3.4.5 is not an address\n")
    expect_clean("octet above 255", "build 999.888.777.666\n")
    expect_clean("address glued to a word", "revision abc10.0.0.1def\n")
    expect_clean("clock time", "took 10:30:00 to converge\n")
    expect_clean("MAC address", "link/ether 00:11:22:33:44:55 brd ff:ff:ff:ff:ff:ff\n")
    expect_clean("commit sha", "pinned at 3b2c1a9f8e7d6c5b4a39281706f5e4d3c2b1a098\n")
    expect_clean("OIDC identity url", "https://github.com/runos-official/nodeagent/.github/workflows/release.yml@refs/tags/v0.24.0\n")
    expect_clean("public runos hosts", "curl https://get.runos.com | bash  # registers with nodeward.runos.com\n")
    expect_clean("lab box name inside a longer word", "the sandbox1 harness and a leftb1as variable\n")
    expect_clean("port only", "listening on :9191\n")

    print()
    print("%d checks, %d failures" % (CHECKS, len(FAILURES)))
    if FAILURES:
        for name in FAILURES:
            print("  failed: %s" % name)
        return 1
    print("leakcheck tests passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
