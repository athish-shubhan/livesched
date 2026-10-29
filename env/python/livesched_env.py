"""Gym-style Python client for the LiveSched gRPC environment.

    env = LiveSchedEnv("127.0.0.1:50071")
    obs = env.reset(scenario="auctions", seed=11)
    while True:
        obs, reward, done, info = env.step({"offload_util": 0.5, "be_max_util": 0.6})
        if done:
            break
    metrics = env.close()          # true 95th-percentile-billing cost, stalls, auction latency

Start the server with `livesched serve -addr 127.0.0.1:50071`. Stubs are
generated from env/proto/livesched_env.proto by env/python/gen.sh.
"""
import json
import os
import sys

import grpc

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import livesched_env_pb2 as pb  # noqa: E402
import livesched_env_pb2_grpc as pbg  # noqa: E402

DEFAULT_PARAMS = {"frozen": True, "cold": True, "hot": True, "offload": True, "protect_lowlat": True,
                  "frozen_max": 3, "cold_max": 40, "hot_util": 0.85, "offload_util": 0.7, "be_max_util": 0.9}


class LiveSchedEnv:
    def __init__(self, addr="127.0.0.1:50071"):
        self.channel = grpc.insecure_channel(addr)
        self.stub = pbg.EnvStub(self.channel)
        self.episode = None
        self.params = dict(DEFAULT_PARAMS)

    def reset(self, scenario="shows", seed=1, hours=12, peak=30000, step_seconds=300, substreams=1,
              substream_aware=False, whole_auctions=False, base_params=None, stall_penalty=0.0, cost_scale=1000.0):
        self.params = dict(DEFAULT_PARAMS, **(base_params or {}))
        o = self.stub.Reset(pb.ResetRequest(scenario=scenario, seed=seed, hours=hours, peak=peak, step_seconds=step_seconds,
                                            substreams=substreams, substream_aware=substream_aware, whole_auctions=whole_auctions,
                                            base_params_json=json.dumps(self.params), stall_penalty=stall_penalty, cost_scale=cost_scale))
        self.episode = o.episode
        return o

    def step(self, action):
        """action: dict of HCDN knobs to change; unspecified knobs keep their values."""
        self.params.update(action)
        p = self.params
        a = pb.Action(offload_util=p["offload_util"], be_max_util=p["be_max_util"], hot_util=p["hot_util"],
                      cold_max=p["cold_max"], frozen_max=p["frozen_max"], frozen=p["frozen"], cold=p["cold"], hot=p["hot"],
                      offload=p["offload"], protect_lowlat=p["protect_lowlat"])
        r = self.stub.Step(pb.StepRequest(episode=self.episode, action=a))
        info = {"cost_rate": r.observation.interval_cost_rate, "stall_share": r.observation.interval_stall_share}
        return r.observation, r.reward, r.done, info

    def observe(self):
        return self.stub.Observe(pb.EpisodeRef(episode=self.episode))

    def close(self):
        m = self.stub.Close(pb.EpisodeRef(episode=self.episode))
        self.episode = None
        return json.loads(m.metrics_json)
