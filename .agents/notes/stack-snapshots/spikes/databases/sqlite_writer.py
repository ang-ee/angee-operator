"""Continuous SQLite WAL writer for spike 7.

usage: python3 -I sqlite_writer.py DB AUTOCHECKPOINT_PAGES SECONDS [SLEEP_PER_TXN]
Every transaction inserts ROWS rows sharing one txn number and bumps
meta.rows by ROWS, so a consistent snapshot has count(*) == meta.rows and
every txn has exactly ROWS rows.
"""
import os
import sqlite3
import sys
import time

ROWS = 50
db, ckpt, secs = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
nap = float(sys.argv[4]) if len(sys.argv) > 4 else 0.0
con = sqlite3.connect(db, isolation_level=None)
con.execute("PRAGMA journal_mode=WAL")
con.execute("PRAGMA synchronous=NORMAL")
con.execute(f"PRAGMA wal_autocheckpoint={ckpt}")
con.execute("CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, txn INTEGER NOT NULL, payload BLOB NOT NULL)")
con.execute("CREATE INDEX IF NOT EXISTS t_txn ON t(txn)")
con.execute("CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL)")
con.execute("INSERT OR IGNORE INTO meta VALUES ('rows', 0)")
txn = (con.execute("SELECT coalesce(max(txn), 0) FROM t").fetchone()[0]) + 1
end = time.time() + secs
n = 0
while time.time() < end:
    con.execute("BEGIN IMMEDIATE")
    con.executemany("INSERT INTO t (txn, payload) VALUES (?, ?)",
                    [(txn, os.urandom(200)) for _ in range(ROWS)])
    con.execute("UPDATE meta SET v = v + ? WHERE k = 'rows'", (ROWS,))
    con.execute("COMMIT")
    txn += 1
    n += 1
    if nap:
        time.sleep(nap)
print(f"writer done: {n} txns, last txn {txn - 1}", flush=True)
con.close()
