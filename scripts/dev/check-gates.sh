#!/usr/bin/env bash
# check-gates.sh — keep docs/delivery/GATES.md and the test runners honest (unit maintainability).
#  1. every mode in test-local.sh's usage line has a row in GATES.md, and every mode GATES.md names
#     exists (no undocumented gate, no stale row);
#  2. every tracked *.spec.*|*.test.* file (git ls-files, whole repo) is run by some gate: its file name
#     appears in scripts/dev/test-local.sh or tests/foundation/*.go, it matches a glob written in
#     scripts/dev/test-node.sh (the Node unit gate that CI runs), or it belongs to a playwright.config.ts
#     suite whose name a tests/foundation/*.go file selects (LC_BROWSER_SUITE);
#  3. CI (.github/workflows/foundation.yml) actually invokes scripts/dev/test-node.sh, so a Node suite is
#     never "covered" by a script no workflow runs.
# Usage: bash scripts/dev/check-gates.sh   (exit 1 on any finding)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
python3 - <<'PY'
import fnmatch, glob, os, re, subprocess, sys
bad = []
sh = open("scripts/dev/test-local.sh").read()
gates = open("docs/delivery/GATES.md").read()
usage = re.search(r"Usage: bash scripts/dev/test-local\.sh \[(.*?)\]", sh)
if not usage:
    sys.exit("check-gates: no usage line in test-local.sh")
modes = set(usage.group(1).split("|"))
rows = set(re.findall(r"^\| `(--[a-z0-9-]+)` \|", gates, re.M))
bad += [f"mode {m} is in test-local.sh usage but has no GATES.md row" for m in sorted(modes - rows)]
bad += [f"GATES.md row {m} names a mode test-local.sh does not accept" for m in sorted(rows - modes)]
go = "".join(open(f).read() for f in glob.glob("tests/foundation/*.go"))
config = open("playwright.config.ts").read()
suites = {}  # suite name -> spec files
for name, body in re.findall(r'"?([a-z-]+)"?:\s*\[([^\]]*)\]', config):
    suites[name] = re.findall(r'"([^"]+\.(?:spec|test)\.ts)"', body)
selected = {s for s in suites if f'"{s}"' in go}
via_suite = {f for s in selected for f in suites[s]}
node_gate = open("scripts/dev/test-node.sh").read()
globs = re.findall(r"[\w./*-]+\.(?:test|spec)\.\w+", node_gate)  # literal paths and dir/*.test.ext globs
tracked = subprocess.run(["git", "ls-files", "*.spec.*", "*.test.*"], capture_output=True, text=True, check=True).stdout.split()
for path in sorted(tracked):
    name = os.path.basename(path)
    if not (name in sh or name in go or name in via_suite or any(fnmatch.fnmatch(path, g) for g in globs)):
        bad.append(f"{path} is run by no gate (test-local.sh, tests/foundation, test-node.sh, or a selected playwright suite)")
if "scripts/dev/test-node.sh" not in open(".github/workflows/foundation.yml").read():
    bad.append("foundation.yml does not run scripts/dev/test-node.sh (Node unit suites would be gated by nothing)")
if bad:
    print("\n".join("check-gates: " + b for b in bad), file=sys.stderr)
    sys.exit(1)
print(f"check-gates: ok ({len(modes)} modes, all documented; every tracked test file is run)")
PY
