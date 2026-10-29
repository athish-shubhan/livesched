#!/usr/bin/env python3
"""LP lower bound on CDN bandwidth cost for a small LiveSched instance.

Input: `livesched demand` JSON (topology, bitrates, per-5-minute demand of
each region for each stream). For each window, solve the LP relaxation of:

  minimise   sum_n price_n * egress_n                      (edge -> viewers)
           + mid_price    * sum_{s,n} bitrate_s * y[s,n]   (L2 -> edge copies)
           + origin_price * sum_{s,l} bitrate_s * z[s,l]   (origin -> L2 copies)
  s.t.  sum_n x[r,s,n] = D[r,s]          every region's demand for s is served
        x[r,s,n] <= D[r,s] * y[s,n]      serving s at edge n needs a copy there
        sum_{r,s} x[r,s,n] <= cap_n      nominal egress capacity
        y[s,n] <= z[s,parent(n)]         a copy at an edge needs one at its L2
        x >= 0, 0 <= y, z <= 1           (y, z binary in the real problem)

Edges serve their own region; multihomed edges serve any region. Billing is
on averages (the 95th-percentile objective is not separable across windows),
so compare the bound with the simulator's `avg_billing_cost`. It ignores QoE
entirely (startup, stalls, latency, best-effort reliability), so a strategy
can only approach it by giving up QoE; that gap is the point of reporting it.
"""
import json
import sys
import time

import numpy as np
from scipy.optimize import linprog
from scipy.sparse import coo_matrix


def solve_window(nodes, bitrate, cells, mid_price, origin_price, n_l2):
    serve = {}  # region -> node ids that may serve it
    for n in nodes:
        for r in range(max(x["region"] for x in nodes) + 1):
            if n["region"] == r or n["kind"] == "multihomed":
                serve.setdefault(r, []).append(n["id"])
    streams = sorted({c["s"] for c in cells})
    if not streams:
        return 0.0
    var = {}
    cost = []

    def v(key, c):
        var[key] = len(cost)
        cost.append(c)
        return var[key]

    price = {n["id"]: n["price"] for n in nodes}
    for c in cells:
        for n in serve[c["r"]]:
            v(("x", c["r"], c["s"], n), price[n])
    for s in streams:
        for n in nodes:
            v(("y", s, n["id"]), mid_price * bitrate[s])
        for l in range(n_l2):
            v(("z", s, l), origin_price * bitrate[s])
    rows, cols, vals = [], [], []
    b_ub, b_eq = [], []
    eq_rows, eq_cols, eq_vals = [], [], []
    # demand
    for c in cells:
        for n in serve[c["r"]]:
            eq_rows.append(len(b_eq)); eq_cols.append(var[("x", c["r"], c["s"], n)]); eq_vals.append(1.0)
        b_eq.append(c["mbps"])
    # presence
    for c in cells:
        for n in serve[c["r"]]:
            rows += [len(b_ub)] * 2
            cols += [var[("x", c["r"], c["s"], n)], var[("y", c["s"], n)]]
            vals += [1.0, -c["mbps"]]
            b_ub.append(0.0)
    # capacity
    for n in nodes:
        row = len(b_ub)
        any_ = False
        for c in cells:
            k = ("x", c["r"], c["s"], n["id"])
            if k in var:
                rows.append(row); cols.append(var[k]); vals.append(1.0); any_ = True
        if any_:
            b_ub.append(n["cap"])
    # edge copy needs L2 copy
    for s in streams:
        for n in nodes:
            rows += [len(b_ub)] * 2
            cols += [var[("y", s, n["id"])], var[("z", s, n["parent"])]]
            vals += [1.0, -1.0]
            b_ub.append(0.0)
    nv = len(cost)
    A_ub = coo_matrix((vals, (rows, cols)), shape=(len(b_ub), nv)).tocsr()
    A_eq = coo_matrix((eq_vals, (eq_rows, eq_cols)), shape=(len(b_eq), nv)).tocsr()
    bounds = [(0, None)] * nv
    for k, i in var.items():
        if k[0] in ("y", "z"):
            bounds[i] = (0, 1)
    res = linprog(np.array(cost), A_ub=A_ub, b_ub=np.array(b_ub), A_eq=A_eq, b_eq=np.array(b_eq), bounds=bounds, method="highs")
    if res.status != 0:
        raise RuntimeError(f"LP failed: {res.message}")
    return res.fun


def main():
    d = json.load(open(sys.argv[1]))
    # optional: derate best-effort capacity to its guaranteed floor, a bound
    # that does not count on capacity that may vanish in any given minute
    derate = float(sys.argv[2]) if len(sys.argv) > 2 else 1.0
    for n in d["nodes"]:
        if n["kind"] == "best-effort":
            n["cap"] *= derate
    n_l2 = d["regions"] * d["l2_per_region"]
    t0 = time.time()
    per = [solve_window(d["nodes"], d["bitrate"], w, d["mid_price"], d["origin_price"], n_l2) for w in d["windows"]]
    bound = float(np.mean(per))
    out = {"lp_bound_avg_billing_cost": bound, "best_effort_derate": derate, "windows": len(per), "solve_sec": round(time.time() - t0, 1)}
    print(json.dumps(out))


if __name__ == "__main__":
    main()
