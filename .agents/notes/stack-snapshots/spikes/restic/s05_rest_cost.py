"""Spike 5: request count and latency cost of many small restic invocations
against a REST backend (rest-server), measured through a latency-injecting proxy.

Run: python3 -I s05_rest_cost.py
"""
import collections
import json
import os
import re
import shutil
import signal
import statistics
import subprocess
import sys
import time

SP = "<scratchpad>"
D = SP + "/spikes/restic"
W = D + "/work/s05"
RESTIC = SP + "/bin/restic"
REST_SERVER = SP + "/bin/rest-server"
RS_PORT, PX_PORT = 18000, 18001
DELAY_FILE = W + "/delay_ms"
PX_LOG = W + "/proxy.jsonl"
RS_LOG = W + "/rest-access.log"
REPO_PX = f"rest:http://127.0.0.1:{PX_PORT}/"
REPO_RS = f"rest:http://127.0.0.1:{RS_PORT}/"
HOST = "stackA"
OUT = D + "/s05.results.json"

ENV = dict(os.environ)
for k in ("RESTIC_REPOSITORY", "RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND", "RESTIC_HOST"):
    ENV.pop(k, None)
ENV["RESTIC_PASSWORD_FILE"] = D + "/password.txt"
ENV["RESTIC_CACHE_DIR"] = W + "/cache"

results = {"invocations": []}


def set_delay(ms):
    with open(DELAY_FILE, "w") as f:
        f.write(str(ms))


def lines(path):
    try:
        with open(path) as f:
            return f.readlines()
    except FileNotFoundError:
        return []


def kind(path):
    p = path.split("?")[0]
    seg = [s for s in p.split("/") if s]
    if not seg:
        return "root"
    t = seg[0]
    if len(seg) == 1:
        return t + "(list)" if t != "config" else "config"
    return t


def run(label, args, stdin=None, repo=REPO_PX, env=None, record=True, delay=None):
    before_px = len(lines(PX_LOG))
    before_rs = len(lines(RS_LOG))
    t0 = time.monotonic()
    p = subprocess.run([RESTIC, "-r", repo] + args, input=stdin, capture_output=True, env=env or ENV)
    wall = time.monotonic() - t0
    time.sleep(0.05)  # let the proxy flush its last log line
    px = [json.loads(l) for l in lines(PX_LOG)[before_px:]]
    rs = lines(RS_LOG)[before_rs:]
    by = collections.Counter(f'{r["m"]} {kind(r["p"])}' for r in px)
    maxinf = max([r["inflight"] for r in px], default=0)
    row = {"label": label, "delay_ms": delay, "exit": p.returncode, "wall_s": round(wall, 3),
           "requests": len(px), "rest_server_log_lines": len(rs), "max_inflight": maxinf,
           "by_kind": dict(sorted(by.items()))}
    if p.returncode != 0:
        row["stderr"] = p.stderr.decode()[-500:]
    if record:
        results["invocations"].append(row)
    return row, p


def start_servers():
    shutil.rmtree(W, ignore_errors=True)
    os.makedirs(W + "/data")
    set_delay(0)
    rs = subprocess.Popen([REST_SERVER, "--path", W + "/data", "--listen", f"127.0.0.1:{RS_PORT}", "--no-auth", "--log", RS_LOG],
                          stdout=open(W + "/rest-server.out", "w"), stderr=subprocess.STDOUT)
    px = subprocess.Popen([sys.executable, "-I", D + "/latency_proxy.py", str(PX_PORT), str(RS_PORT), DELAY_FILE, PX_LOG],
                          stdout=open(W + "/proxy.out", "w"), stderr=subprocess.STDOUT)
    time.sleep(1.0)
    return rs, px


def payload(n, seed):
    return os.urandom(n)


def backup_args(snap_id, kind_, svc, fname, extra=()):
    tags = f"angee.snapshot={snap_id},angee.kind={kind_}" + (f",angee.service={svc}" if svc else "")
    return ["backup", "--stdin", "--stdin-filename", fname, "--host", HOST, "--tag", tags, "--json", "-q", *extra]


def main():
    rs, px = start_servers()
    print("servers pids", rs.pid, px.pid, flush=True)
    try:
        row, p = run("init", ["init", "--json"], delay=0)
        print(row, flush=True)
        if p.returncode != 0:
            print(p.stderr.decode())
            return

        # Phase A: 20 sequential small stdin backups, 1 KiB .. 1 MiB, delay 0
        sizes = [1024 * (2 ** (i % 11)) for i in range(20)]  # 1 KiB .. 1 MiB
        for i, n in enumerate(sizes):
            row, _ = run(f"A.backup#{i+1} {n}B", backup_args(f"a{i:03d}", "member", "app", f"databases/app/app.pgdump"),
                         stdin=payload(n, i), delay=0)
            print(json.dumps(row), flush=True)

        # Phase B: populate to ~200 snapshots directly against rest-server (not recorded)
        # 45 stack snapshots x (3 members + 1 manifest) = 180 more
        t0 = time.monotonic()
        for s in range(45):
            sid = f"b{s:03d}"
            for svc, fn in (("app", "databases/app/app.pgdump"), ("redis", "databases/redis/dump.rdb"), ("media", "files/media/media.tar")):
                run("populate", backup_args(sid, "member", svc, fn), stdin=payload(4096, s), repo=REPO_RS, record=False)
            run("populate", backup_args(sid, "manifest", None, "angee/manifest.json"), stdin=json.dumps({"id": sid}).encode(), repo=REPO_RS, record=False)
        print(f"populated in {time.monotonic()-t0:.1f}s", flush=True)
        _, p = run("count", ["snapshots", "--json"], repo=REPO_RS, record=False)
        snaps = json.loads(p.stdout)
        print("snapshot count:", len(snaps), flush=True)
        results["snapshot_count"] = len(snaps)
        member = next(s for s in snaps if "angee.kind=member" in s.get("tags", []))
        n_index = len(os.listdir(W + "/data/index"))
        results["index_files_before_phaseC"] = n_index
        print("index files:", n_index, flush=True)

        # Phase C: per-delay measurements (warm cache), plus a cold-cache backup
        for d in (0, 20, 50, 100):
            set_delay(d)
            for rep in range(3):
                row, _ = run(f"C.backup warm", backup_args(f"c{d}-{rep}", "member", "app", "databases/app/app.pgdump"),
                             stdin=payload(65536, rep), delay=d)
                print(json.dumps(row), flush=True)
            row, _ = run("C.backup manifest warm", backup_args(f"c{d}", "manifest", None, "angee/manifest.json"),
                         stdin=b'{"x":1}', delay=d)
            print(json.dumps(row), flush=True)
            row, _ = run("C.snapshots --host --tag manifest", ["snapshots", "--json", "--host", HOST, "--tag", "angee.kind=manifest"], delay=d)
            print(json.dumps(row), flush=True)
            row, _ = run("C.snapshots --no-lock", ["snapshots", "--json", "--no-lock", "--host", HOST, "--tag", "angee.kind=manifest"], delay=d)
            print(json.dumps(row), flush=True)
            row, _ = run("C.dump member file", ["dump", member["id"], member["paths"][0]], delay=d)
            print(json.dumps(row), flush=True)
            row, _ = run("C.forget --dry-run manifest keep-last 3",
                         ["forget", "--dry-run", "--json", "--host", HOST, "--tag", "angee.kind=manifest", "--keep-last", "3"], delay=d)
            print(json.dumps(row), flush=True)
            cold = dict(ENV)
            cold["RESTIC_CACHE_DIR"] = W + f"/cache-cold-{d}"
            row, _ = run("C.backup COLD cache", backup_args(f"cold{d}", "member", "app", "databases/app/app.pgdump"),
                         stdin=payload(65536, 99), env=cold, delay=d)
            print(json.dumps(row), flush=True)
            row, _ = run("C.backup warm rest.connections=1", backup_args(f"c1conn{d}", "member", "app", "databases/app/app.pgdump",
                                                                         extra=("-o", "rest.connections=1")),
                         stdin=payload(65536, 7), delay=d)
            print(json.dumps(row), flush=True)
        set_delay(0)
    finally:
        for proc in (px, rs):
            proc.send_signal(signal.SIGTERM)
        for proc in (px, rs):
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        with open(OUT, "w") as f:
            json.dump(results, f, indent=1)
        print("servers stopped; results in", OUT, flush=True)


if __name__ == "__main__":
    main()
