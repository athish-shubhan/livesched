#!/usr/bin/env bash
# Reproduces results/: baselines and ablations, the cost/stall frontier of
# HCDN's offload knobs, the strategy search with a held-out test split, the
# LP lower bound, the auction-latency sweep, simulator throughput, the RL
# baseline over the gRPC environment, and the figures. About 55 minutes on 2 vCPUs.
set -euo pipefail
cd "$(dirname "$0")/.."
go build -o bin/livesched ./cmd/livesched
B=./bin/livesched
mkdir -p results
SEEDS=${SEEDS:-5}

echo "== baselines x scenarios x transports ($SEEDS seeds)"
$B sweep -strategies static,globalhot,hcdn,hcdn-noS1,hcdn-noS2,hcdn-noS3,hcdn-noS4,hcdn-noproactive \
  -scenarios shows,matches,auctions -transports 1,3,3:aware -seeds "$SEEDS" -out results/sweep.jsonl 2>/dev/null

echo "== offload knob grid (shows)"
$B grid -scenario shows -seeds 3 -out results/grid.jsonl

echo "== strategy search: stall SLO 0.1%, train weeks 11,12,14 / validate 13 / test 21,22,23 (proposer seeds 1-5)"
$B search -proposer mutate -budget 80 -stall-slo 0.001 -train 11,12,14 -val 13 -test 21,22,23 -seed 1 -out results/search | tail -8
for s in 2 3 4 5; do
  $B search -proposer mutate -budget 80 -stall-slo 0.001 -train 11,12,14 -val 13 -test 21,22,23 -seed $s -out results/search-seed$s | tail -3
done

echo "== each winner and HCDN defaults on every held-out week separately"
: > results/search-perweek.jsonl
row() { python3 -c "import json,sys; m=json.load(sys.stdin); print(json.dumps({'search':'$1','scenario':'$2','week':$3,'cost':m['cost'],'stall_ratio':m['stall_ratio']}))"; }
for d in search search-seed2 search-seed3 search-seed4 search-seed5 hcdn-defaults; do
  if [[ $d == hcdn-defaults ]]; then ARGS=(-strategy hcdn); else
    ARGS=(-params "$(python3 -c "import json;print(json.dumps(json.load(open('results/$d/report.json'))['winner']['candidate']['params']))")"); fi
  for sc in shows matches auctions; do for w in 21 22 23; do
    $B run "${ARGS[@]}" -scenario $sc -seed $w -json | row $d $sc $w >> results/search-perweek.jsonl
  done; done
done

echo "== auction cohort latency: scenario-blind vs scenario-aware scheduling and transport ($SEEDS seeds)"
$B sweep -strategies hcdn,hcdn-blind -scenarios auctions -transports 1,3:aware,3:aware:whole -seeds "$SEEDS" -out results/latency.jsonl 2>/dev/null

echo "== simulator throughput (one 12-hour shows week, HCDN, single process)"
$B run -strategy hcdn -scenario shows -seed 1 -json > results/throughput.json

echo "== RL baseline over the gRPC environment (REINFORCE, 5 training seeds)"
$B serve -addr 127.0.0.1:50071 > results/rl-server.log 2>&1 &
SRV=$!
sleep 1
mkdir -p results/rl
for pair in "1 2" "3 4" "5"; do   # two at a time on 2 vCPUs
  P=()
  for s in $pair; do
    BL=""; [[ $s == 1 ]] && BL="--baselines"
    python3 python/rl_baseline.py --addr 127.0.0.1:50071 --seed $s --episodes 90 $BL --out results/rl/seed$s.json > results/rl/seed$s.log 2>&1 &
    P+=($!)
  done
  wait "${P[@]}"
done
kill $SRV

echo "== LP lower bound (2 peak hours, average billing)"
$B demand -hours 2 -peak 30000 -streams 3000 -seed 5 -out results/demand-peak.json
python3 python/lpbound.py results/demand-peak.json > results/lpbound.json
python3 python/lpbound.py results/demand-peak.json 0.6 > results/lpbound-derated.json
for s in static globalhot hcdn; do
  $B run -strategy $s -hours 2 -peak 30000 -streams 3000 -seed 5 -json
done > results/lp-compare.jsonl
if [ -f results/search/report.json ]; then
  P=$(python3 -c "import json;r=json.load(open('results/search/report.json'));w=r.get('winner');print(json.dumps(w['candidate']['params']) if w else '')")
  [ -n "$P" ] && $B run -params "$P" -hours 2 -peak 30000 -streams 3000 -seed 5 -json >> results/lp-compare.jsonl
fi

python3 python/plot.py results
