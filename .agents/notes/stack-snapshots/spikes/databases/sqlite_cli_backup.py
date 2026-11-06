"""Spike 7 (c'): how long does `sqlite3 app.db ".backup copy.db"` take under a
continuous writer in another process, relative to when the writer stops?

usage: python3 -I sqlite_cli_backup.py WORKDIR
"""
import os
import shutil
import sqlite3
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
work = sys.argv[1]


def check(path):
    con = sqlite3.connect(path)
    ic = con.execute("PRAGMA integrity_check").fetchone()[0]
    n = con.execute("SELECT count(*) FROM t NOT INDEXED").fetchone()[0]
    meta = con.execute("SELECT v FROM meta WHERE k='rows'").fetchone()[0]
    con.close()
    return ic, n, meta


for nap, secs in (("0.002", 25), ("0.05", 25), ("0.5", 25)):
    shutil.rmtree(work, ignore_errors=True)
    os.makedirs(work)
    db = os.path.join(work, "app.db")
    # seed ~20 MB so the backup needs many 100-page steps
    seed = subprocess.run([sys.executable, "-I", os.path.join(HERE, "sqlite_writer.py"), db, "1000", "1.5", "0"],
                          capture_output=True, text=True)
    t0 = time.time()
    w = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "sqlite_writer.py"), db, "1000", str(secs), nap],
                         stdout=subprocess.DEVNULL)
    time.sleep(2)
    tb = time.time()
    r = subprocess.run(["/usr/bin/sqlite3", db, f".backup '{work}/copy.db'"], capture_output=True, text=True)
    te = time.time()
    w.wait()
    tw = time.time()
    size = os.path.getsize(db)
    print(f"writer sleep {nap}s/txn for {secs}s (db {size // 1048576} MiB): .backup started t+{tb - t0:.1f}s, "
          f"returned t+{te - t0:.1f}s (took {te - tb:.1f}s, rc={r.returncode}); writer exited t+{tw - t0:.1f}s; "
          f"copy integrity/rows/meta = {check(work + '/copy.db')}", flush=True)
    t1 = time.time()
    w = subprocess.Popen([sys.executable, "-I", os.path.join(HERE, "sqlite_writer.py"), db, "1000", "8", nap],
                         stdout=subprocess.DEVNULL)
    time.sleep(1)
    tv = time.time()
    subprocess.run(["/usr/bin/sqlite3", db, f"VACUUM INTO '{work}/copy2.db'"], check=True)
    print(f"    VACUUM INTO under the same writer took {time.time() - tv:.2f}s; copy = {check(work + '/copy2.db')}",
          flush=True)
    w.wait()
shutil.rmtree(work, ignore_errors=True)
