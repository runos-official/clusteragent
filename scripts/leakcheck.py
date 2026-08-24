#!/usr/bin/env python3
"""leakcheck: keep internal identifiers and credentials out of a PUBLIC repo.

This file is IDENTICAL in every public RunOS repo (nodeagent, clusteragent,
desktop, cli). Do not fork it. Change it here, then copy it across and bump
LEAKCHECK_VERSION so drift is visible in a diff.

It has no dependency beyond python3 and git.

TWO SEVERITIES
--------------
1. credential  Hard fail, always. Never baselineable. Same shapes as the
               SECRET_RE floor in scripts/release.sh, so behaviour matches.
2. internal    Ratcheted, like a knip dead-code baseline. A finding already
               listed in the baseline file passes. A NEW finding fails. The
               baseline only shrinks unless a human runs --update.

WHAT COUNTS AS AN INTERNAL IDENTIFIER
-------------------------------------
* Named lab machines and rented boxes, and account ids, both read from
  scripts/leakcheck.config.
* IP address literals, by ALLOW-LIST, not deny-list. You cannot tell a real
  address from an invented one by looking at it, so only the ranges that are
  reserved for writing about (documentation, loopback, link-local, unspecified,
  broadcast, well-known multicast) pass unasked. Everything else, RFC1918
  included, is ratcheted: a pasted lab address is caught, and a project
  constant such as a service CIDR is absorbed into the baseline once and never
  asked about again.

MODES
-----
  leakcheck.py                     scan every tracked file (the authoritative mode)
  leakcheck.py --staged            scan only added lines in the staged diff (pre-commit)
  leakcheck.py --range A..B        scan only added lines in a commit range
  leakcheck.py --paths F [F...]    scan the named files (used by the tests)
  leakcheck.py --update            rewrite the baseline from a full tree scan
  leakcheck.py --version           print the checker version

Exit codes: 0 clean, 1 findings, 2 usage or environment error.
"""

from __future__ import annotations

import argparse
import fnmatch
import ipaddress
import os
import re
import subprocess
import sys

LEAKCHECK_VERSION = "1.0.0"

# ---------------------------------------------------------------------------
# Credential shapes. Kept in step with SECRET_RE in scripts/release.sh.
# High precision on purpose: it must not fire on commit SHAs, pinned action
# SHAs, or the public OIDC identity URL.
# ---------------------------------------------------------------------------
CREDENTIAL_RE = re.compile(
    r"(gh[pousr]_[A-Za-z0-9]{20,})"
    r"|(github_pat_[A-Za-z0-9_]{20,})"
    r"|(xox[baprs]-[A-Za-z0-9-]{10,})"
    r"|(AKIA[0-9A-Z]{16})"
    r"|(-----BEGIN [A-Z ]*PRIVATE KEY-----)"
    r"|((api[_-]?key|secret|password|passwd|token|bearer)[\"' ]*[:=][\"' ]*[\"'][A-Za-z0-9/+_.=-]{16,}[\"'])",
    re.IGNORECASE,
)

# ---------------------------------------------------------------------------
# IP literals.
#
# IPv4: four octets, each 0-255, and NOT part of a longer dotted run. The
# lookaround on "." is what keeps semver, dates and dotted versions out:
# "1.2.3" is three octets so it never matches, and "1.2.3.4.5" is rejected
# because a dot sits on the boundary.
# ---------------------------------------------------------------------------
IPV4_CANDIDATE_RE = re.compile(r"(?<![0-9A-Za-z._-])(\d{1,3}(?:\.\d{1,3}){3})(?![0-9A-Za-z._-])")

# IPv6: a candidate needs at least two colons, then it must either contain "::"
# or carry the full seven colons. That is what stops a clock ("10:30:00") and a
# MAC address ("00:11:22:33:44:55") from being read as an address. Every
# candidate is then validated by the ipaddress module, so nothing shaped like
# an address but invalid survives.
IPV6_CANDIDATE_RE = re.compile(
    r"(?<![0-9A-Za-z:._-])([0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7})(?![0-9A-Za-z:._-])"
)

# Ranges that exist so people can write about addresses. These never fire.
ALLOWED_NETS = [
    ipaddress.ip_network(cidr)
    for cidr in (
        "192.0.2.0/24",  # RFC 5737 TEST-NET-1
        "198.51.100.0/24",  # RFC 5737 TEST-NET-2
        "203.0.113.0/24",  # RFC 5737 TEST-NET-3
        "127.0.0.0/8",  # loopback
        "169.254.0.0/16",  # IPv4 link-local
        "224.0.0.0/24",  # well-known (local network control) multicast
        "0.0.0.0/32",  # unspecified
        "255.255.255.255/32",  # limited broadcast
        "2001:db8::/32",  # RFC 3849 documentation
        "fe80::/10",  # IPv6 link-local
        "ff01::/16",  # IPv6 interface-local multicast (well-known groups)
        "ff02::/16",  # IPv6 link-local multicast (well-known groups)
        "::/128",  # unspecified
        "::1/128",  # loopback
    )
]

# Forced so a local git config (diff.noprefix, diff.external, textconv) cannot
# change the shape of the diff the parser reads.
DIFF_ARGS = ("--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "-U0")

BASELINE_HEADER = """\
# leakcheck baseline (leakcheck v{version})
#
# WHAT THIS FILE IS
# Every line below is an internal identifier that is ALREADY PUBLISHED in this
# public repo. The gate ignores these lines so that existing work is not
# blocked, and fails on anything new.
#
# A LINE HERE IS A RECORD OF A LEAK THAT ALREADY HAPPENED. IT IS NOT A LICENCE
# TO ADD MORE. Never hand-add a line to get a commit through. The honest way to
# clear a finding is to delete the identifier from the source and run
# `make leakcheck-update`, which is the only direction this file should move.
#
# Columns are tab separated: <path> <kind> <token>
# Regenerate with: make leakcheck-update
"""


class Finding:
    __slots__ = ("path", "line", "kind", "token", "snippet")

    def __init__(self, path: str, line: int, kind: str, token: str, snippet: str):
        self.path = path
        self.line = line
        self.kind = kind
        self.token = token
        self.snippet = snippet

    @property
    def key(self) -> tuple:
        return (self.path, self.kind, self.token)


# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
def load_config(path: str) -> dict:
    """Read scripts/leakcheck.config.

    Format is one `key: value` per line. Blank lines and lines starting with #
    are ignored. Repeated keys accumulate.
    """
    cfg = {"name": [], "id": [], "exclude": []}
    if not os.path.exists(path):
        fail("missing config file: %s" % path)
    with open(path, "r", encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if ":" not in line:
                fail("bad config line in %s: %r" % (path, raw.rstrip()))
            key, value = line.split(":", 1)
            key = key.strip()
            value = value.strip()
            if key not in cfg:
                fail("unknown config key %r in %s" % (key, path))
            if value:
                cfg[key].append(value)
    return cfg


def build_identifier_patterns(cfg: dict) -> list:
    """Compile one word-boundary regex per configured name and id.

    Word boundaries mean a bare token hits and an unrelated longer word does
    not. For a configured token "box9": "box9" matches, "sandbox9a" does not,
    and "node-box9-a" does, because a hyphen is a boundary.
    """
    patterns = []
    for kind, values in (("name", cfg["name"]), ("id", cfg["id"])):
        for value in values:
            # A trailing "+" in the config means "followed by one or more digits".
            if value.endswith("+"):
                body = re.escape(value[:-1]) + r"\d+"
            else:
                body = re.escape(value)
            patterns.append((kind, re.compile(r"(?<![0-9A-Za-z_])(" + body + r")(?![0-9A-Za-z_])", re.IGNORECASE)))
    return patterns


# ---------------------------------------------------------------------------
# Scanning
# ---------------------------------------------------------------------------
def classify_ip(token: str):
    """Return the address if it must be reported, or None if it is allowed."""
    try:
        addr = ipaddress.ip_address(token)
    except ValueError:
        return None
    for net in ALLOWED_NETS:
        if addr.version == net.version and addr in net:
            return None
    return addr


def scan_line(path: str, lineno: int, text: str, patterns: list) -> list:
    findings = []
    snippet = text.strip()
    if len(snippet) > 160:
        snippet = snippet[:157] + "..."

    for match in CREDENTIAL_RE.finditer(text):
        findings.append(Finding(path, lineno, "credential", match.group(0)[:40], snippet))

    for kind, pattern in patterns:
        for match in pattern.finditer(text):
            findings.append(Finding(path, lineno, kind, match.group(1).lower(), snippet))

    for match in IPV4_CANDIDATE_RE.finditer(text):
        token = match.group(1)
        if classify_ip(token) is not None:
            findings.append(Finding(path, lineno, "ipv4", token, snippet))

    for match in IPV6_CANDIDATE_RE.finditer(text):
        token = match.group(1)
        if "::" not in token and token.count(":") != 7:
            continue
        if classify_ip(token) is not None:
            findings.append(Finding(path, lineno, "ipv6", token.lower(), snippet))

    return findings


def read_text(path: str):
    """Return the file's text, or None when it is binary or unreadable."""
    try:
        with open(path, "rb") as fh:
            blob = fh.read()
    except OSError:
        return None
    if b"\x00" in blob:
        return None
    try:
        return blob.decode("utf-8")
    except UnicodeDecodeError:
        return None


def scan_files(paths, patterns, skip_globs) -> list:
    findings = []
    for path in paths:
        if is_skipped(path, skip_globs):
            continue
        text = read_text(path)
        if text is None:
            continue
        for lineno, line in enumerate(text.splitlines(), start=1):
            findings.extend(scan_line(path, lineno, line, patterns))
    return findings


# leakcheck's own config names the internal machines and its baseline lists the
# published tokens, so both would report themselves. Skipping them is not a
# loophole: neither file may hold anything but bare tokens. Matched by basename
# so the skip does not depend on where the files sit.
SELF_FILES = {"leakcheck.config", "leakcheck.baseline"}


def is_skipped(path: str, skip_globs) -> bool:
    norm = path.replace(os.sep, "/")
    if os.path.basename(norm) in SELF_FILES:
        return True
    for glob in skip_globs:
        if fnmatch.fnmatch(norm, glob) or norm.endswith("/" + glob):
            return True
    return False


def scan_diff(diff_text: str, patterns: list, skip_globs) -> list:
    """Scan only the added lines of a unified diff.

    Line numbers come from the hunk headers, so a finding points at the line it
    will have in the committed file.
    """
    findings = []
    path = None
    lineno = 0
    skipping = False
    saw_minus_header = False
    hunk_re = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")

    for raw in diff_text.splitlines():
        if raw.startswith("--- ") and (raw[4:].strip() == "/dev/null" or raw[4:].strip().startswith("a/")):
            saw_minus_header = True
            continue
        if saw_minus_header and raw.startswith("+++ "):
            saw_minus_header = False
            target = raw[4:].strip()
            if target == "/dev/null":
                path = None
                skipping = True
                continue
            path = target[2:] if target.startswith("b/") else target
            skipping = is_skipped(path, skip_globs)
            continue
        saw_minus_header = False
        if raw.startswith("diff --git"):
            continue
        head = hunk_re.match(raw)
        if head:
            lineno = int(head.group(1))
            continue
        if raw.startswith("+"):
            if path and not skipping:
                findings.extend(scan_line(path, lineno, raw[1:], patterns))
            lineno += 1
        elif raw.startswith(" "):
            lineno += 1
    return findings


# ---------------------------------------------------------------------------
# Baseline
# ---------------------------------------------------------------------------
def load_baseline(path: str) -> set:
    keys = set()
    if not os.path.exists(path):
        return keys
    with open(path, "r", encoding="utf-8") as fh:
        for raw in fh:
            line = raw.rstrip("\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            parts = line.split("\t")
            if len(parts) != 3:
                fail("bad baseline line in %s: %r" % (path, line))
            keys.add((parts[0], parts[1], parts[2]))
    return keys


def write_baseline(path: str, keys) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(BASELINE_HEADER.format(version=LEAKCHECK_VERSION))
        for key in sorted(keys):
            fh.write("%s\t%s\t%s\n" % key)


# ---------------------------------------------------------------------------
# git helpers
# ---------------------------------------------------------------------------
def git(*args) -> str:
    proc = subprocess.run(
        ["git"] + list(args), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True
    )
    if proc.returncode != 0:
        fail("git %s failed: %s" % (" ".join(args), proc.stderr.strip()))
    return proc.stdout


def repo_root() -> str:
    return git("rev-parse", "--show-toplevel").strip()


def tracked_files() -> list:
    out = git("ls-files", "-z")
    return [p for p in out.split("\0") if p]


def fail(message: str):
    sys.stderr.write("leakcheck: %s\n" % message)
    sys.exit(2)


# ---------------------------------------------------------------------------
# Reporting
# ---------------------------------------------------------------------------
def report(findings, stream) -> None:
    # One line per (file, line, kind, token). A token that repeats on a line is
    # one problem, not several.
    seen = set()
    for finding in sorted(findings, key=lambda f: (f.path, f.line, f.kind, f.token)):
        mark = (finding.path, finding.line, finding.kind, finding.token)
        if mark in seen:
            continue
        seen.add(mark)
        stream.write(
            "  %s:%d  [%s] %s\n      %s\n"
            % (finding.path, finding.line, finding.kind, finding.token, finding.snippet)
        )


def main(argv) -> int:
    parser = argparse.ArgumentParser(prog="leakcheck", add_help=True)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--staged", action="store_true", help="scan added lines in the staged diff")
    mode.add_argument("--range", dest="rng", metavar="A..B", help="scan added lines in a commit range")
    mode.add_argument("--paths", nargs="+", metavar="FILE", help="scan the named files")
    parser.add_argument("--update", action="store_true", help="rewrite the baseline from a full tree scan")
    parser.add_argument("--no-baseline", action="store_true", help="report every finding, baseline ignored")
    parser.add_argument("--json", action="store_true", help="emit findings as JSON")
    parser.add_argument("--version", action="store_true", help="print the checker version")
    args = parser.parse_args(argv)

    if args.version:
        print("leakcheck %s" % LEAKCHECK_VERSION)
        return 0

    here = os.path.dirname(os.path.abspath(__file__))
    config_path = os.path.join(here, "leakcheck.config")
    baseline_path = os.path.join(here, "leakcheck.baseline")

    cfg = load_config(config_path)
    patterns = build_identifier_patterns(cfg)

    skip_globs = list(cfg["exclude"])

    if args.paths:
        findings = scan_files(args.paths, patterns, skip_globs)
    else:
        root = repo_root()
        os.chdir(root)
        if args.staged:
            findings = scan_diff(git("diff", "--cached", *DIFF_ARGS), patterns, skip_globs)
        elif args.rng:
            findings = scan_diff(git("diff", *(DIFF_ARGS + (args.rng,))), patterns, skip_globs)
        else:
            findings = scan_files(tracked_files(), patterns, skip_globs)

    credentials = [f for f in findings if f.kind == "credential"]
    internal = [f for f in findings if f.kind != "credential"]

    if args.update:
        if args.staged or args.rng or args.paths:
            fail("--update only works on a full tree scan")
        if credentials:
            sys.stderr.write("leakcheck: credential-shaped content found, refusing to update the baseline\n")
            report(credentials, sys.stderr)
            return 1
        write_baseline(baseline_path, {f.key for f in internal})
        print("leakcheck: baseline rewritten with %d entries (%s)" % (len({f.key for f in internal}), baseline_path))
        return 0

    baseline = set() if args.no_baseline else load_baseline(baseline_path)
    new_internal = [f for f in internal if f.key not in baseline]

    if args.json:
        import json

        print(
            json.dumps(
                [
                    {"path": f.path, "line": f.line, "kind": f.kind, "token": f.token}
                    for f in credentials + new_internal
                ],
                indent=2,
            )
        )
        return 1 if (credentials or new_internal) else 0

    if credentials:
        sys.stderr.write("\nleakcheck FAILED: credential-shaped content (never baselineable)\n")
        report(credentials, sys.stderr)

    if new_internal:
        sys.stderr.write("\nleakcheck FAILED: internal identifiers that are not in the baseline\n")
        report(new_internal, sys.stderr)
        sys.stderr.write(
            "\nRemove these before committing. This is a PUBLIC repo.\n"
            "If a hit is a false positive, fix the checker (scripts/leakcheck.py).\n"
            "Do not hand-add a line to scripts/leakcheck.baseline to get past this.\n"
        )

    if credentials or new_internal:
        return 1

    if not (args.staged or args.rng or args.paths or args.no_baseline):
        found = {f.key for f in internal}
        stale = baseline - found
        if stale:
            print(
                "leakcheck: clean (%d baselined). %d baseline entries no longer exist, "
                "prune them with `make leakcheck-update`." % (len(found), len(stale))
            )
            return 0
    print("leakcheck: clean (leakcheck %s)" % LEAKCHECK_VERSION)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
