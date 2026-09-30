#!/usr/bin/env bash
# File: scripts/dev/release-gate.sh
# Purpose: the R1 acceptance command (docs/delivery/PROCESS.md §1, R1-8). Runs every automated tier
#   in order and prints ONE table of PASS / FAIL / NOT_RUN. A tier whose prerequisites are missing is
#   NOT_RUN, never PASS; a test that skips with a `NOT_RUN:` message is listed as NOT_RUN, any other
#   skip is FAIL; zero tests, a missing log or a non-zero exit is FAIL (R1 ruling F5). Go results are
#   read as `go test -json` events (G06 natively, the `-v` logs of test-local.sh via `go tool test2json`).
#   The overall line says PASS only when every row is PASS or an accepted NOT_RUN ("(accepted: <reason>)");
#   any other NOT_RUN is labelled "UNACCEPTED:" and keeps the overall INCOMPLETE.
# Order: G01 check_packet, G02 build/vet/gofmt, G03 TypeScript typecheck, G04 secret grep,
#   G05 dependency map, G06 unit tests (all packages but tests/foundation), G06n Node unit suites
#   (scripts/dev/test-node.sh, ruling F9), G07 the whole foundation
#   package (real PG, race, vet: test-local.sh), then EVERY browser mode listed in test-local.sh's
#   usage line (names containing "browser", plus --browser-e2e; --browser-webkit is its own B-browser-webkit row on Playwright
#   WebKit, NOT_RUN when WebKit is not installed), SANDBOX modes only with the Stripe
#   TEST key present, then G90 deploy smoke static, G91 deploy smoke full and G99 (no gate rewrote a
#   tracked .impeccable file, ruling F7; full runs only).
# Usage: bash scripts/dev/release-gate.sh [--strict] [--list] [--only ID[,ID...]]
#   --strict  exit 3 when there is no FAIL but at least one UNACCEPTED NOT_RUN (use for release acceptance);
#             NOT_RUN rows listed in the accepted catalogue below (R1 rulings F11/F12/G3/G4) do not count
#   --list    print the step ids and exit
#   --only    run just these ids (e.g. G01,G04,B-browser-buyer); the table then shows only them
#   Exit: 0 no FAIL (and, with --strict, no NOT_RUN), 1 any FAIL, 2 usage, 3 --strict and NOT_RUN.
# Runs as/in: a developer machine or CI runner with go, python3; docker/node/pnpm/Chromium for the
#   PG and browser tiers; Linux + root + free 80/443 for `smoke.sh full` (G91: NOT_RUN unless
#   LC_RELEASE_GATE_SMOKE_FULL=1). Never needs production access and never contacts a live provider.
# Reads env: STRIPE_BROWSER=1 and STRIPE_SANDBOX=1 (opt in to SANDBOX modes), LC_SECRETS_FILE
#   (default ~/.config/livecommerce/secrets.env; only the presence and the sk_test_/rk_test_ prefix
#   of STRIPE_SECRET_KEY are checked, in a subshell, never printed), LC_RELEASE_GATE_OUT (evidence
#   dir), LC_RELEASE_GATE_SMOKE_FULL, GOTOOLCHAIN (default go1.27.1).
# Reads secrets: none printed. Logs may hold what the tests print; the secret grep (G04) and the CI
#   rule keep key-shaped literals out of the repo, and no command here echoes an environment.
# Used by: the integrator before merging a release branch; docs/delivery/PROCESS.md §1 R1-8.
# Depends on: scripts/check_packet.py, scripts/dev/{depmap,test-local}.sh, deploy/scripts/smoke.sh,
#   .github/workflows/foundation.yml (the G04 pattern is a copy; G04 fails if the two diverge),
#   docs/delivery/GATES.md when present (every selected mode must be listed there).
# Status: MODEL of the gate is exercised by `--list` and by a run with prerequisites removed; the
#   PASS rows are only as real as the underlying suites (see each row's log).
# Evidence: $LC_RELEASE_GATE_OUT (default <main checkout>/output/release-gate/<UTC>-<sha12>/):
#   results.tsv (id, tier, status, note, exit code, log), summary.txt (the table, commit, dirty flag),
#   one <id>.log per step with the command and its exit code. Worktrees are deleted after merge, so the
#   default is the MAIN checkout's output/ (PROCESS.md §4).
# Change rules: a new gate mode in test-local.sh needs no change here (parsed from its usage line) but
#   must be added to docs/delivery/GATES.md; never map a SKIP or an empty run to PASS; keep bash 3.2
#   compatible (macOS): no associative arrays, no mapfile.
set -uo pipefail

strict=0 list=0 only=""
while [[ $# -gt 0 ]]; do
  case "$1" in
  --strict) strict=1 ;;
  --list) list=1 ;;
  --only)
    shift
    only=",${1:-},"
    ;;
  *)
    echo "usage: release-gate.sh [--strict] [--list] [--only ID[,ID...]]" >&2
    exit 2
    ;;
  esac
  shift
done

cd "$(dirname "$0")/../.." || exit 2
ROOT=$(pwd)
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.1}"
sha=$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
dirty=$(git status --porcelain 2>/dev/null | grep -v '^?? output/' | wc -l | tr -d ' ')
MAIN=$(cd "$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/.." 2>/dev/null && pwd || echo "$ROOT")
OUT="${LC_RELEASE_GATE_OUT:-$MAIN/output/release-gate/$(date -u +%Y%m%dT%H%M%SZ)-$sha}"

have() { command -v "$1" >/dev/null 2>&1; }
# SANDBOX opt-in is read once and then removed from the environment: only the SANDBOX rows (G07z and
# --stripe-browser) get it back, so no other suite turns into a half-configured SANDBOX run.
stripe_opt_in=0
[[ "${STRIPE_BROWSER:-}" == 1 && "${STRIPE_SANDBOX:-}" == 1 ]] && stripe_opt_in=1
unset STRIPE_BROWSER STRIPE_SANDBOX STRIPE_SECRET_KEY STRIPE_SECRET_KEY_ROTATED STRIPE_ACCOUNT_ID
selected() { [[ -z "$only" || "$only" == *",$1,"* ]]; }

# ---- mode discovery: the usage line of test-local.sh is the single list of modes ------------------
modes=$(sed -n "s/.*Usage: bash scripts\/dev\/test-local.sh \[\(.*\)\]\\\\n'.*/\1/p" scripts/dev/test-local.sh | tr '|' '\n')
browser_modes=$(printf '%s\n' "$modes" | grep -E -- '^--(.*browser.*|e2e)$' || true)
all_modes=$(printf '%s\n' "$modes" | grep -c . | tr -d ' ')
ids="G01 G02 G03 G04 G05 G06 G06n G07 G07z"
for m in $browser_modes; do ids="$ids B-${m#--}"; done
ids="$ids G90 G91 G99"

if ((list)); then
  for id in $ids; do echo "$id"; done
  exit 0
fi
if [[ -z "$browser_modes" ]]; then
  echo "release-gate: cannot parse the usage line of scripts/dev/test-local.sh (no browser modes found)" >&2
  exit 2
fi

mkdir -p "$OUT" || exit 2
impeccable_before=$(git status --porcelain --untracked-files=no -- .impeccable 2>/dev/null)
: >"$OUT/results.tsv"
rows=""
n_pass=0 n_fail=0 n_notrun=0 n_accepted=0

# ---- accepted NOT_RUN catalogue (R1 rulings F11, F12, G3, G4: docs/delivery/units/r1-final-rulings.md) --
# A NOT_RUN row is "accepted" only when EVERY item it names matches an entry below; its note is then
# prefixed "(accepted: <reason>)". Any other NOT_RUN is prefixed "UNACCEPTED:" and counted as open.
# Items: the lines of a *.skipped list (go test names), the "not run: S01,S34" ids of a smoke row, or
# else the note itself. First match wins; keep specific entries above broad ones.
accepted_notrun() { # $1 id, $2 note, $3 log/list file -> prints the reason(s), or nothing
  python3 - "$1" "$2" "$3" <<'PY'
import fnmatch, re, sys
rid, note, path = sys.argv[1:4]
catalogue = [  # (row id glob, item glob, reason)
    ("*", "*TestStripeSP16Sandbox/expires_at_29min*", "G4 SP16 29-min expiry probe is developer-only"),
    ("*", "*TestStripeRF10Sandbox/rotated_key*", "G4 rotated-key replay needs a second Stripe test key"),
    ("*", "*populated_upgrade*", "G4 populated-DB upgrade test: R1 first deploy is a fresh DB"),
    ("*", "*TestMetaClaimsMCI11LiveReadOnlyProbes*", "G4 Meta LIVE probes need the owner's Page token"),
    ("*", "*TestBrowserRefund*SANDBOX*", "G4 RF11(b) covered by RF10 via the API (G07z)"),
    ("*", "*TestBrowserE2EDealLoopSandbox*", "F12 T12 SANDBOX tier covered by SP18"),
    ("B-stripe-browser+", "*SP17*", "G4 SP17 real webhook delivery: Dashboard test event after deploy"),
    ("G06s", "*TestStripe*Sandbox", "runs in G07z with the Stripe test key"),
    ("G07+", "TestStripe*Sandbox", "runs in G07z with the Stripe test key"),
    ("G07+", "TestStripeSP21Registrar/sandbox_probe*", "runs in G07z with the Stripe test key"),
    ("G06n+", "*r04-input-runner*", "F11 live media (LiveKit) is not in R1"),
    ("G90", "S01", "G3 shellcheck runs in CI job deploy-smoke"),
    ("G91", "*", "G3 smoke full runs in CI job deploy-smoke"),
]
items = []
if path.endswith(".skipped"):
    try:
        items = [l.strip() for l in open(path) if l.strip()]
    except OSError:
        items = []
if not items:
    m = re.search(r"not run: ([A-Za-z0-9,]+)", note)
    items = m.group(1).split(",") if m else [note]
reasons = []
for item in items:
    hit = next((r for g, pat, r in catalogue if fnmatch.fnmatchcase(rid, g) and fnmatch.fnmatchcase(item, pat)), None)
    if hit is None:
        sys.exit(0)  # one unaccepted item keeps the whole row open
    if hit not in reasons:
        reasons.append(hit)
print("; ".join(reasons))
PY
}

# record ID TIER STATUS NOTE [EXIT] [LOG]
record() {
  local id=$1 tier=$2 status=$3 note=$4 code=${5:--} log=${6:--} why
  if [[ "$status" == NOT_RUN ]]; then
    why=$(accepted_notrun "$id" "$note" "$log")
    if [[ -n "$why" ]]; then note="(accepted: $why) $note"; n_accepted=$((n_accepted + 1)); else note="UNACCEPTED: $note"; fi
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$tier" "$status" "$note" "$code" "$log" >>"$OUT/results.tsv"
  rows="$rows$(printf '%-24s %-10s %-8s %s' "$id" "$tier" "$status" "$note")"$'\n'
  case "$status" in PASS) n_pass=$((n_pass + 1)) ;; FAIL) n_fail=$((n_fail + 1)) ;; *) n_notrun=$((n_notrun + 1)) ;; esac
  printf '%-24s %-10s %-8s %s\n' "$id" "$tier" "$status" "$note" >&2
}

# run_cmd ID CMD... : run CMD in $ROOT with combined output in $OUT/ID.log; sets rc and LOG.
run_cmd() {
  local id=$1
  shift
  LOG="$OUT/$id.log"
  {
    printf '# command: %s\n# commit: %s dirty_files: %s\n# started: %s\n' "$*" "$sha" "$dirty" "$(date -u +%FT%TZ)"
  } >"$LOG"
  "$@" >>"$LOG" 2>&1
  rc=$?
  printf '# exit: %s\n' "$rc" >>"$LOG"
}

docker_ok() { have docker && docker info >/dev/null 2>&1; }
# Chromium of the repo's pinned Playwright must exist (a missing browser is a missing prerequisite).
browser_ok() {
  have node && have pnpm && [[ -d node_modules/@playwright ]] &&
    node --input-type=module -e "import fs from 'node:fs'; import('@playwright/test').then(m=>process.exit(fs.existsSync(m.chromium.executablePath())?0:1)).catch(()=>process.exit(1))" >/dev/null 2>&1
}

# ---- G01 architecture packet --------------------------------------------------------------------
if selected G01; then
  if ! have python3; then
    record G01 STATIC NOT_RUN "python3 missing"
  else
    # check_packet.py always rewrites a tracked timestamp file; put it back so a gate run never dirties the tree.
    pc=experiments/results/packet-check.json
    cp "$pc" "$OUT/packet-check.before.json" 2>/dev/null
    run_cmd G01 python3 scripts/check_packet.py
    cp "$OUT/packet-check.before.json" "$pc" 2>/dev/null
    if ((rc == 0)); then record G01 STATIC PASS "check_packet.py exit 0" 0 "$LOG"; else record G01 STATIC FAIL "check_packet.py exit $rc" "$rc" "$LOG"; fi
  fi
fi

# ---- G02 build, vet, gofmt ----------------------------------------------------------------------
if selected G02; then
  if ! have go; then
    record G02 STATIC NOT_RUN "go missing"
  else
    run_cmd G02 bash -c 'go build ./... && go vet ./... && { u=$(gofmt -l $(git ls-files "*.go")); [ -z "$u" ] || { echo "gofmt needs to run on:"; echo "$u"; exit 1; }; }'
    if ((rc == 0)); then record G02 STATIC PASS "go build + vet + gofmt clean" 0 "$LOG"; else record G02 STATIC FAIL "build/vet/gofmt exit $rc" "$rc" "$LOG"; fi
  fi
fi

# ---- G03 TypeScript strict typecheck ------------------------------------------------------------
if selected G03; then
  if ! have pnpm || [[ ! -d node_modules ]]; then
    record G03 STATIC NOT_RUN "pnpm or node_modules missing (pnpm install --frozen-lockfile)"
  else
    run_cmd G03 bash -c 'pnpm run typecheck:i18n && pnpm run typecheck:admin && pnpm run typecheck:storefront'
    if ((rc == 0)); then record G03 STATIC PASS "tsc strict: i18n, admin, storefront" 0 "$LOG"; else record G03 STATIC FAIL "typecheck exit $rc" "$rc" "$LOG"; fi
  fi
fi

# ---- G04 key-shaped secret literals (copy of the CI step; fails if the CI pattern changes) ---------
SECRET_RE='(sk|rk|pk)_(test|live)_[A-Za-z0-9]{8,}|whsec_[A-Za-z0-9]{8,}|EAA[A-Za-z0-9]{40,}|postgres(ql)?://[A-Za-z0-9_.-]+:[^@"$ {%]+@'
if selected G04; then
  LOG="$OUT/G04.log"
  if ! have git; then
    record G04 STATIC NOT_RUN "git missing"
  elif ! grep -qF -- "$SECRET_RE" .github/workflows/foundation.yml 2>/dev/null; then
    printf 'the pattern in release-gate.sh no longer matches .github/workflows/foundation.yml\n' >"$LOG"
    record G04 STATIC FAIL "secret pattern drifted from the CI step: update both" 1 "$LOG"
  else
    # -c prints counts only, so a hit never puts the matching text into the gate output or log.
    git grep -cE "$SECRET_RE" -- ':!*.md' ':!pnpm-lock.yaml' ':!tests/payments/stripe-webhook-vectors.json' >"$LOG" 2>&1
    if [[ $? -eq 1 ]]; then
      record G04 STATIC PASS "no key-shaped literal (sk_/rk_/pk_/whsec_/EAA/inline-password DSN)" 0 "$LOG"
    else
      record G04 STATIC FAIL "key-shaped literal in the files listed in the log (counts per file)" 1 "$LOG"
    fi
  fi
fi

# ---- G05 dependency map is current ---------------------------------------------------------------
if selected G05; then
  if ! have go || ! have python3; then
    record G05 STATIC NOT_RUN "go or python3 missing"
  else
    run_cmd G05 bash scripts/dev/depmap.sh --check
    if ((rc == 0)); then record G05 STATIC PASS "dependency-map.md current" 0 "$LOG"; else record G05 STATIC FAIL "dependency map stale (bash scripts/dev/depmap.sh)" "$rc" "$LOG"; fi
  fi
fi

# ---- go test -json summariser: prints "pass fail skip" and writes skipped names to $2 -------------
summarise_json() { # $1 = jsonl log, $2 = file for skipped test names
  python3 - "$1" "$2" <<'PY'
import json, sys
p = f = s = 0
skipped = []
for line in open(sys.argv[1], errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        e = json.loads(line)
    except ValueError:
        continue
    a, t = e.get("Action"), e.get("Test")
    if not t:
        if a == "fail":
            f += 1  # package-level failure (build error, panic, timeout)
        continue
    if a == "pass":
        p += 1
    elif a == "fail":
        f += 1
    elif a == "skip":
        s += 1
        skipped.append(e.get("Package", "?").rsplit("/", 1)[-1] + "." + t)
open(sys.argv[2], "w").write("\n".join(skipped))
print(p, f, s)
PY
}

# ---- G06 unit tests (everything but tests/foundation, which needs PG) ----------------------------
if selected G06; then
  if ! have go || ! have python3; then
    record G06 UNIT NOT_RUN "go or python3 missing"
  else
    LOG="$OUT/G06.jsonl"
    pkgs=$(go list ./... 2>/dev/null | grep -v '/tests/foundation$')
    # shellcheck disable=SC2086
    go test -count=1 -json $pkgs >"$LOG" 2>"$OUT/G06.stderr"
    rc=$?
    read -r up uf us <<<"$(summarise_json "$LOG" "$OUT/G06.skipped")"
    if ((rc != 0 || uf > 0)); then
      record G06 UNIT FAIL "go test exit $rc, pass=$up fail=$uf skip=$us" "$rc" "$LOG"
    elif ((up == 0)); then
      record G06 UNIT FAIL "no test ran (pass=0): an empty run is never PASS" "$rc" "$LOG"
    else
      record G06 UNIT PASS "$up tests pass, 0 fail (packages except tests/foundation)" "$rc" "$LOG"
    fi
    # G06s (the skips) is recorded after G07: a skip here is covered only by a PASS of that test in G07.
  fi
fi

# ---- Node unit suites (ruling F9: GATES.md "Node unit suites"; run by CI too) ------------------------
if selected G06n; then
  if ! have node || [[ ! -d node_modules ]]; then
    record G06n UNIT NOT_RUN "node or node_modules missing"
  else
    run_cmd G06n bash scripts/dev/test-node.sh
    np=$(sed -n 's/^ℹ pass \([0-9]*\)$/\1/p' "$LOG" | awk '{s+=$1} END {print s+0}')
    nf=$(sed -n 's/^ℹ fail \([0-9]*\)$/\1/p' "$LOG" | awk '{s+=$1} END {print s+0}')
    if ((rc != 0 || nf > 0)); then
      record G06n UNIT FAIL "test-node.sh exit $rc, pass=$np fail=$nf" "$rc" "$LOG"
    elif ((np == 0)); then
      record G06n UNIT FAIL "no Node test ran (pass=0): an empty run is never PASS" "$rc" "$LOG"
    else
      record G06n UNIT PASS "$np Node tests pass (storefront, i18n)" "$rc" "$LOG"
      if grep -q '^NOT_RUN:' "$LOG"; then record G06n+ UNIT NOT_RUN "$(grep -m1 '^NOT_RUN:' "$LOG" | cut -c10-120)" - "$LOG"; fi
    fi
  fi
fi

# ---- judge a `go test -v` log as go test -json events (ruling F5) ----------------------------------
# `go tool test2json` turns the -v text of test-local.sh into the -json event stream. Counts every test
# and subtest. A skip is NOT_RUN only when the skipped test's own output has a `<file>.go:<n>: NOT_RUN:`
# line (the tests' statement that a SANDBOX/LIVE/migrator prerequisite is absent); any other skip is
# "other" and fails the row. Prints "pass fail notrun other"; names go to $2 (NOT_RUN) and $3 (other).
judge_gotest() { # $1 = go test -v log
  python3 - "$1" "$2" "$3" <<'PY'
import json, re, subprocess, sys
try:
    raw = open(sys.argv[1], "rb").read()
    out = subprocess.run(["go", "tool", "test2json"], input=raw, capture_output=True, check=True).stdout
except Exception:
    print(0, 1, 0, 0)  # unreadable log or no converter: never PASS
    sys.exit(0)
p = f = 0
msgs, skipped = {}, []
for line in out.decode(errors="replace").splitlines():
    try:
        e = json.loads(line)
    except ValueError:
        continue
    a, t = e.get("Action"), e.get("Test")
    if not t:
        continue
    if a == "output":
        msgs[t] = msgs.get(t, "") + e.get("Output", "")
    elif a == "pass":
        p += 1
    elif a == "fail":
        f += 1
    elif a == "skip":
        skipped.append(t)
nr = [t for t in skipped if re.search(r"^\s*\S+\.go:\d+: NOT_RUN:", msgs.get(t, ""), re.M)]
other = [t for t in skipped if t not in nr]
open(sys.argv[2], "w").write("\n".join(nr))
open(sys.argv[3], "w").write("\n".join(other))
print(p, f, len(nr), len(other))
PY
}

# ---- G07 whole foundation package: real PostgreSQL, race detector, vet ----------------------------
if selected G07; then
  if ! docker_ok || ! have go || ! have node || ! have python3; then
    record G07 REAL_PG NOT_RUN "docker daemon, go, node or python3 missing"
  else
    run_cmd G07 bash scripts/dev/test-local.sh
    if ((rc != 0)); then
      record G07 REAL_PG FAIL "test-local.sh exit $rc" "$rc" "$LOG"
    else
      read -r gp gf sn so <<<"$(judge_gotest "$LOG" "$OUT/G07.skipped" "$OUT/G07.skipped-other")"
      if ((gf > 0 || gp == 0)); then
        record G07 REAL_PG FAIL "exit 0 but go test events pass=$gp fail=$gf: zero tests or a failure is never PASS" "$rc" "$LOG"
      elif ((so > 0)); then
        record G07 REAL_PG FAIL "$so test(s) skipped without a NOT_RUN: message (SKIP is never PASS): $(tr '\n' ' ' <"$OUT/G07.skipped-other" | cut -c1-140)" "$rc" "$LOG"
      else
        record G07 REAL_PG PASS "$gp tests/subtests pass, 0 fail (go test -race ./... + go vet)" "$rc" "$LOG"
        if ((sn > 0)); then
          record G07+ REAL_PG NOT_RUN "$sn skipped with NOT_RUN: (SANDBOX/LIVE/migrator prerequisites): $(tr '\n' ' ' <"$OUT/G07.skipped" | cut -c1-140)" - "$OUT/G07.skipped"
        fi
      fi
    fi
  fi
fi

# ---- G06s: unit-run skips (real-PG tests of non-foundation packages lack the PG env in G06) ---------------
# A G06 skip ("pkg.Test") is covered only when the SAME test PASSed in this run's G07 (go test -p 1 -v ./...
# prints each package's results before its "ok <import path>" line). Uncovered skips stay NOT_RUN and go
# through the accepted catalogue; covered ones are listed as the G06r PASS row (they ran, against real PG).
if selected G06 && selected G07 && [[ -s "$OUT/G06.skipped" ]]; then
  python3 - "$OUT/G06.skipped" "$OUT/G07.log" "$OUT/G06s.covered" "$OUT/G06s.skipped" <<'PY'
import re, sys
skipped = [l.strip() for l in open(sys.argv[1]) if l.strip()]
passed, pending = set(), []
try:
    for line in open(sys.argv[2], errors="replace"):
        m = re.match(r"\s*--- PASS: (\S+) \(", line)
        if m:
            pending.append(m.group(1))
            continue
        m = re.match(r"(ok|FAIL)\s+(\S+)", line)
        if m:
            if m.group(1) == "ok":
                passed.update(m.group(2).rsplit("/", 1)[-1] + "." + t for t in pending)
            pending = []
except OSError:
    pass
open(sys.argv[3], "w").write("\n".join(t for t in skipped if t in passed))
open(sys.argv[4], "w").write("\n".join(t for t in skipped if t not in passed))
PY
elif [[ -s "$OUT/G06.skipped" ]]; then
  cp "$OUT/G06.skipped" "$OUT/G06s.skipped" && : >"$OUT/G06s.covered"
fi
if [[ -s "$OUT/G06s.covered" ]]; then
  record G06r UNIT PASS "$(grep -c . "$OUT/G06s.covered") G06-skipped test(s) PASSED in G07 (real PG): $(tr '\n' ' ' <"$OUT/G06s.covered" | cut -c1-140)" - "$OUT/G06s.covered"
fi
if [[ -s "$OUT/G06s.skipped" ]]; then
  record G06s UNIT NOT_RUN "$(grep -c . "$OUT/G06s.skipped") skipped in G06 and not passed in G07: $(tr '\n' ' ' <"$OUT/G06s.skipped")" - "$OUT/G06s.skipped"
fi

# ---- stripe SANDBOX prerequisite (no value is printed) ---------------------------------------------
stripe_sandbox_ready() {
  ((stripe_opt_in)) || return 1
  local f="${LC_SECRETS_FILE:-$HOME/.config/livecommerce/secrets.env}"
  [[ -r "$f" ]] || return 1
  ( # subshell: the key never leaves it
    set +x
    # shellcheck disable=SC1090
    . "$f" 2>/dev/null
    [[ "${STRIPE_SECRET_KEY:-}" =~ ^(sk|rk)_test_ ]]
  )
}

# ---- G07z Stripe SANDBOX API tiers with the owner's test key (SP16 unit, RF10 + SP21 real PG) ---------
# The key is read in a subshell from the secrets file and exported to these go test processes only.
if selected G07z; then
  if ! stripe_sandbox_ready; then
    record G07z SANDBOX NOT_RUN "needs STRIPE_BROWSER=1, STRIPE_SANDBOX=1 and an sk_test_/rk_test_ key in secrets.env"
  elif ! docker_ok || ! have go; then
    record G07z SANDBOX NOT_RUN "docker daemon or go missing"
  else
    run_cmd G07z bash -c 'set +x
      STRIPE_SECRET_KEY="$(. "${LC_SECRETS_FILE:-$HOME/.config/livecommerce/secrets.env}" 2>/dev/null; printf %s "${STRIPE_SECRET_KEY:-}")"
      export STRIPE_SECRET_KEY STRIPE_SANDBOX=1 STRIPE_ACCOUNT_ID=acct_1UJDb0RusP6Wwj7e
      go test -count=1 -v -run "^TestStripeSP16Sandbox$" ./internal/integrations/psp/stripe || exit 1
      bash scripts/dev/test-focused.sh "^(TestStripeRF10Sandbox|TestStripeSP21Registrar)$"'
    if ((rc != 0)); then
      record G07z SANDBOX FAIL "SP16/RF10/SP21 SANDBOX exit $rc" "$rc" "$LOG"
    else
      read -r gp gf sn so <<<"$(judge_gotest "$LOG" "$OUT/G07z.skipped" "$OUT/G07z.skipped-other")"
      if ((gf > 0 || gp == 0 || so > 0)); then
        record G07z SANDBOX FAIL "pass=$gp fail=$gf other-skips=$so" "$rc" "$LOG"
      else
        record G07z SANDBOX PASS "$gp SANDBOX tests/subtests pass against Stripe test mode (SP16, RF10, SP21 probe)" "$rc" "$LOG"
        if ((sn > 0)); then record G07z+ SANDBOX NOT_RUN "skipped with NOT_RUN: $(tr '\n' ' ' <"$OUT/G07z.skipped" | cut -c1-100)" - "$OUT/G07z.skipped"; fi
      fi
    fi
  fi
fi

# ---- browser modes ------------------------------------------------------------------------------
gates_doc=docs/delivery/GATES.md
for m in $browser_modes; do
  id="B-${m#--}"
  selected "$id" || continue
  tier=BROWSER
  [[ "$m" == --stripe-browser ]] && tier=SANDBOX
  if [[ -f "$gates_doc" ]] && ! grep -qF -- "$m" "$gates_doc"; then
    record "$id" "$tier" FAIL "mode $m is not listed in $gates_doc" 1 -
    continue
  fi
  if [[ "$m" == --stripe-browser ]] && ! stripe_sandbox_ready; then
    record "$id" SANDBOX NOT_RUN "needs STRIPE_BROWSER=1, STRIPE_SANDBOX=1 and an sk_test_/rk_test_ key in secrets.env"
    continue
  fi
  if ! docker_ok || ! have go; then
    record "$id" "$tier" NOT_RUN "docker daemon or go missing"
    continue
  fi
  # --live-browser-input is a Go/PG gate (no Chromium); every other mode drives a real browser.
  if [[ "$m" != --live-browser-input ]] && ! browser_ok; then
    record "$id" "$tier" NOT_RUN "node/pnpm, @playwright/test or its Chromium missing (pnpm install; pnpm exec playwright install chromium)"
    continue
  fi
  if [[ "$m" == --stripe-browser ]]; then
    run_cmd "$id" env STRIPE_BROWSER=1 STRIPE_SANDBOX=1 bash scripts/dev/test-local.sh "$m"
  else
    run_cmd "$id" bash scripts/dev/test-local.sh "$m"
  fi
  if grep -q '^NOT_RUN' "$LOG" && ((rc == 2)); then
    record "$id" "$tier" NOT_RUN "$(grep -m1 '^NOT_RUN' "$LOG" | cut -c1-110)" "$rc" "$LOG"
  elif ((rc != 0)); then
    record "$id" "$tier" FAIL "test-local.sh $m exit $rc" "$rc" "$LOG"
  elif [[ "$m" == --stripe-browser ]]; then
    # test-local.sh parses its own go test -json per step (min leaf cases, no SKIP); require >=1 passing step.
    if grep -qE 'pass=[1-9][0-9]* fail=0 skip=0 .*verdict=0' "$LOG" && ! grep -qE 'verdict=[^0]' "$LOG"; then
      record "$id" "$tier" PASS "$(grep -m1 '^PASS:' "$LOG" | cut -c7-110)" "$rc" "$LOG"
      if grep -q 'NOT_RUN' "$LOG"; then record "${id}+" SANDBOX NOT_RUN "$(grep -m1 'NOT_RUN' "$LOG" | cut -c1-110)" - "$LOG"; fi
    else
      record "$id" "$tier" FAIL "stripe-browser exit 0 without a passing -json step: never PASS" "$rc" "$LOG"
    fi
  elif [[ "$m" == --browser-webkit ]]; then
    # Own row for the real Safari engine (Playwright WebKit, iPhone 15 buyer / Desktop Safari admin). test-local.sh runs six go test -json steps
    # and prints one "<step>: pass=N fail=0 skip=0 ... exit=0 verdict=0" line each; all six must be clean, never inferred from the exit code alone.
    wk=$(grep -cE '^[a-z-]+: pass=[1-9][0-9]* fail=0 skip=0 .*exit=0 verdict=0' "$LOG" || true)
    if ((wk >= 6)) && ! grep -qE 'verdict=[^0]' "$LOG" && grep -q '^PASS:' "$LOG"; then
      record "$id" "$tier" PASS "$wk WebKit steps clean (buyer/order/payment/merchant-buyer/cvs/password-auth)" "$rc" "$LOG"
    else
      record "$id" "$tier" FAIL "browser-webkit exit 0 but only $wk/6 clean step lines: never PASS" "$rc" "$LOG"
    fi
  else
    read -r gp gf sn so <<<"$(judge_gotest "$LOG" "$OUT/$id.skipped" "$OUT/$id.skipped-other")"
    if ((gf > 0 || gp == 0)); then
      record "$id" "$tier" FAIL "exit 0 but go test events pass=$gp fail=$gf: zero tests or a failure is never PASS" "$rc" "$LOG"
    elif ((so > 0)); then
      record "$id" "$tier" FAIL "$so test(s) skipped without a NOT_RUN: message: $(tr '\n' ' ' <"$OUT/$id.skipped-other" | cut -c1-100)" "$rc" "$LOG"
    elif ! grep -qE -- '^PASS:' "$LOG"; then
      record "$id" "$tier" FAIL "exit 0 but test-local.sh printed no PASS line" "$rc" "$LOG"
    else
      record "$id" "$tier" PASS "$gp passed; $(grep -m1 '^PASS:' "$LOG" | cut -c7-100)" "$rc" "$LOG"
      if ((sn > 0)); then
        record "${id}+" SANDBOX NOT_RUN "skipped with NOT_RUN: $(tr '\n' ' ' <"$OUT/$id.skipped" | cut -c1-100)" - "$OUT/$id.skipped"
      fi
    fi
  fi
done

# ---- smoke result reader: FAIL if any FAIL, NOT_RUN if any NOT_RUN/BLOCKED, else PASS ----------------
smoke_verdict() { # $1 = smoke log; prints "STATUS|note"
  python3 - "$1" <<'PY'
import json, re, sys
text = open(sys.argv[1], errors="replace").read()
m = re.findall(r"evidence: (\S+/result\.json)", text)
if not m:
    print("FAIL|no evidence file reported (smoke did not finish)")
    sys.exit(0)
try:
    r = json.load(open(m[-1]))
except Exception:
    print("FAIL|evidence file unreadable")
    sys.exit(0)
res = r.get("results", [])
bad = [c["id"] for c in res if c["status"] == "FAIL"]
nr = [c["id"] for c in res if c["status"] in ("NOT_RUN", "BLOCKED")]
n_pass = sum(1 for c in res if c["status"] == "PASS")
exp = set(r.get("expected_cases", []))
missing = sorted(exp - {c["id"] for c in res})
where = m[-1]
if bad or missing:
    print("FAIL|FAIL=%s missing=%s (%s)" % (",".join(bad) or "-", ",".join(missing) or "-", where))
elif nr:
    print("NOT_RUN|%d PASS, not run: %s (%s)" % (n_pass, ",".join(nr), where))
else:
    print("PASS|%d cases PASS (%s)" % (n_pass, where))
PY
}

# ---- G90 deploy smoke static -------------------------------------------------------------------------
if selected G90; then
  if ! have python3 || ! have docker; then
    record G90 DEPLOY NOT_RUN "python3 or docker missing"
  else
    run_cmd G90 bash deploy/scripts/smoke.sh static
    v=$(smoke_verdict "$LOG")
    record G90 DEPLOY "${v%%|*}" "smoke static: ${v#*|}" "$rc" "$LOG"
  fi
fi

# ---- G91 deploy smoke full (Linux deploy host, root, ports 80/443) -----------------------------------
if selected G91; then
  if [[ "${LC_RELEASE_GATE_SMOKE_FULL:-0}" != 1 ]]; then
    record G91 DEPLOY NOT_RUN "run on the Linux deploy host as root: LC_RELEASE_GATE_SMOKE_FULL=1 or bash deploy/scripts/smoke.sh full"
  elif [[ "$(uname -s)" != Linux || "$(id -u)" != 0 ]] || ! docker_ok; then
    record G91 DEPLOY NOT_RUN "smoke full needs Linux, root and a docker daemon"
  else
    run_cmd G91 bash deploy/scripts/smoke.sh full
    v=$(smoke_verdict "$LOG")
    record G91 DEPLOY "${v%%|*}" "smoke full: ${v#*|}" "$rc" "$LOG"
  fi
fi

# ---- G99 no gate rewrote a tracked file (ruling F7: run output belongs under output/) --------------------
if [[ -z "$only" ]]; then
  changed=$(git status --porcelain --untracked-files=no -- .impeccable 2>/dev/null)
  if [[ "$changed" != "$impeccable_before" ]]; then
    printf '%s\n' "$changed" >"$OUT/G99.log"
    # Restore only when the run started clean, so an owner's uncommitted edit is never discarded.
    [[ -z "$impeccable_before" ]] && git checkout -- .impeccable 2>/dev/null
    record G99 STATIC FAIL "a gate rewrote tracked .impeccable files (restored if the run started clean; see log)" 1 "$OUT/G99.log"
  else
    record G99 STATIC PASS "no tracked .impeccable file rewritten by the run" 0 -
  fi
fi

# ---- table ----------------------------------------------------------------------------------------------
if [[ -n "$only" ]]; then subset=" [subset via --only: not a release verdict]"; else subset=""; fi
n_open=$((n_notrun - n_accepted))
if ((n_fail > 0)); then overall="FAIL ($n_fail FAIL, $n_notrun NOT_RUN [$n_accepted accepted, $n_open UNACCEPTED], $n_pass PASS)"; code=1
elif ((n_open > 0)); then overall="INCOMPLETE: no FAIL, but $n_open UNACCEPTED NOT_RUN (not release acceptance), $n_accepted accepted NOT_RUN, $n_pass PASS"; code=0
elif ((n_notrun > 0)); then overall="PASS with accepted NOT_RUN: 0 FAIL, $n_pass PASS, $n_accepted NOT_RUN (accepted by F11/F12/G3/G4)"; code=0
else overall="PASS ($n_pass PASS)"; code=0; fi
overall="$overall$subset"
if ((strict && n_fail == 0 && n_open > 0)); then code=3; fi
{
  echo "release-gate  commit=$sha  dirty_files=$dirty  modes_in_test-local=$all_modes  evidence=$OUT"
  printf '%-24s %-10s %-8s %s\n' ID TIER STATUS NOTE
  printf '%s' "$rows"
  echo "OVERALL: $overall"
} | tee "$OUT/summary.txt"
exit "$code"
