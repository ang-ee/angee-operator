"""XOR `n` bytes at `fraction` of a file with 0xA5, in place. Prints the offset.

usage: python3 -I flip.py FILE FRACTION [N]
"""
import os
import sys

path, frac = sys.argv[1], float(sys.argv[2])
n = int(sys.argv[3]) if len(sys.argv) > 3 else 16
size = os.path.getsize(path)
off = int(size * frac)
with open(path, "r+b") as f:
    f.seek(off)
    buf = bytearray(f.read(n))
    for i in range(len(buf)):
        buf[i] ^= 0xA5
    f.seek(off)
    f.write(buf)
print(f"flipped {n} bytes at offset {off} of {size}")
