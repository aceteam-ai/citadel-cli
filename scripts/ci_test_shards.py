#!/usr/bin/env python3
"""Deterministic test sharding for the CI "Build + Test" gate (citadel-cli#1707).

Usage:
  scripts/ci_test_shards.py plan        print the shard plan
  scripts/ci_test_shards.py check       fail unless every `go list ./...` package
                                        (and every internal/platform Test func)
                                        lands in exactly one shard
  scripts/ci_test_shards.py run N       run shard N (0-based) with `go test`

Weights come from a green main run (seconds, test time only, ubuntu-latest):
platform 459, update 124, jobs 121, cmd 83, worker 39, everything else ~140
combined. internal/platform alone is longer than the whole target, so its Test
functions are split by name round-robin across PLATFORM_SHARDS shards (each
shard runs `go test -run '^(A|B|...)$' ./internal/platform`). Packages not
named in HEAVY are spread round-robin over SPREAD_SHARDS, so a new package is
picked up automatically and `check` fails only if the plan itself is broken.
"""
import glob
import os
import re
import subprocess
import sys

NSHARDS = 5
PLATFORM = "internal/platform"
PLATFORM_SHARDS = [0, 1, 2]
# Static map of known-heavy packages (relative to the module root).
HEAVY = {
    "internal/update": 3,
    "internal/jobs": 3,
    "cmd": 4,
    "internal/worker": 4,
}
SPREAD_SHARDS = [0, 1, 2, 4]

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def go_list():
    out = subprocess.check_output(["go", "list", "./..."], cwd=ROOT, text=True)
    pkgs = [p for p in out.split() if p]
    mod = subprocess.check_output(["go", "list", "-m"], cwd=ROOT, text=True).strip()
    rel = {}
    for p in pkgs:
        rel[p] = "." if p == mod else p[len(mod) + 1:]
    return rel  # import path -> relative dir


def platform_tests():
    names = set()
    pat = re.compile(r"^func (Test[A-Za-z0-9_]*)\(", re.M)
    for f in glob.glob(os.path.join(ROOT, PLATFORM, "*_test.go")):
        with open(f, encoding="utf-8") as fh:
            names.update(pat.findall(fh.read()))
    return sorted(names)


def plan():
    rel = go_list()
    shards = [{"pkgs": [], "platform_tests": []} for _ in range(NSHARDS)]
    rest = []
    for imp in sorted(rel):
        d = rel[imp]
        if d == PLATFORM:
            continue
        if d in HEAVY:
            shards[HEAVY[d]]["pkgs"].append(imp)
        else:
            rest.append(imp)
    for i, imp in enumerate(rest):
        shards[SPREAD_SHARDS[i % len(SPREAD_SHARDS)]]["pkgs"].append(imp)
    plat_imp = [i for i, d in rel.items() if d == PLATFORM]
    for i, name in enumerate(platform_tests()):
        shards[PLATFORM_SHARDS[i % len(PLATFORM_SHARDS)]]["platform_tests"].append(name)
    return rel, plat_imp, shards


def check():
    rel, plat_imp, shards = plan()
    seen = {}
    for n, s in enumerate(shards):
        for p in s["pkgs"]:
            if p in seen:
                sys.exit(f"package {p} in shards {seen[p]} and {n}")
            seen[p] = n
    missing = [p for p in rel if p not in seen and rel[p] != PLATFORM]
    if missing:
        sys.exit("packages not covered by any shard: " + ", ".join(missing))
    if not plat_imp:
        sys.exit("internal/platform not found in go list")
    all_names = platform_tests()
    got = [t for s in shards for t in s["platform_tests"]]
    if sorted(got) != all_names or len(set(got)) != len(got):
        sys.exit("internal/platform Test functions are not partitioned exactly once")
    if not all_names:
        sys.exit("found no internal/platform tests")
    print(f"ok: {len(rel)} packages ({len(seen)} sharded + {PLATFORM} split "
          f"{len(all_names)} tests over {len(PLATFORM_SHARDS)} shards) across {NSHARDS} shards")


def show():
    _, _, shards = plan()
    for n, s in enumerate(shards):
        print(f"shard {n}: {len(s['pkgs'])} packages, {len(s['platform_tests'])} platform tests")
        for p in s["pkgs"]:
            print("   ", p)


def run(n):
    _, plat_imp, shards = plan()
    s = shards[n]
    rc = 0
    if s["pkgs"]:
        print("+ go test", len(s["pkgs"]), "packages", flush=True)
        rc |= subprocess.call(["go", "test"] + s["pkgs"], cwd=ROOT)
    if s["platform_tests"]:
        regex = "^(" + "|".join(s["platform_tests"]) + ")$"
        print(f"+ go test -run <{len(s['platform_tests'])} tests> {plat_imp[0]}", flush=True)
        rc |= subprocess.call(["go", "test", "-run", regex] + plat_imp, cwd=ROOT)
    sys.exit(1 if rc else 0)


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "check":
        check()
    elif cmd == "plan":
        show()
    elif cmd == "run" and len(sys.argv) == 3 and 0 <= int(sys.argv[2]) < NSHARDS:
        run(int(sys.argv[2]))
    else:
        sys.exit(__doc__)
