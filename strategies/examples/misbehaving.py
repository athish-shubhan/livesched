"""A candidate that tries to reach the network and then hangs. Used to show
the sandbox: the connect fails (no network namespace access) and the hang
hits the per-call timeout, so the run degrades to the default edge."""
import json
import socket
import sys
import time

try:
    socket.create_connection(("1.1.1.1", 443), timeout=1)
    sys.stderr.write("NETWORK REACHABLE\n")
except OSError as e:
    sys.stderr.write(f"network blocked as expected: {e}\n")
for i, line in enumerate(sys.stdin):
    if i == 1000:
        time.sleep(3600)
    sys.stdout.write(json.dumps({"node": json.loads(line)["default"]}) + "\n")
    sys.stdout.flush()
