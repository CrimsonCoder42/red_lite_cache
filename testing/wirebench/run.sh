#!/bin/bash
# gRPC wire benchmarks (grpc_build_2026-11-11), 6 runs, medians. The WebSocket comparison made
# before WebSocket was removed is doc/grpc_build_2026-11-11/out/bench_ws_vs_grpc.txt. Output: testing/runs/wirebench_<stamp>.txt and a
# median table on stdout.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
OUT="testing/runs/wirebench_$(date +%Y%m%d_%H%M%S).txt"
mkdir -p testing/runs
for i in 1 2 3 4 5 6; do
	echo "== run $i" | tee -a "$OUT"
	go test -run '^$' -count=1 -timeout 1800s ./testing/wirebench/ \
		-bench 'BenchmarkOp$|BenchmarkOp/' -benchtime 3000x 2>&1 | grep -E '^Benchmark' | tee -a "$OUT"
	go test -run '^$' -count=1 -timeout 1800s ./testing/wirebench/ \
		-bench 'BenchmarkConnectState' -benchtime 3x 2>&1 | grep -E '^Benchmark' | tee -a "$OUT"
	go test -run '^$' -count=1 -timeout 1800s ./testing/wirebench/ \
		-bench 'BenchmarkBroadcast' -benchtime 2000x 2>&1 | grep -E '^Benchmark' | tee -a "$OUT"
	go test -run '^$' -count=1 -timeout 1800s ./testing/wirebench/ \
		-bench 'BenchmarkParallel' -benchtime 20000x 2>&1 | grep -E '^Benchmark' | tee -a "$OUT"
	go test -run '^$' -count=1 -timeout 1800s ./testing/wirebench/ \
		-bench 'BenchmarkStalledReader' -benchtime 3000x 2>&1 | grep -E '^Benchmark' | tee -a "$OUT"
done
python3 - "$OUT" <<'PY'
import re, sys, statistics, collections
vals = collections.defaultdict(lambda: collections.defaultdict(list))
for line in open(sys.argv[1]):
    if not line.startswith("Benchmark"):
        continue
    f = line.split()
    name = re.sub(r"-\d+$", "", f[0])
    for v, unit in zip(f[2::2], f[3::2]):
        vals[name][unit].append(float(v))
print(f"\n{'benchmark':40s} {'n':>2s}  medians")
for name in vals:
    parts = [f"{statistics.median(v):,.1f} {u}" for u, v in vals[name].items()]
    print(f"{name:40s} {len(next(iter(vals[name].values()))):2d}  " + "   ".join(parts))
PY
echo "raw: $OUT"
