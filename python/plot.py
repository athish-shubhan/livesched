#!/usr/bin/env python3
"""Figures and results/SUMMARY.md for LiveSched."""
import collections
import json
import os
import statistics as st
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

R = sys.argv[1] if len(sys.argv) > 1 else "results"


def jl(path):
    if not os.path.exists(path):
        return []
    return [json.loads(l) for l in open(path) if l.strip()]


def ci(xs):
    """mean and 95% half-width (t, small n)."""
    if len(xs) < 2:
        return (xs[0] if xs else float("nan")), 0.0
    t = {2: 12.71, 3: 4.30, 4: 3.18, 5: 2.78, 6: 2.57, 7: 2.45, 8: 2.36, 9: 2.31, 10: 2.26}.get(len(xs), 2.0)
    return st.mean(xs), t * st.stdev(xs) / len(xs) ** 0.5


md = ["# LiveSched results", "",
      "All numbers are simulated (flow-level model, synthetic Twitch-like trace; see docs/DESIGN.md for the model and its validity limits). "
      "Cost is 95th-percentile billing of egress plus midgress, in arbitrary price units; it is reported relative to static nearest-edge mapping on the same traces. "
      "Means over seeds with 95% confidence intervals.", ""]

# ---------------------------------------------------------------- sweep
sw = jl(os.path.join(R, "sweep.jsonl"))
if sw:
    cells = collections.defaultdict(list)
    for r in sw:
        cells[(r["workload"], r["base_strategy"], r["substreams"], r["aware"])].append(r)
    static = {(w, s): r for (w, b, k, a), rs in cells.items() if b == "static" and k == 1 for r in rs for s in [r["seed"]]}

    def rel(r):
        base = static.get((r["workload"], r["seed"]))
        return r["cost"] / base["cost"] if base else float("nan")

    order = ["static", "globalhot", "hcdn", "hcdn-noS1", "hcdn-noS2", "hcdn-noS3", "hcdn-noS4", "hcdn-noproactive"]
    for scen in ["shows", "matches", "auctions"]:
        md += [f"## Scenario: {scen}", "",
               "| strategy | transport | cost vs static | midgress/egress | stall ratio | startup p50 / p95 ms | redirects | auction cohort g2g within 2.0 s |",
               "|---|---|---|---|---|---|---|---|"]
        for b in order:
            for k, a in [(1, False), (3, False), (3, True)]:
                rs = cells.get((scen, b, k, a))
                if not rs or (k > 1 and b not in ("static", "hcdn")):
                    continue
                c, h = ci([rel(r) for r in rs])
                stl, sh = ci([r["stall_ratio"] * 100 for r in rs])
                tr = "whole streams" if k == 1 else f"Medley K={k}" + (" (substream-aware)" if a else " (naive cache)")
                ll = st.mean([r["auction_cohort_g2g_within_bound"] for r in rs]) if scen == "auctions" else None
                md.append(f"| {b} | {tr} | {100*(c-1):+.1f}% ± {100*h:.1f} | {st.mean([r['midgress_egress_ratio'] for r in rs]):.3f} | "
                          f"{stl:.3f}% ± {sh:.3f} | {st.mean([r['startup_p50_ms'] for r in rs]):.0f} / {st.mean([r['startup_p95_ms'] for r in rs]):.0f} | "
                          f"{100*st.mean([r['redirect_share'] for r in rs]):.0f}% | {'' if ll is None else f'{100*ll:.1f}%'} |")
        md.append("")

    # bar chart: cost vs static per scenario for main strategies
    fig, ax = plt.subplots(figsize=(8, 3.8))
    mains = [("globalhot", 1, False, "global hotness"), ("hcdn", 1, False, "HCDN (4 strategies)"),
             ("hcdn", 3, False, "HCDN + Medley K=3, naive cache"), ("hcdn", 3, True, "HCDN + Medley K=3, aware")]
    scen = ["shows", "matches", "auctions"]
    w = 0.2
    for i, (b, k, a, lab) in enumerate(mains):
        vals, errs = [], []
        for s in scen:
            c, h = ci([rel(r) for r in cells.get((s, b, k, a), [])] or [float("nan")])
            vals.append(100 * (c - 1))
            errs.append(100 * h)
        ax.bar([j + i * w for j in range(3)], vals, w, yerr=errs, label=lab, capsize=2)
    ax.axhline(0, color="k", lw=0.8)
    ax.set_xticks([j + 1.5 * w for j in range(3)])
    ax.set_xticklabels(scen)
    ax.set_ylabel("cost vs static nearest-edge (%)")
    ax.set_title("Simulated bandwidth cost (95th-pct billing), 5 synthetic weeks")
    ax.legend(fontsize=7)
    ax.grid(axis="y", alpha=0.3)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "cost_by_strategy.png"), dpi=140)

# ---------------------------------------------------------------- grid / pareto
gr = jl(os.path.join(R, "grid.jsonl"))
trials = jl(os.path.join(R, "search", "trials.jsonl"))
rep = json.load(open(os.path.join(R, "search", "report.json"))) if os.path.exists(os.path.join(R, "search", "report.json")) else None
if gr:
    pts = collections.defaultdict(list)
    for r in gr:
        pts[(r["offload_util"], r["be_max_util"])].append(r["metrics"])
    static_shows = [r for r in sw if r["base_strategy"] == "static" and r["workload"] == "shows" and r["substreams"] == 1 and r["seed"] <= 3]
    base = st.mean([r["cost"] for r in static_shows]) if static_shows else None
    xs, ys, lab = [], [], []
    for (o, b), ms in sorted(pts.items()):
        xs.append(100 * st.mean([m["stall_ratio"] for m in ms]))
        ys.append(st.mean([m["cost"] for m in ms]))
        lab.append((o, b))
    if base:
        ys = [100 * (y / base - 1) for y in ys]
    # Pareto front
    front = []
    for i in sorted(range(len(xs)), key=lambda i: (xs[i], ys[i])):
        if not front or ys[i] < ys[front[-1]]:
            front.append(i)
    fig, ax = plt.subplots(figsize=(7, 4.5))
    ax.scatter(xs, ys, s=14, c="#9aa5b1", label="HCDN with other offload_util / be_max_util")
    ax.plot([xs[i] for i in front], [ys[i] for i in front], "-o", ms=4, c="#2b6cb0", label="Pareto front")
    d = [i for i, l in enumerate(lab) if l == (0.7, 0.9)]
    if d:
        ax.scatter([xs[d[0]]], [ys[d[0]]], s=60, marker="s", c="#e8a33d", label="HCDN defaults (0.7, 0.9)", zorder=5)
    ax.set_xlabel("stall ratio (%)")
    ax.set_ylabel("cost vs static (%)" if base else "cost")
    ax.set_title("Cost vs stalls: when to offload to best-effort edges (shows, 3 seeds)")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=7)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "pareto_offload.png"), dpi=140)
    md += ["## Cost/stall frontier of the best-effort offload knobs (shows, seeds 1-3)", "",
           "| offload_util | be_max_util | cost vs static | stall ratio |", "|---|---|---|---|"]
    for i in front:
        md.append(f"| {lab[i][0]} | {lab[i][1]} | {ys[i]:+.1f}% | {xs[i]:.3f}% |")
    md.append("")

if trials:
    fig, ax = plt.subplots(figsize=(7, 4))
    ref = trials[0]["train"]["cost"]
    best, bx = [], []
    b = float("inf")
    for t in trials:
        if t["train"]["feasible"]:
            b = min(b, t["train"]["cost"])
        best.append(100 * (b / ref - 1))
        bx.append(t["n"])
    ax.scatter([t["n"] for t in trials], [100 * (t["train"]["cost"] / ref - 1) for t in trials],
               c=["#2bb3a3" if t["train"]["feasible"] else "#c75b7a" for t in trials], s=14)
    ax.plot(bx, best, c="k", lw=1, label="best feasible so far")
    ax.set_xlabel("candidate #")
    ax.set_ylabel("train cost vs HCDN defaults (%)")
    ax.set_ylim(-20, 20)
    ax.set_title("Strategy search (green: meets the stall/startup SLO, red: violates it)")
    ax.grid(alpha=0.3)
    ax.legend(fontsize=7)
    fig.tight_layout()
    fig.savefig(os.path.join(R, "search_progress.png"), dpi=140)

if rep:
    md += ["## Strategy search (simulator-verified, held-out weeks)", "",
           f"Objective: minimum cost subject to a stall SLO of {100*rep.get('stall_slo', 0):.2f}% of watch time and p95 startup within 25 ms of the reference. "
           f"Proposer: `{rep['proposer']}`, {rep['budget']} candidates on 3 training weeks (the first 20 random, then mutation of the best), top 8 re-checked on a validation week, "
           f"winner reported on 3 held-out weeks per scenario. The reference is HCDN with our default thresholds. Wall time {rep['wall_sec']:.0f} s.", ""]
    if rep.get("winner"):
        w = rep["winner"]
        md += [f"Winner (#{w['n']}): `{json.dumps(w['candidate']['params'])}`", "",
               "| test scenario | reference cost | winner cost | change | reference stall | winner stall | ref / winner startup p95 ms | auction cohort ok (ref / winner) |",
               "|---|---|---|---|---|---|---|---|"]
        for sc, t in rep["test"].items():
            r_, w_ = t["reference"], t["winner"]
            llc = f"{100*r_['auction_cohort_g2g_within_bound']:.1f}% / {100*w_['auction_cohort_g2g_within_bound']:.1f}%" if sc == "auctions" else ""
            md.append(f"| {sc} | {r_['cost']:.0f} | {w_['cost']:.0f} | {t['cost_change_pct']:+.1f}% | {100*r_['stall_ratio']:.3f}% | {100*w_['stall_ratio']:.3f}% | "
                      f"{r_['startup_p95_ms']:.0f} / {w_['startup_p95_ms']:.0f} | {llc} |")
    else:
        md.append("No candidate stayed feasible on the validation week; the reference stands.")
    md.append("")

# ---------------------------------------------------------------- search over 5 proposer seeds
reps = []
for d in sorted(os.listdir(R)):
    if d == "search" or d.startswith("search-seed"):
        f = os.path.join(R, d, "report.json")
        if os.path.exists(f):
            reps.append(json.load(open(f)))
if len(reps) > 1:
    md += ["## Strategy search over 5 proposer seeds", "",
           "Same budget and split; only the proposer's random seed changes. Cost change of each seed's winner against HCDN defaults on the held-out weeks, "
           "and the search cost until the first candidate beat the reference on the training weeks at equal or better QoE (cost lower, stall ratio no higher, "
           "p95 startup within 25 ms).", "",
           "Held-out stall is the mean stall ratio over the three scenarios' test weeks (HCDN defaults / winner).", "",
           "| seed | winner | S1 (frozen streams) | shows | matches | auctions | held-out stall | first candidate beating HCDN (trial, simulations, seconds) | wall s |",
           "|---|---|---|---|---|---|---|---|---|"]
    per = collections.defaultdict(list)
    fb_trials, fb_sec = [], []
    for r in reps:
        w = r.get("winner")
        cells, rst, wst = [], [], []
        for sc in ["shows", "matches", "auctions"]:
            t = (r.get("test") or {}).get(sc)
            if t:
                per[sc].append(t["cost_change_pct"])
                cells.append(f"{t['cost_change_pct']:+.1f}%")
                rst.append(t["reference"]["stall_ratio"]); wst.append(t["winner"]["stall_ratio"])
            else:
                cells.append("")
        stall = f"{100*st.mean(rst):.2f}% / {100*st.mean(wst):.2f}%" if rst else ""
        s1 = ("on" if w["candidate"]["params"].get("frozen") else "off") if w else ""
        fb = r.get("first_beats_reference")
        fbs = f"#{fb['trial']}, {fb['simulations']}, {fb['elapsed_sec']:.0f}" if fb else "none"
        if fb:
            fb_trials.append(fb["trial"]); fb_sec.append(fb["elapsed_sec"])
        md.append(f"| {r.get('seed', 1)} | {'#'+str(w['n']) if w else 'none'} | {s1} | {' | '.join(cells)} | {stall} | {fbs} | {r['wall_sec']:.0f} |")
    line = []
    for sc in ["shows", "matches", "auctions"]:
        if per[sc]:
            m, h = ci(per[sc])
            line.append(f"{sc} {m:+.1f}% ± {h:.1f}")
    md += ["", "Mean over seeds (95% CI): " + ", ".join(line) + "." +
           (f" First beat of HCDN: median trial {st.median(fb_trials):.0f}, median {st.median(fb_sec):.0f} s." if fb_trials else ""), ""]
    pw = jl(os.path.join(R, "search-perweek.jsonl"))
    if pw:
        by = collections.defaultdict(dict)
        for r in pw:
            by[r["search"]][(r["scenario"], r["week"])] = r
        ref = by.get("hcdn-defaults", {})
        weeks = sorted({r["week"] for r in pw})
        md += ["### Held-out weeks one at a time", "",
               "Each seed's winner and HCDN defaults re-run on every held-out week (mean of the three scenarios per week). "
               "Week 21 is the overloaded one (static mapping reaches 134% regional utilization).", "",
               "| policy | " + " | ".join(f"week {w} stall | week {w} cost vs HCDN" for w in weeks) + " |",
               "|---|" + "---|---|" * len(weeks)]
        names = ["hcdn-defaults"] + sorted(k for k in by if k != "hcdn-defaults")
        for n in names:
            cells = []
            for w in weeks:
                rows = [by[n][(sc, w)] for sc in ("shows", "matches", "auctions") if (sc, w) in by[n]]
                stall = st.mean([x["stall_ratio"] for x in rows])
                cost = st.mean([100 * (x["cost"] / ref[(x["scenario"], w)]["cost"] - 1) for x in rows])
                cells += [f"{100*stall:.3f}%", f"{cost:+.1f}%"]
            label = "HCDN defaults" if n == "hcdn-defaults" else ("seed 1 winner" if n == "search" else n.replace("search-seed", "seed ") + " winner")
            md.append(f"| {label} | " + " | ".join(cells) + " |")
        md.append("")

# ---------------------------------------------------------------- auction cohort latency
lat = jl(os.path.join(R, "latency.jsonl"))
if lat:
    cells = collections.defaultdict(list)
    for r in lat:
        cells[(r["base_strategy"], r["substreams"], r["aware"], r.get("whole_auctions", False))].append(r)
    static_cost = {}
    for r in sw:
        if r["workload"].startswith("auction") and r["base_strategy"] == "static" and r["substreams"] == 1:
            static_cost[r["seed"]] = r["cost"]
    md += ["## Auction cohort: glass-to-glass latency vs cost (auctions scenario, 5 seeds)", "",
           "Glass-to-glass latency of each auction bidder, sampled every 10 s of viewing: capture/encode/ingest, two relay hops, the low-latency player buffer, "
           "one more hop on a remote or best-effort edge, substream reassembly jitter, and drift behind live after stalls (the player catches up at 1.1x). "
           "Scenario-blind: HCDN treats bidders like everyone else (best-effort offload, cross-region consolidation). "
           "Scenario-aware: bidders stay on regular in-region edges, and with substreams on, auction streams travel whole.", "",
           "| scheduling | transport | cost vs static | bidder g2g p50 ms | bidder g2g p95 ms | bidder g2g p99 ms | time within 2.0 s | stall ratio |",
           "|---|---|---|---|---|---|---|---|"]
    order = [("hcdn-blind", 1, False, False), ("hcdn", 1, False, False), ("hcdn-blind", 3, True, False), ("hcdn", 3, True, False), ("hcdn", 3, True, True)]
    for key in order:
        rs = cells.get(key)
        if not rs:
            continue
        b, k, a, wh = key
        rel = [r["cost"] / static_cost[r["seed"]] for r in rs if r["seed"] in static_cost]
        c, h = ci(rel) if rel else (float("nan"), 0)
        tr = "whole streams" if k == 1 else "Medley K=3, aware" + (" (auctions whole)" if wh else "")
        sched = "scenario-blind" if b == "hcdn-blind" else "scenario-aware"
        g50, g95, g99 = (st.mean([r.get(f"auction_cohort_g2g_{q}_ms", 0) for r in rs]) for q in ("p50", "p95", "p99"))
        h95 = ci([r.get("auction_cohort_g2g_p95_ms", 0) for r in rs])[1]
        md.append(f"| {sched} | {tr} | {100*(c-1):+.1f}% ± {100*h:.1f} | {g50:.0f} | {g95:.0f} ± {h95:.0f} | {g99:.0f} | "
                  f"{100*st.mean([r['auction_cohort_g2g_within_bound'] for r in rs]):.1f}% | {100*st.mean([r['stall_ratio'] for r in rs]):.3f}% |")
    md.append("")

# ---------------------------------------------------------------- throughput
tp = os.path.join(R, "throughput.json")
if os.path.exists(tp):
    m = json.load(open(tp))
    md += ["## Simulator throughput", "",
           f"One 12-hour week of the shows scenario under HCDN, one process: {m['sessions']:,} sessions, {m['watch_sec']/60/1e6:.1f}M viewer-minutes, "
           f"{m['cpu_sec']:.2f} CPU-seconds: **{m['viewer_minutes_per_cpu_sec']/1e6:.1f}M simulated viewer-minutes per CPU-second** "
           f"({m['sessions_per_cpu_sec']*60/1e6:.1f}M sessions per CPU-minute).", ""]

# ---------------------------------------------------------------- RL baseline
rl = []
rd = os.path.join(R, "rl")
if os.path.isdir(rd):
    for f in sorted(os.listdir(rd)):
        if f.endswith(".json"):
            rl.append(json.load(open(os.path.join(rd, f))))
if rl:
    base = {}
    for r in rl:
        for t in r["test"]:
            if "hcdn_cost" in t:
                base[(t["scenario"], t["week"])] = (t["hcdn_cost"], t["hcdn_stall_ratio"])
    md += ["## RL baseline over the gRPC environment", "",
           f"REINFORCE with a linear softmax policy over 12 settings of HCDN's best-effort offload (S4: start threshold x best-effort cap), "
           f"chosen every 5 simulated minutes from regional utilization and time of day; everything else at HCDN defaults. "
           f"{rl[0]['episodes']} training episodes on weeks 11, 12, 14 (all scenarios), greedy policy evaluated on held-out weeks 21-23. "
           f"Reward: -(average-billing cost rate / 1000) - {rl[0]['stall_penalty']:.0f} x stalled share. {len(rl)} training seeds.", "",
           "| test scenario | HCDN defaults: cost / stall | RL policy: cost change vs HCDN (mean ± 95% CI over seeds) | RL stall ratio |", "|---|---|---|---|"]
    for sc in ["shows", "matches", "auctions"]:
        ch, stl = [], []
        hc, hs = [], []
        for r in rl:
            cs, ss = [], []
            for t in r["test"]:
                if t["scenario"] == sc and (sc, t["week"]) in base:
                    b = base[(sc, t["week"])]
                    cs.append(t["cost"] / b[0] - 1)
                    ss.append(t["stall_ratio"])
            if cs:
                ch.append(100 * st.mean(cs)); stl.append(100 * st.mean(ss))
        for (s_, w_), b in base.items():
            if s_ == sc:
                hc.append(b[0]); hs.append(100 * b[1])
        if ch:
            m, h = ci(ch)
            md.append(f"| {sc} | {st.mean(hc):.0f} / {st.mean(hs):.3f}% | {m:+.1f}% ± {h:.1f} | {st.mean(stl):.3f}% |")
    md.append(f"\nTraining time per seed: {st.mean([r['train_sec'] for r in rl])/60:.1f} min.")
    md.append("")

lp = os.path.join(R, "lpbound.json")
if os.path.exists(lp):
    b1 = json.load(open(lp))["lp_bound_avg_billing_cost"]
    b2 = json.load(open(os.path.join(R, "lpbound-derated.json")))["lp_bound_avg_billing_cost"]
    md += ["## Distance from the LP lower bound (2-hour instance, average billing)", "",
           f"LP relaxation bound: {b1:.0f} (best-effort capacity at nominal), {b2:.0f} (best-effort derated to its 60% floor). "
           "The bound ignores startup, stalls and latency, so the gap is the price of QoE as much as the strategy's inefficiency.", "",
           "| strategy | average-billing cost | % of bound (nominal) | % of bound (derated) | stall ratio |", "|---|---|---|---|---|"]
    for m in jl(os.path.join(R, "lp-compare.jsonl")):
        md.append(f"| {m['strategy']} | {m['avg_billing_cost']:.0f} | {100*m['avg_billing_cost']/b1:.0f}% | {100*m['avg_billing_cost']/b2:.0f}% | {100*m['stall_ratio']:.3f}% |")
    md.append("")

open(os.path.join(R, "SUMMARY.md"), "w").write("\n".join(md) + "\n")
print("\n".join(md))
