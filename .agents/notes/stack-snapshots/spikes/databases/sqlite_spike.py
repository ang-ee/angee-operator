"""Spike 7: copying a live SQLite WAL database.

usage: python3 -I sqlite_spike.py WORKDIR AUTOCHECKPOINT_PAGES SECONDS ITERATIONS [WRITER_SLEEP]

Starts sqlite_writer.py against WORKDIR/live/app.db and, while it writes,
repeatedly takes copies with each method and checks each copy.
"""
import collections
import os
import random
import shutil
import sqlite3
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SQLITE = "/usr/bin/sqlite3"
work, ckpt, secs, iters = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
nap = sys.argv[5] if len(sys.argv) > 5 else "0.005"
live = os.path.join(work, "live")
shutil.rmtree(work, ignore_errors=True)
os.makedirs(live)
db = os.path.join(live, "app.db")


def check(path):
    """Open a copy and report integrity + consistency."""
    try:
        con = sqlite3.connect(path)
        ic = [r[0] for r in con.execute("PRAGMA integrity_check").fetchall()]
        n = con.execute("SELECT count(*) FROM t NOT INDEXED").fetchone()[0]
        meta = con.execute("SELECT v FROM meta WHERE k='rows'").fetchone()[0]
        torn = con.execute(
            "SELECT count(*) FROM (SELECT txn FROM t NOT INDEXED GROUP BY txn HAVING count(*) != 50)"
        ).fetchone()[0]
        con.close()
        ok = ic == ["ok"] and n == meta and torn == 0
        return {"ok": ok, "integrity": ic[:3], "rows": n, "meta": meta, "torn_txns": torn}
    except sqlite3.DatabaseError as e:
        return {"ok": False, "error": f"{type(e).__name__}: {e}"}


def live_rows():
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    n = con.execute("SELECT v FROM meta WHERE k='rows'").fetchone()[0]
    con.close()
    return n


def fresh(name):
    d = os.path.join(work, name)
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    return d


def m_cp_db_then_wal(i):
    d = fresh(f"a{i}")
    before = live_rows()
    subprocess.run(["cp", db, d + "/app.db"], check=True)
    time.sleep(random.uniform(0.02, 0.4))
    if os.path.exists(db + "-wal"):
        subprocess.run(["cp", db + "-wal", d + "/app.db-wal"], check=True)
    return before, check(d + "/app.db")


def m_cp_wal_then_db(i):
    d = fresh(f"w{i}")
    before = live_rows()
    if os.path.exists(db + "-wal"):
        subprocess.run(["cp", db + "-wal", d + "/app.db-wal"], check=True)
    time.sleep(random.uniform(0.02, 0.4))
    subprocess.run(["cp", db, d + "/app.db"], check=True)
    return before, check(d + "/app.db")


def m_cp_db_only(i):
    d = fresh(f"b{i}")
    before = live_rows()
    subprocess.run(["cp", db, d + "/app.db"], check=True)
    return before, check(d + "/app.db")


def m_cli_backup(i):
    d = fresh(f"c{i}")
    before = live_rows()
    t0 = time.time()
    r = subprocess.run([SQLITE, db, f".backup '{d}/copy.db'"], capture_output=True, text=True, timeout=120)
    res = check(d + "/copy.db")
    res["secs"] = round(time.time() - t0, 3)
    if r.returncode:
        res["cli_rc"] = r.returncode
        res["cli_err"] = r.stderr.strip()[:200]
    return before, res


def m_vacuum_into(i):
    d = fresh(f"d{i}")
    before = live_rows()
    t0 = time.time()
    r = subprocess.run([SQLITE, db, f"VACUUM INTO '{d}/copy2.db'"], capture_output=True, text=True, timeout=120)
    res = check(d + "/copy2.db")
    res["secs"] = round(time.time() - t0, 3)
    if r.returncode:
        res["cli_rc"] = r.returncode
        res["cli_err"] = r.stderr.strip()[:200]
    return before, res


def m_py_backup(i):
    d = fresh(f"p{i}")
    before = live_rows()
    src = sqlite3.connect(db)
    dst = sqlite3.connect(d + "/copy.db")
    src.backup(dst)  # pages=-1: one step, one read transaction
    dst.close()
    src.close()
    return before, check(d + "/copy.db")


METHODS = [
    ("cp db, then -wal later", m_cp_db_then_wal),
    ("cp -wal, then db later", m_cp_wal_then_db),
    ("cp db only (no -wal)", m_cp_db_only),
    ("sqlite3 .backup", m_cli_backup),
    ("sqlite3 VACUUM INTO", m_vacuum_into),
    ("python Connection.backup()", m_py_backup),
]
if len(sys.argv) > 6 and sys.argv[6] == "nocli":
    # the CLI .backup restarts whenever another process writes, so under a
    # continuous writer one call can block for the writer's whole lifetime;
    # spike7_cli_backup.py measures that on its own.
    METHODS = [m for m in METHODS if m[1] is not m_cli_backup]

writer = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "sqlite_writer.py"), db, str(ckpt), str(secs), nap])
time.sleep(1.0)
print(f"== writer: wal_autocheckpoint={ckpt}, {secs}s, sleep {nap}s/txn; {iters} iterations per method", flush=True)
stats = collections.defaultdict(lambda: collections.Counter())
examples = collections.defaultdict(list)
t_end = time.time() + secs - 1.5
i = 0
while i < iters and time.time() < t_end:
    for name, fn in METHODS:
        before, res = fn(i)
        st = stats[name]
        st["n"] += 1
        if "error" in res:
            st["open/query error"] += 1
            key = "error"
        elif res["integrity"] != ["ok"]:
            st["integrity_check failed"] += 1
            key = "integrity"
        elif res["rows"] != res["meta"] or res["torn_txns"]:
            st["inconsistent (rows!=meta or torn txn)"] += 1
            key = "inconsistent"
        else:
            st["ok"] += 1
            key = "ok"
            if res["rows"] < before:
                st["ok but older than live-before"] += 1
                missing = before - res["rows"]
                st["missing rows (sum)"] += missing
                st["max missing rows"] = max(st["max missing rows"], missing)
        if "secs" in res:
            st["max secs x1000"] = max(st["max secs x1000"], int(res["secs"] * 1000))
        if key != "ok" and len(examples[name]) < 3:
            examples[name].append({"iter": i, "live_rows_before": before, **res})
        if key == "ok" and name == "cp db only (no -wal)" and len(examples[name]) < 3:
            examples[name].append({"iter": i, "live_rows_before": before, **res})
        for d in ("a", "w", "b", "c", "d", "p"):
            shutil.rmtree(os.path.join(work, f"{d}{i}"), ignore_errors=True)
    i += 1
writer.wait()
print(f"== iterations completed: {i}; final live rows {live_rows()}; wal size {os.path.getsize(db + '-wal') if os.path.exists(db + '-wal') else 0}", flush=True)
for name, _ in METHODS:
    print(f"-- {name}: {dict(stats[name])}")
    for ex in examples[name]:
        print(f"     e.g. {ex}")
