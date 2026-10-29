#!/usr/bin/env python3
"""RL baseline over the gRPC environment: REINFORCE with a linear softmax policy.

The agent controls HCDN's best-effort offload (S4) every 5 simulated
minutes: when to start offloading (regional utilization threshold) and how
full best-effort edges may get. Everything else stays at HCDN defaults. It
trains on the same weeks as the strategy search (11, 12, 14; all three
scenarios) and is evaluated greedily on the held-out weeks (21, 22, 23).

Reward per step: -(average-billing cost rate / 1000) - stall_penalty * stalled share of watch time.
The test metric is the episode's true 95th-percentile-billing cost and stall ratio.

    livesched serve -addr 127.0.0.1:50071 &
    python3 python/rl_baseline.py --addr 127.0.0.1:50071 --seed 1 --episodes 90 --out results/rl/seed1.json
"""
import argparse
import json
import math
import os
import sys
import time

import numpy as np

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "env", "python"))
from livesched_env import LiveSchedEnv  # noqa: E402

OFFLOAD = [0.33, 0.5, 0.7, 1.01]   # 1.01: never offload
BE_MAX = [0.6, 0.75, 0.9]
ACTIONS = [(o, b) for o in OFFLOAD for b in BE_MAX]
TRAIN = [(sc, w) for w in (11, 12, 14) for sc in ("shows", "matches", "auctions")]
TEST = [(sc, w) for w in (21, 22, 23) for sc in ("shows", "matches", "auctions")]


def features(o):
    reg = np.array(o.regular_util, dtype=float)
    be = np.array(o.best_effort_util, dtype=float)
    p = o.t / max(o.horizon, 1)
    return np.concatenate([[1.0, reg.mean(), reg.max(), be.mean(), p, math.sin(2 * math.pi * p), math.cos(2 * math.pi * p)], reg])


def softmax(z):
    z = z - z.max()
    e = np.exp(z)
    return e / e.sum()


class Adam:
    def __init__(self, shape, lr):
        self.m, self.v, self.t, self.lr = np.zeros(shape), np.zeros(shape), 0, lr

    def step(self, g):
        self.t += 1
        self.m = 0.9 * self.m + 0.1 * g
        self.v = 0.999 * self.v + 0.001 * g * g
        mh, vh = self.m / (1 - 0.9 ** self.t), self.v / (1 - 0.999 ** self.t)
        return self.lr * mh / (np.sqrt(vh) + 1e-8)


def episode(env, W, scenario, week, rng, greedy, args, fixed=None):
    o = env.reset(scenario=scenario, seed=week, hours=args.hours, peak=args.peak, step_seconds=300,
                  stall_penalty=args.stall_penalty, cost_scale=1000.0)
    xs, acts, rews = [], [], []
    done = False
    while not done:
        x = features(o)
        if fixed is not None:
            a = fixed
        else:
            pi = softmax(W @ x)
            a = int(pi.argmax()) if greedy else int(rng.choice(len(ACTIONS), p=pi))
        off, be = ACTIONS[a]
        o, r, done, _ = env.step({"offload_util": off, "be_max_util": be, "offload": True})
        xs.append(x)
        acts.append(a)
        rews.append(r)
    m = env.close()
    return xs, acts, rews, m


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--addr", default="127.0.0.1:50071")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--episodes", type=int, default=90)
    ap.add_argument("--lr", type=float, default=0.05)
    ap.add_argument("--stall-penalty", type=float, default=5000.0)
    ap.add_argument("--hours", type=float, default=12)
    ap.add_argument("--peak", type=int, default=30000)
    ap.add_argument("--out", default="")
    ap.add_argument("--baselines", action="store_true", help="also evaluate HCDN defaults as a fixed action on the test weeks")
    args = ap.parse_args()
    rng = np.random.default_rng(args.seed)
    env = LiveSchedEnv(args.addr)
    F = len(features(env.reset(seed=1, hours=0.1, peak=1000)))
    env.close()
    W = rng.normal(0, 0.01, size=(len(ACTIONS), F))
    opt = Adam(W.shape, args.lr)
    base = None
    t0 = time.time()
    log = []
    for ep in range(args.episodes):
        sc, wk = TRAIN[ep % len(TRAIN)]
        xs, acts, rews, m = episode(env, W, sc, wk, rng, False, args)
        G = np.cumsum(rews[::-1])[::-1]
        base = G.copy() if base is None or len(base) != len(G) else 0.9 * base + 0.1 * G
        adv = G - base
        adv = adv / (adv.std() + 1e-6)
        grad = np.zeros_like(W)
        for x, a, A in zip(xs, acts, adv):
            pi = softmax(W @ x)
            g = -pi[:, None] * x[None, :]
            g[a] += x
            grad += A * g
        W += opt.step(grad / len(xs))
        log.append({"episode": ep, "scenario": sc, "week": wk, "return": float(np.sum(rews)), "cost": m["cost"], "stall_ratio": m["stall_ratio"]})
        print(f"[seed {args.seed}] ep {ep:3d} {sc:8s} w{wk} return {np.sum(rews):9.1f} cost {m['cost']:8.0f} stall {100*m['stall_ratio']:.3f}%", flush=True)
    train_sec = time.time() - t0
    test = []
    for sc, wk in TEST:
        _, acts, _, m = episode(env, W, sc, wk, rng, True, args)
        hist = np.bincount(acts, minlength=len(ACTIONS)).tolist()
        row = {"scenario": sc, "week": wk, "cost": m["cost"], "stall_ratio": m["stall_ratio"], "startup_p95_ms": m["startup_p95_ms"],
               "auction_g2g_p95_ms": m.get("auction_cohort_g2g_p95_ms", 0), "action_hist": hist}
        if args.baselines:
            _, _, _, mb = episode(env, W, sc, wk, rng, True, args, fixed=ACTIONS.index((0.7, 0.9)))
            row["hcdn_cost"], row["hcdn_stall_ratio"] = mb["cost"], mb["stall_ratio"]
        test.append(row)
        print(f"[seed {args.seed}] test {sc:8s} w{wk} cost {m['cost']:8.0f} stall {100*m['stall_ratio']:.3f}% actions {hist}", flush=True)
    out = {"seed": args.seed, "episodes": args.episodes, "train_sec": train_sec, "actions": ACTIONS, "train_log": log, "test": test,
           "policy": W.tolist(), "stall_penalty": args.stall_penalty}
    if args.out:
        os.makedirs(os.path.dirname(args.out), exist_ok=True)
        json.dump(out, open(args.out, "w"), indent=1)


if __name__ == "__main__":
    main()
