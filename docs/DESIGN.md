# LiveSched design

## 1. Question

A live-streaming CDN decides, for every viewer who joins a stream, which edge
node serves them. That choice sets three things at once: egress cost (which
node's bandwidth is billed), midgress cost (whether the stream must be copied
to one more edge and one more parent), and the viewer's quality of experience
(startup delay, stalls, glass-to-glass latency). ByteDance published two
systems that make this choice at scale in 2026: HCDN (four redirection
strategies over a hierarchical CDN) and Medley (splitting streams into K
substreams across L2 nodes). There is no public environment in which their
strategies can be compared or improved. LiveSched is one.

## 2. Model

Flow-level, discrete-time (1 s ticks), in Go. One 12-hour run with about a
million sessions takes about one second.

**Topology** (`sim.DefaultTopo`): 4 regions. Per region: 6 regular edges
(6 Gbps, price 1.0), 2 multihomed edges (4 Gbps, price 1.1, can serve any
region at a higher RTT), 3 best-effort edges (2.5 Gbps nominal, price 0.4,
capacity each minute uniform in 60-100% of nominal, an outage every 2 hours
on average lasting 60 s), and 3 L2 parents. Prices are relative units.

**Workload** (`workload.Synth`, a synthetic Twitch-like trace): 3,000
streams with Zipf(1.1) popularity, each with a home region holding 70% of its
audience (so a stream can be hot in one region and cold in another), region
weights 40/30/20/10%, diurnal load peaking at 30,000 concurrent viewers,
lognormal sessions (median 10 minutes), 60% of joins from the recommendation
feed. The top 2% of streams stay live all run; the rest have 1-5 hour live
windows. A loader for the public Twitch dataset (Rappaz et al., RecSys '21)
is included and tested on a generated file in the same format; geography,
bitrate and home region are synthetic for it too, because the dataset has
none.

**Scenarios** (overlays on the same trace):

- *shows*: the base trace;
- *matches*: every 30 minutes, 4 pairs of popular streams battle for 5
  minutes; both audiences grow together (correlated), with a 3x surge of joins
  in the final 30 s when gifts decide the winner;
- *auctions*: about 5% of mid-popularity streams run 2-minute countdowns every
  10 minutes with a 2x surge in the final 30 s (Extended mode resets the
  timer on late bids); 30% of their viewers are bidders with a low-latency
  player (0.6 s buffer) and a 2.0 s glass-to-glass bound.

**Costs**: egress is billed per edge at the 95th percentile of 5-minute
averages; midgress is one stream copy per (edge, stream) pair with viewers
(L2 to edge) plus one per (L2, stream) pair (origin to L2), billed per region
the same way. Average-billing cost is also reported (comparable with the LP
bound).

**Transport**: whole streams, or Medley-style K substreams placed on K L2
nodes. With substream-aware caching each substream is pulled once per
region; without it, a miss on any substream pulls the full stream at that L2,
which is HCDN's observation about substreams and patch streams.

**QoE**:

- startup = connection setup (3 RTT) + redirection penalty (4 RTT to the
  default edge, HTTP 302 style) + cache-miss penalty (edge to L2 RTT, L2 to
  origin RTT on a second miss, a GOP wait) + first frame (100 ms). Proactive
  pre-connection (HCDN's "proactive QoE assurance") removes setup and
  redirection for feed joins;
- stalls: each edge has a fluid buffer per latency class; when demand exceeds
  the edge's capacity this minute, every viewer on it receives the same
  fraction of their bitrate and the buffer drains; an empty buffer is stall
  time. A best-effort outage forces every viewer on it to reconnect, which is
  counted as stall;
- glass-to-glass latency per auction bidder: a fixed part, capture/encode/
  ingest (1.0 s) + two relay hops (0.1 s each) + the low-latency player buffer
  (0.6 s), + one more hop (0.1 s) on a cross-region multihomed or a
  best-effort edge, + 0.3 s reassembly jitter with substreams; plus a dynamic
  part, the drift behind live: stall time and the startup of a forced
  reconnect add to it, and the player's 1.1x catch-up playback removes 0.1 s
  per second. Sampled every 10 s of viewing; the bound is 2.0 s. The fixed
  part makes p50 and p95 coarse (1.8 s, 2.1 s with substreams); only the tail
  is dynamic.

## 3. Strategies

- **static nearest-edge**: DNS-style, hash of the session over the region's
  regular edges. No redirection, no awareness of streams or load.
- **global hotness**: globally hot streams to the least-loaded edge, globally
  cold ones pinned to one edge per region by rendezvous hashing. This is the
  prior approach HCDN improves on.
- **HCDN**, re-implemented from the paper's description, with each strategy
  switchable for ablation:
  - S1 frozen streams (a handful of viewers in a region) go to multihomed edges, consolidated across regions;
  - S2 cold streams are aggregated onto one regular edge per region;
  - S3 hot streams go to the regular edge where they are already hottest (moving from locally cold nodes to hot ones);
  - S4 when a region's regular edges run hot, hot streams are offloaded to cheap best-effort edges;
  - proactive pre-connection for feed joins;
  - the auction cohort is kept off best-effort and remote edges.

  The paper does not publish thresholds; ours are guesses (`DefaultHCDN`).
- **Medley** is a transport mode, combinable with any strategy.
- **Policy as code**: `-external` runs a strategy in any language as a
  sandboxed process (no network namespace, CPU and memory limits, empty
  environment) that answers one JSON observation per join. A hung or crashing
  candidate is cut off and the run falls back to the default edge.

## 4. Strategy search

`livesched search` is simulator-verified search in the ADRS style: a proposer
submits candidates, the simulator scores them, and nothing is trusted until
it has been re-checked on traces it has not seen.

- objective: minimum cost subject to a stall SLO (0.1% of watch time) and p95
  startup within 25 ms of the reference;
- splits: 3 training weeks (seeds), 1 validation week to choose among the top
  8, 3 held-out test weeks per scenario, including scenarios not used in
  training (matches, auctions);
- proposers: `mutate` (20 random candidates, then (mu + lambda) mutation of the
  best; no model) and `llm`, which builds a
  prompt from the parameter schema and the table of past trials and calls a
  `Model` interface. The only shipped implementation is `CommandModel`, which
  pipes the prompt to any command you configure, so no provider is built in.
  Budgets: model calls and approximate tokens. The proposer never touches the
  simulator; it reads metrics of past trials and submits candidates;
- the current search space is HCDN's parameters. Searching over code
  (candidate strategies written by a model) uses the same harness through
  `-external`; that loop is wired but needs a model to run.

Five runs with different proposer seeds cut held-out cost 1.8-6.1% against
HCDN defaults (means -4.4% to -4.8% by scenario). Re-running each winner per
held-out week showed that the split has a hole: no training or validation
week is overloaded, and in the one overloaded test week (21) the four winners
that switched S1 off stall 43-52% more than HCDN, while on the normal weeks
they stall less. A worst-week stall constraint or an overload week in
validation would have rejected them.

## 5. Environment API and the RL baseline

`livesched serve` exposes the engine as a gRPC service
(`env/proto/livesched_env.proto`): `Reset` builds a synthetic week for a
scenario and seed, `Step` applies HCDN's knobs for the next control interval
(default 300 s) and returns per-region utilization of regular, multihomed and
best-effort edges, the interval's average-billing cost rate and stalled
share, and the p95 latency of attached bidders, with reward
-(cost rate / scale) - penalty x stalled share; `Close` returns the episode's
full metrics, including the 95th-percentile-billing cost that per-step
rewards only approximate. The Python client is `env/python/livesched_env.py`.

`python/rl_baseline.py` is REINFORCE with a linear softmax policy over 12
settings of S4's two knobs (offload start threshold x best-effort cap),
features from utilization and time of day, 90 training episodes on the
search's training weeks across all three scenarios, greedy evaluation on the
held-out weeks. Over 5 seeds it was +0.1% to +0.7% (± 3%) against HCDN
defaults: each seed collapsed to an almost constant action, and only one
found the cheap region the search finds. The per-step reward is linear in
cost while billing is a 95th percentile over the episode, which weakens
credit assignment; 90 episodes are also few. It is reported as a negative
result.

## 6. Checks that the model behaves like the papers say

These are directional consistency checks, not validation against
ByteDance's data (which is not public). Each is a test in
`sim/engine_test.go`:

| check | expectation | status |
|---|---|---|
| substreams without substream-aware caching | more internal traffic than whole streams (HCDN: 1.72-5.76x on their traces) | holds: midgress/egress 0.141 vs 0.077 under HCDN, cost +3% for static |
| substream-aware caching | less origin-to-L2 traffic than whole streams (Medley) | holds: midgress/egress 0.073 vs 0.077 |
| aggregation (S1, S2, S3) | lower midgress/egress than static mapping | holds: 0.077 vs 0.177 |
| redirection | adds startup delay unless pre-connected (HCDN's motivation for proactive QoE assurance) | holds: p50 240 ms redirected vs 100 ms pre-connected |
| accounting | every counter returns to zero after all sessions end | holds |
| determinism | same seed, same result | holds |

## 7. Validity threats

- **Synthetic geography, prices and capacities.** Absolute costs mean nothing;
  only relative changes on the same traces do, and they depend on the price
  ratios chosen (best-effort at 0.4, midgress at 0.6 of regular egress).
- **Fluid node model.** All viewers on a node share its deficit; no TCP
  dynamics, no ABR (one bitrate per stream), no per-viewer throughput
  variation (Puffer's traces would calibrate this; not used yet).
- **Stall variance across weeks is large** (95% CIs in the tables are often as
  wide as the mean), so stall comparisons need several seeds.
- **The HCDN re-implementation is ours.** It follows the paper's description;
  thresholds and tie-breaking are not published.
- **Search space**: parameters, not code, in the results shown.
- **Search split**: the training and validation weeks contain no overload, so
  winners were never tested against it before the held-out weeks (see
  section 4).
- **Glass-to-glass model**: fixed per-hop and buffer constants; the substream
  result (every bidder past 2.0 s) follows from the assumed 0.3 s reassembly
  cost.

## 8. Design notes

- *Why flow level?* The decision is per viewer join and the costs are
  per-node bandwidth; packet-level detail would cost 1000x the runtime and
  change the cost terms little. What is lost is in section 6.
- *How did you validate without ByteDance data?* Directional checks against
  the published findings (section 5), determinism and accounting tests, and
  confidence intervals over synthetic weeks. Not a claim of fidelity.
- *How do you stop the search from overfitting?* Separate train, validation
  and test weeks, a test on scenarios never trained on, and reporting the
  test result even when the winner loses. It still overfit in one way: no
  training week was overloaded, and 4 of 5 winners stalled 43-52% more than
  HCDN in the overloaded test week. Per-week test results, not means, showed
  it.
- *Why HTTP redirection rather than DNS or anycast?* Redirection is decided
  per request with the stream known, so it can aggregate by stream; DNS sees
  only the resolver, anycast only the network path. The price is the extra
  round trips, which is why pre-connection matters.
- *Why did RL lose to a simple search?* The search scores whole episodes on
  the real objective (95th-percentile billing, stall SLO); REINFORCE learns
  from per-step rewards that only approximate it, with high-variance
  gradients, and every seed collapsed to a near-constant action. With a
  12-action space, evaluating every constant policy would have been cheaper
  than training.
- *Why are bidders' p50 and p95 latencies so coarse?* They are the model's
  fixed components; only drift after stalls and outages is dynamic, which is
  why the p99 (18.8 s scenario-blind vs 1.8 s aware) is the informative
  number.
