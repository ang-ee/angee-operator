"""Summarise s05.results.json: mean wall per op per injected delay, request counts,
and the fitted slope (= effective number of sequential round trips)."""
import collections
import json
import statistics
import sys

d = json.load(open(sys.argv[1]))
inv = d["invocations"]

print("Phase A (20 sequential stdin backups, 1 KiB..1 MiB, 0 ms):")
a = [r for r in inv if r["label"].startswith("A.")]
walls = [r["wall_s"] for r in a]
reqs = collections.Counter(r["requests"] for r in a)
print(f"  wall mean={statistics.mean(walls):.3f}s min={min(walls):.3f}s max={max(walls):.3f}s; requests per invocation: {dict(reqs)}")
first_kinds = collections.Counter()
for r in a:
    first_kinds.update(r["by_kind"])
print("  per-invocation breakdown (sum over 20 / 20):", {k: round(v / len(a), 2) for k, v in sorted(first_kinds.items())})
print()

c = [r for r in inv if r["label"].startswith("C.")]
ops = collections.OrderedDict()
for r in c:
    ops.setdefault(r["label"], collections.defaultdict(list))[r["delay_ms"]].append(r)
print(f'{"operation":44s} {"reqs":>9s} {"inflight":>8s} | ' + " ".join(f"{x:>7d}ms" for x in (0, 20, 50, 100)) + " | slope(RTTs)")
for label, by in ops.items():
    means = {dl: statistics.mean(x["wall_s"] for x in rs) for dl, rs in by.items()}
    reqs = sorted(set(x["requests"] for rs in by.values() for x in rs))
    inf = max(x["max_inflight"] for rs in by.values() for x in rs)
    xs = sorted(means)
    mx = statistics.mean(xs)
    my = statistics.mean(means[x] for x in xs)
    slope = sum((x - mx) * (means[x] - my) for x in xs) / sum((x - mx) ** 2 for x in xs)
    rtts = slope * 1000  # seconds per ms of delay -> number of sequential round trips
    print(f"{label:44s} {str(reqs):>9s} {inf:>8d} | " + " ".join(f"{means.get(x, float('nan')):8.3f}s" for x in (0, 20, 50, 100)) + f" | {rtts:5.1f}")
