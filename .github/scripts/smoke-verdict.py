#!/usr/bin/env python3
"""CI verdict for deploy/scripts/smoke.sh (workflow deploy-smoke.yml, R1 ruling G3).

Usage: smoke-verdict.py <result.json> <smoke exit code> static|full
Follows smoke.sh's exit contract (0 PASS, 1 FAIL, 3 BLOCKED) and adds what the exit code cannot say:
BLOCKED is accepted only for the ids in ACCEPTED_BLOCKED (ruling F11), and a NOT_RUN case or an expected
case that never ran fails the job (a runner has every smoke prerequisite). Prints ids and notes only;
result.json holds no secret values (smoke.sh secret-scans its evidence before deleting temp secrets).
"""
import json
import sys

ACCEPTED_BLOCKED = {"S29m"}  # F11: logical-restore media gate (I8); LiveKit media is not in R1


def verdict(result, rc, mode):
    cases = result.get("results", [])
    by = lambda status: [c["id"] for c in cases if c["status"] == status]
    fail, blocked, not_run = by("FAIL"), by("BLOCKED"), by("NOT_RUN")
    missing = sorted(set(result.get("expected_cases", [])) - {c["id"] for c in cases})
    problems = []
    if rc not in (0, 1, 3):
        problems.append(f"smoke.sh exit {rc} is outside its contract (0/1/3)")
    if rc == 1 or fail:
        problems.append("FAIL: " + (",".join(fail) or "smoke.sh exit 1"))
    if set(blocked) - ACCEPTED_BLOCKED:
        problems.append("BLOCKED (not accepted): " + ",".join(sorted(set(blocked) - ACCEPTED_BLOCKED)))
    if rc == 3 and not blocked:
        problems.append("exit 3 without a BLOCKED case")
    if not_run:
        problems.append("NOT_RUN: " + ",".join(not_run))
    if missing:
        problems.append("never ran: " + ",".join(missing))
    if result.get("environment", {}).get("mode") != mode:
        problems.append(f"result.json is not a {mode} run")
    n_pass = len(by("PASS"))
    summary = f"smoke {mode}: {n_pass} PASS, {len(fail)} FAIL, BLOCKED={','.join(blocked) or '-'}, NOT_RUN={','.join(not_run) or '-'}"
    return problems, summary


def main():
    path, rc, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
    try:
        result = json.load(open(path))
    except (OSError, ValueError):
        print(f"::error::smoke {mode}: no readable result.json (smoke did not finish)")
        return 1
    problems, summary = verdict(result, rc, mode)
    for c in result.get("results", []):
        print(f"{c['id']}\t{c['status']}\t{c.get('note', '')}")
    print(summary)
    for p in problems:
        print(f"::error::smoke {mode}: {p}")
    return 1 if problems else 0


def selftest():
    ok = {"environment": {"mode": "full"}, "expected_cases": ["S01", "S29m"],
          "results": [{"id": "S01", "status": "PASS"}, {"id": "S29m", "status": "BLOCKED"}]}
    assert verdict(ok, 3, "full")[0] == []
    assert verdict(dict(ok, results=[{"id": "S01", "status": "PASS"}, {"id": "S29m", "status": "PASS"}]), 0, "full")[0] == []
    assert verdict(dict(ok, results=[{"id": "S01", "status": "BLOCKED"}, {"id": "S29m", "status": "BLOCKED"}]), 3, "full")[0]
    assert verdict(dict(ok, results=[{"id": "S01", "status": "NOT_RUN"}, {"id": "S29m", "status": "BLOCKED"}]), 3, "full")[0]
    assert verdict(dict(ok, results=[{"id": "S01", "status": "PASS"}]), 0, "full")[0]  # S29m never ran
    assert verdict(dict(ok, results=[{"id": "S01", "status": "FAIL"}, {"id": "S29m", "status": "BLOCKED"}]), 1, "full")[0]
    assert verdict(ok, 0, "static")[0]  # wrong mode
    print("smoke-verdict selftest ok")


if __name__ == "__main__":
    if sys.argv[1:] == ["--selftest"]:
        selftest()
        sys.exit(0)
    sys.exit(main())
