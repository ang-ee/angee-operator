"""Spike 7 restore hazard: put a backup in place of app.db while the old
database's app.db-wal / app.db-shm (left by a killed writer) are still there.

usage: python3 -I sqlite_restore_hazard.py WORKDIR
"""
import os
import shutil
import signal
import sqlite3
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
work = sys.argv[1]


def check(path):
    try:
        con = sqlite3.connect(path)
        ic = [r[0] for r in con.execute("PRAGMA integrity_check").fetchall()]
        n = con.execute("SELECT count(*) FROM t NOT INDEXED").fetchone()[0]
        m = con.execute("SELECT v FROM meta WHERE k='rows'").fetchone()
        meta = m[0] if m else "MISSING meta row"
        con.close()
        return {"integrity": (ic[0][:160] + (f" ... (+{len(ic) - 1} more)" if len(ic) > 1 else "")), "rows": n, "meta": meta}
    except sqlite3.DatabaseError as e:
        return {"error": f"{type(e).__name__}: {e}"}


def snapshot_state(src_dir, name):
    """Copy db+wal+shm of a stopped DB to a side dir and open it there."""
    d = os.path.join(work, name)
    shutil.copytree(src_dir, d)
    return check(os.path.join(d, "app.db"))


def scenario(label, ckpt, method, clean_sidecars):
    live = os.path.join(work, "live")
    shutil.rmtree(work, ignore_errors=True)
    os.makedirs(live)
    db = os.path.join(live, "app.db")
    w = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "sqlite_writer.py"), db, str(ckpt), "60", "0.002"],
                         stdout=subprocess.DEVNULL)
    time.sleep(2)
    bk = os.path.join(work, "backup.db")
    if method == "py-backup":
        s, d = sqlite3.connect(db), sqlite3.connect(bk)
        s.backup(d)
        d.close(); s.close()
    else:
        subprocess.run(["/usr/bin/sqlite3", db, f"VACUUM INTO '{bk}'"], check=True)
    expected = check(bk)
    time.sleep(2)
    w.send_signal(signal.SIGKILL)
    w.wait()
    files = {f: os.path.getsize(os.path.join(live, f)) for f in sorted(os.listdir(live))}
    crashed = snapshot_state(live, "crashed-copy")
    # "restore": put the backup in place of app.db
    shutil.copyfile(bk, db)
    if clean_sidecars:
        for f in ("app.db-wal", "app.db-shm"):
            if os.path.exists(os.path.join(live, f)):
                os.remove(os.path.join(live, f))
    after = check(db)
    print(f"== {label}")
    print(f"   writer wal_autocheckpoint={ckpt}, backup via {method}, SIGKILLed writer left: {files}")
    print(f"   backup.db (what we meant to restore): {expected}")
    print(f"   old DB as the crash left it         : {crashed}")
    print(f"   app.db after restore{' (+ removed -wal/-shm)' if clean_sidecars else ' (stale -wal/-shm kept)'}: {after}")


scenario("R1 page-identical backup + stale WAL, checkpoints happened after the backup", 1000, "py-backup", False)
scenario("R2 page-identical backup + stale WAL, no checkpoints at all (autocheckpoint=0)", 0, "py-backup", False)
scenario("R3 VACUUM INTO backup (re-laid-out pages) + stale WAL", 1000, "vacuum-into", False)
scenario("R4 VACUUM INTO backup, -wal/-shm removed before opening", 1000, "vacuum-into", True)
scenario("R5 page-identical backup, -wal/-shm removed before opening", 1000, "py-backup", True)
shutil.rmtree(work, ignore_errors=True)
