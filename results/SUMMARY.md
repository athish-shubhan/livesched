# LiveSched results

All numbers are simulated (flow-level model, synthetic Twitch-like trace; see docs/DESIGN.md for the model and its validity limits). Cost is 95th-percentile billing of egress plus midgress, in arbitrary price units; it is reported relative to static nearest-edge mapping on the same traces. Means over seeds with 95% confidence intervals.

## Scenario: shows

| strategy | transport | cost vs static | midgress/egress | stall ratio | startup p50 / p95 ms | redirects | auction cohort g2g within 2.0 s |
|---|---|---|---|---|---|---|---|
| static | whole streams | +0.0% ± 0.0 | 0.177 | 0.164% ± 0.455 | 160 / 320 | 0% |  |
| static | Medley K=3 (naive cache) | +3.0% ± 0.5 | 0.222 | 0.164% ± 0.455 | 180 / 340 | 0% |  |
| static | Medley K=3 (substream-aware) | -2.2% ± 0.3 | 0.143 | 0.164% ± 0.455 | 180 / 340 | 0% |  |
| globalhot | whole streams | -4.6% ± 0.9 | 0.103 | 0.162% ± 0.451 | 240 / 240 | 83% |  |
| hcdn | whole streams | -8.5% ± 3.5 | 0.077 | 0.384% ± 0.263 | 100 / 240 | 85% |  |
| hcdn | Medley K=3 (naive cache) | -4.4% ± 3.4 | 0.141 | 0.384% ± 0.263 | 120 / 260 | 85% |  |
| hcdn | Medley K=3 (substream-aware) | -8.8% ± 3.5 | 0.073 | 0.384% ± 0.263 | 120 / 260 | 85% |  |
| hcdn-noS1 | whole streams | -9.2% ± 3.7 | 0.084 | 0.480% ± 0.296 | 100 / 240 | 84% |  |
| hcdn-noS2 | whole streams | -8.4% ± 3.7 | 0.077 | 0.384% ± 0.263 | 100 / 240 | 85% |  |
| hcdn-noS3 | whole streams | -8.7% ± 3.9 | 0.099 | 0.384% ± 0.263 | 100 / 240 | 85% |  |
| hcdn-noS4 | whole streams | -4.3% ± 1.3 | 0.079 | 0.114% ± 0.318 | 100 / 240 | 84% |  |
| hcdn-noproactive | whole streams | -8.5% ± 3.5 | 0.077 | 0.384% ± 0.263 | 240 / 240 | 85% |  |

## Scenario: matches

| strategy | transport | cost vs static | midgress/egress | stall ratio | startup p50 / p95 ms | redirects | auction cohort g2g within 2.0 s |
|---|---|---|---|---|---|---|---|
| static | whole streams | +0.0% ± 0.0 | 0.174 | 0.212% ± 0.569 | 160 / 320 | 0% |  |
| static | Medley K=3 (naive cache) | +2.9% ± 0.4 | 0.219 | 0.212% ± 0.569 | 180 / 340 | 0% |  |
| static | Medley K=3 (substream-aware) | -2.2% ± 0.2 | 0.141 | 0.212% ± 0.569 | 180 / 340 | 0% |  |
| globalhot | whole streams | -3.9% ± 0.7 | 0.106 | 0.208% ± 0.563 | 240 / 240 | 83% |  |
| hcdn | whole streams | -8.1% ± 4.0 | 0.076 | 0.456% ± 0.249 | 100 / 240 | 85% |  |
| hcdn | Medley K=3 (naive cache) | -4.1% ± 3.7 | 0.138 | 0.456% ± 0.249 | 120 / 260 | 85% |  |
| hcdn | Medley K=3 (substream-aware) | -8.4% ± 4.0 | 0.072 | 0.456% ± 0.249 | 120 / 260 | 85% |  |
| hcdn-noS1 | whole streams | -9.0% ± 3.6 | 0.084 | 0.537% ± 0.289 | 100 / 240 | 84% |  |
| hcdn-noS2 | whole streams | -8.4% ± 4.0 | 0.077 | 0.456% ± 0.249 | 100 / 240 | 85% |  |
| hcdn-noS3 | whole streams | -9.0% ± 4.0 | 0.098 | 0.456% ± 0.249 | 100 / 240 | 85% |  |
| hcdn-noS4 | whole streams | -3.9% ± 1.8 | 0.079 | 0.150% ± 0.415 | 100 / 240 | 84% |  |
| hcdn-noproactive | whole streams | -8.1% ± 4.0 | 0.076 | 0.456% ± 0.249 | 240 / 240 | 85% |  |

## Scenario: auctions

| strategy | transport | cost vs static | midgress/egress | stall ratio | startup p50 / p95 ms | redirects | auction cohort g2g within 2.0 s |
|---|---|---|---|---|---|---|---|
| static | whole streams | +0.0% ± 0.0 | 0.214 | 0.168% ± 0.467 | 160 / 360 | 0% | 97.1% |
| static | Medley K=3 (naive cache) | +3.5% ± 0.5 | 0.270 | 0.168% ± 0.467 | 180 / 380 | 0% | 0.0% |
| static | Medley K=3 (substream-aware) | -2.6% ± 0.3 | 0.173 | 0.168% ± 0.467 | 180 / 380 | 0% | 0.0% |
| globalhot | whole streams | -5.0% ± 1.5 | 0.123 | 0.165% ± 0.459 | 240 / 240 | 83% | 97.1% |
| hcdn | whole streams | -9.5% ± 3.4 | 0.096 | 0.387% ± 0.237 | 100 / 240 | 85% | 100.0% |
| hcdn | Medley K=3 (naive cache) | -4.5% ± 3.2 | 0.176 | 0.387% ± 0.237 | 120 / 260 | 85% | 0.0% |
| hcdn | Medley K=3 (substream-aware) | -9.8% ± 3.4 | 0.092 | 0.387% ± 0.237 | 120 / 260 | 85% | 0.0% |
| hcdn-noS1 | whole streams | -10.3% ± 3.5 | 0.103 | 0.491% ± 0.281 | 100 / 240 | 84% | 100.0% |
| hcdn-noS2 | whole streams | -9.4% ± 3.4 | 0.097 | 0.387% ± 0.237 | 100 / 240 | 85% | 100.0% |
| hcdn-noS3 | whole streams | -9.6% ± 3.5 | 0.119 | 0.387% ± 0.237 | 100 / 240 | 85% | 100.0% |
| hcdn-noS4 | whole streams | -5.3% ± 1.6 | 0.098 | 0.109% ± 0.303 | 100 / 240 | 84% | 97.4% |
| hcdn-noproactive | whole streams | -9.5% ± 3.4 | 0.096 | 0.387% ± 0.237 | 240 / 261 | 85% | 100.0% |

## Cost/stall frontier of the best-effort offload knobs (shows, seeds 1-3)

| offload_util | be_max_util | cost vs static | stall ratio |
|---|---|---|---|
| 1 | 0.4 | -3.3% | 0.000% |
| 0.9 | 0.4 | -3.6% | 0.000% |
| 0.9 | 0.5 | -3.8% | 0.000% |
| 0.9 | 0.6 | -4.0% | 0.000% |
| 0.8 | 0.4 | -5.2% | 0.000% |
| 0.8 | 0.5 | -5.6% | 0.000% |
| 0.8 | 0.6 | -5.8% | 0.000% |
| 0.7 | 0.5 | -5.9% | 0.000% |
| 0.7 | 0.6 | -6.6% | 0.000% |
| 0.6 | 0.6 | -7.2% | 0.000% |
| 0.5 | 0.5 | -7.8% | 0.000% |
| 0.5 | 0.6 | -8.6% | 0.000% |
| 0.4 | 0.6 | -9.2% | 0.000% |
| 0.4 | 0.7 | -10.1% | 0.145% |
| 0.4 | 0.8 | -11.1% | 0.733% |
| 0.4 | 0.9 | -12.2% | 1.725% |

## Strategy search (simulator-verified, held-out weeks)

Objective: minimum cost subject to a stall SLO of 0.10% of watch time and p95 startup within 25 ms of the reference. Proposer: `mutate`, 80 candidates on 3 training weeks (the first 20 random, then mutation of the best), top 8 re-checked on a validation week, winner reported on 3 held-out weeks per scenario. The reference is HCDN with our default thresholds. Wall time 281 s.

Winner (#75): `{"frozen": true, "cold": true, "hot": false, "offload": true, "proactive": true, "frozen_max": 4, "cold_max": 101, "hot_util": 0.3342107167052591, "offload_util": 0.32541047760042807, "max_util": 0.99, "protect_lowlat": true, "be_max_util": 0.6291635125833402}`

| test scenario | reference cost | winner cost | change | reference stall | winner stall | ref / winner startup p95 ms | auction cohort ok (ref / winner) |
|---|---|---|---|---|---|---|---|
| auctions | 76662 | 72710 | -5.2% | 0.689% | 0.720% | 240 / 240 | 93.8% / 92.7% |
| matches | 76062 | 72215 | -5.1% | 0.776% | 0.808% | 240 / 240 |  |
| shows | 74724 | 70519 | -5.6% | 0.656% | 0.709% | 240 / 240 |  |

## Strategy search over 5 proposer seeds

Same budget and split; only the proposer's random seed changes. Cost change of each seed's winner against HCDN defaults on the held-out weeks, and the search cost until the first candidate beat the reference on the training weeks at equal or better QoE (cost lower, stall ratio no higher, p95 startup within 25 ms).

Held-out stall is the mean stall ratio over the three scenarios' test weeks (HCDN defaults / winner).

| seed | winner | S1 (frozen streams) | shows | matches | auctions | held-out stall | first candidate beating HCDN (trial, simulations, seconds) | wall s |
|---|---|---|---|---|---|---|---|---|
| 1 | #75 | on | -5.6% | -5.1% | -5.2% | 0.71% / 0.75% | #2, 6, 7 | 281 |
| 2 | #68 | off | -5.4% | -4.4% | -5.1% | 0.71% / 1.00% | #10, 30, 35 | 286 |
| 3 | #80 | off | -1.9% | -2.3% | -1.8% | 0.71% / 1.04% | #24, 72, 75 | 272 |
| 4 | #77 | off | -5.0% | -4.5% | -4.5% | 0.71% / 0.99% | #7, 21, 22 | 263 |
| 5 | #65 | off | -6.1% | -5.5% | -5.9% | 0.71% / 1.00% | #2, 6, 7 | 250 |

Mean over seeds (95% CI): shows -4.8% ± 2.1, matches -4.4% ± 1.5, auctions -4.5% ± 2.0. First beat of HCDN: median trial 7, median 22 s.

### Held-out weeks one at a time

Each seed's winner and HCDN defaults re-run on every held-out week (mean of the three scenarios per week). Week 21 is the overloaded one (static mapping reaches 134% regional utilization).

| policy | week 21 stall | week 21 cost vs HCDN | week 22 stall | week 22 cost vs HCDN | week 23 stall | week 23 cost vs HCDN |
|---|---|---|---|---|---|---|
| HCDN defaults | 2.059% | +0.0% | 0.037% | +0.0% | 0.024% | +0.0% |
| seed 1 winner | 2.232% | -5.0% | 0.003% | -6.1% | 0.003% | -4.8% |
| seed 2 winner | 2.947% | -5.5% | 0.019% | -5.3% | 0.022% | -4.2% |
| seed 3 winner | 3.126% | -2.6% | 0.000% | -1.8% | 0.000% | -1.5% |
| seed 4 winner | 2.951% | -5.7% | 0.014% | -4.6% | 0.017% | -3.6% |
| seed 5 winner | 2.976% | -6.1% | 0.010% | -6.2% | 0.012% | -5.0% |

## Auction cohort: glass-to-glass latency vs cost (auctions scenario, 5 seeds)

Glass-to-glass latency of each auction bidder, sampled every 10 s of viewing: capture/encode/ingest, two relay hops, the low-latency player buffer, one more hop on a remote or best-effort edge, substream reassembly jitter, and drift behind live after stalls (the player catches up at 1.1x). Scenario-blind: HCDN treats bidders like everyone else (best-effort offload, cross-region consolidation). Scenario-aware: bidders stay on regular in-region edges, and with substreams on, auction streams travel whole.

| scheduling | transport | cost vs static | bidder g2g p50 ms | bidder g2g p95 ms | bidder g2g p99 ms | time within 2.0 s | stall ratio |
|---|---|---|---|---|---|---|---|
| scenario-blind | whole streams | -9.7% ± 3.3 | 1790 | 1900 ± 0 | 18760 | 98.3% | 0.396% |
| scenario-aware | whole streams | -9.5% ± 3.4 | 1790 | 1790 ± 0 | 1790 | 100.0% | 0.387% |
| scenario-blind | Medley K=3, aware | -10.0% ± 3.3 | 2090 | 2190 ± 0 | 18876 | 0.0% | 0.396% |
| scenario-aware | Medley K=3, aware | -9.8% ± 3.4 | 2090 | 2090 ± 0 | 2090 | 0.0% | 0.387% |
| scenario-aware | Medley K=3, aware (auctions whole) | -9.7% ± 3.4 | 1790 | 1790 ± 0 | 1790 | 100.0% | 0.387% |

## Simulator throughput

One 12-hour week of the shows scenario under HCDN, one process: 1,034,195 sessions, 16.4M viewer-minutes, 1.93 CPU-seconds: **8.5M simulated viewer-minutes per CPU-second** (32.1M sessions per CPU-minute).

## RL baseline over the gRPC environment

REINFORCE with a linear softmax policy over 12 settings of HCDN's best-effort offload (S4: start threshold x best-effort cap), chosen every 5 simulated minutes from regional utilization and time of day; everything else at HCDN defaults. 90 training episodes on weeks 11, 12, 14 (all scenarios), greedy policy evaluated on held-out weeks 21-23. Reward: -(average-billing cost rate / 1000) - 5000 x stalled share. 5 training seeds.

| test scenario | HCDN defaults: cost / stall | RL policy: cost change vs HCDN (mean ± 95% CI over seeds) | RL stall ratio |
|---|---|---|---|
| shows | 74724 / 0.656% | +0.1% ± 3.1 | 0.666% |
| matches | 76062 / 0.776% | +0.7% ± 2.9 | 0.760% |
| auctions | 76662 / 0.689% | +0.4% ± 3.0 | 0.685% |

Training time per seed: 4.1 min.

## Distance from the LP lower bound (2-hour instance, average billing)

LP relaxation bound: 40796 (best-effort capacity at nominal), 45601 (best-effort derated to its 60% floor). The bound ignores startup, stalls and latency, so the gap is the price of QoE as much as the strategy's inefficiency.

| strategy | average-billing cost | % of bound (nominal) | % of bound (derated) | stall ratio |
|---|---|---|---|---|
| static-nearest | 64770 | 159% | 142% | 0.000% |
| global-hotness(h=200) | 59116 | 145% | 130% | 0.000% |
| hcdn | 58182 | 143% | 128% | 0.000% |
| hcdn(custom) | 54555 | 134% | 120% | 0.001% |

