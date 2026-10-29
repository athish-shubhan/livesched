"""Example policy-as-code candidate for `livesched run -external`.

Reads one JSON observation per line on stdin and answers {"node": id}.
This one re-implements cold-stream aggregation plus hot-stream concentration
in ~30 lines, to show the protocol; model-generated candidates use the same
interface and run in the same sandbox (no network, CPU/memory limits).
"""
import hashlib
import json
import sys

COLD = 40
HOT_UTIL = 0.85


def rendezvous(stream, ids):
    return max(ids, key=lambda i: hashlib.blake2b(f"{stream}:{i}".encode(), digest_size=8).digest())


for line in sys.stdin:
    obs = json.loads(line)
    s = obs["session"]
    regular = [c for c in obs["candidates"] if c["kind"] == "regular" and c["region"] == s["region"] and c["util"] < 0.95]
    if not regular:
        node = obs["default"]
    elif obs["region_viewers"] < COLD:
        node = rendezvous(s["stream"], [c["id"] for c in regular])
    else:
        hot = [c for c in regular if c["has_stream"] and c["util"] < HOT_UTIL]
        node = max(hot, key=lambda c: c["stream_viewers"])["id"] if hot else min(regular, key=lambda c: c["util"])["id"]
    sys.stdout.write(json.dumps({"node": node}) + "\n")
    sys.stdout.flush()
