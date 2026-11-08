"""Silently corrupt an uncompressed (-Z0) dump: find the first lowercase hex
digit at/after `fraction` of the file that sits inside a 32-char md5 run and
replace it with a different hex digit. Keeps the COPY row syntactically valid.

usage: python3 -I flip_hex.py FILE FRACTION
"""
import os
import re
import sys

path, frac = sys.argv[1], float(sys.argv[2])
size = os.path.getsize(path)
start = int(size * frac)
with open(path, "r+b") as f:
    f.seek(start)
    chunk = f.read(4096)
    m = re.search(rb"\t([0-9a-f]{32})\t", chunk)
    if not m:
        sys.exit("no md5 run found")
    off = start + m.start(1)
    old = chunk[m.start(1):m.start(1) + 1]
    new = b"0" if old != b"0" else b"1"
    f.seek(off)
    f.write(new)
    line_start = chunk.rfind(b"\n", 0, m.start(1)) + 1
    line_end = chunk.find(b"\n", m.start(1))
print(f"replaced {old!r} with {new!r} at offset {off} of {size}")
print("row before:", chunk[line_start:line_end].decode())
