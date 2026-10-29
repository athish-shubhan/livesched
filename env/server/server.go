// Package server implements the gRPC environment (env/proto) over the
// simulator: Reset builds an engine and a tunable HCDN strategy, Step sets
// the strategy's knobs and advances the engine by one control interval.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"livesched/env/envpb"
	"livesched/sim"
	"livesched/strategies"
	"livesched/workload"
)

type episode struct {
	mu       sync.Mutex
	e        *sim.Engine
	st       *strategies.Tunable
	step     int
	last     sim.StepStats
	lastT    int
	penalty  float64
	scale    float64
	finished *sim.Metrics
}

type Server struct {
	envpb.UnimplementedEnvServer
	mu  sync.Mutex
	eps map[string]*episode
	n   atomic.Int64
}

func New() *Server { return &Server{eps: map[string]*episode{}} }

func (s *Server) get(id string) (*episode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep, ok := s.eps[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no episode %q", id)
	}
	return ep, nil
}

func (s *Server) Reset(_ context.Context, r *envpb.ResetRequest) (*envpb.Observation, error) {
	cfg := workload.DefaultSynth()
	cfg.Seed = max(r.Seed, 1)
	if r.Hours > 0 {
		cfg.Hours = r.Hours
	}
	if r.Peak > 0 {
		cfg.PeakConcurrent = int(r.Peak)
	}
	if r.Scenario != "" {
		cfg.Scenario = workload.Scenario(r.Scenario)
	}
	w := workload.NewSynth(cfg)
	p := strategies.DefaultHCDN()
	if r.BaseParamsJson != "" {
		if err := json.Unmarshal([]byte(r.BaseParamsJson), &p); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "base params: %v", err)
		}
	}
	tr := sim.Transport{Substreams: int(max(r.Substreams, 1)), SubstreamAware: r.SubstreamAware, WholeAuctions: r.WholeAuctions}
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), tr, w, cfg.Seed*1000+7)
	st := &strategies.Tunable{P: p, Tag: "hcdn(env)"}
	e.Begin(st, w)
	ep := &episode{e: e, st: st, step: int(r.StepSeconds), penalty: r.StallPenalty, scale: r.CostScale}
	if ep.step <= 0 {
		ep.step = 300
	}
	if ep.scale <= 0 {
		ep.scale = 1000
	}
	id := fmt.Sprintf("ep-%d", s.n.Add(1))
	s.mu.Lock()
	s.eps[id] = ep
	s.mu.Unlock()
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.observe(id, 0, 0), nil
}

func (ep *episode) observe(id string, costRate, stallShare float64) *envpb.Observation {
	e := ep.e
	o := &envpb.Observation{Episode: id, T: int32(e.Now()), Horizon: int32(e.Horizon()),
		HourOfDay: math.Mod(8+float64(e.Now())/3600, 24), IntervalCostRate: costRate, IntervalStallShare: stallShare,
		AuctionG2GP95Ms: e.LLG2GNow() * 1000}
	var viewers int64
	for _, n := range e.Nodes {
		viewers += int64(n.Viewers)
	}
	o.Viewers = viewers
	for r := 0; r < e.Topo.Regions; r++ {
		o.RegularUtil = append(o.RegularUtil, e.RegionUtil(r, sim.Regular))
		o.BestEffortUtil = append(o.BestEffortUtil, e.RegionUtil(r, sim.BestEffort))
		o.MultihomedUtil = append(o.MultihomedUtil, e.RegionUtil(r, sim.Multihomed))
	}
	return o
}

func apply(p *strategies.HCDNParams, a *envpb.Action) {
	if a == nil {
		return
	}
	setF := func(dst *float64, v float64) {
		if v != 0 || !a.KeepUnset {
			*dst = v
		}
	}
	setI := func(dst *int, v int32) {
		if v != 0 || !a.KeepUnset {
			*dst = int(v)
		}
	}
	setF(&p.OffloadUtil, a.OffloadUtil)
	setF(&p.BEMaxUtil, a.BeMaxUtil)
	setF(&p.HotUtil, a.HotUtil)
	setI(&p.ColdMax, a.ColdMax)
	setI(&p.FrozenMax, a.FrozenMax)
	if !a.KeepUnset {
		p.Frozen, p.Cold, p.Hot, p.Offload, p.ProtectLowLat = a.Frozen, a.Cold, a.Hot, a.Offload, a.ProtectLowlat
	}
}

func (s *Server) Step(_ context.Context, r *envpb.StepRequest) (*envpb.StepResult, error) {
	ep, err := s.get(r.Episode)
	if err != nil {
		return nil, err
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.e.Done() {
		return nil, status.Error(codes.FailedPrecondition, "episode is over")
	}
	apply(&ep.st.P, r.Action)
	done := ep.e.Advance(ep.step)
	now := ep.e.Stats()
	dt := float64(ep.e.Now() - ep.lastT)
	costRate := (now.Cost - ep.last.Cost) / math.Max(dt, 1)
	stall := 0.0
	if dw := now.WatchSec - ep.last.WatchSec; dw > 0 {
		stall = (now.StallSec - ep.last.StallSec) / dw
	}
	ep.last, ep.lastT = now, ep.e.Now()
	reward := -costRate/ep.scale - ep.penalty*stall
	return &envpb.StepResult{Observation: ep.observe(r.Episode, costRate, stall), Reward: reward, Done: done}, nil
}

func (s *Server) Observe(_ context.Context, r *envpb.EpisodeRef) (*envpb.Observation, error) {
	ep, err := s.get(r.Episode)
	if err != nil {
		return nil, err
	}
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.observe(r.Episode, 0, 0), nil
}

// Close runs the episode to the end if needed (with the current knobs) and
// returns its metrics; the episode is then forgotten.
func (s *Server) Close(_ context.Context, r *envpb.EpisodeRef) (*envpb.EpisodeMetrics, error) {
	ep, err := s.get(r.Episode)
	if err != nil {
		return nil, err
	}
	ep.mu.Lock()
	for !ep.e.Done() {
		ep.e.Advance(ep.step)
	}
	m := ep.e.Finish()
	ep.mu.Unlock()
	s.mu.Lock()
	delete(s.eps, r.Episode)
	s.mu.Unlock()
	b, _ := json.Marshal(m)
	return &envpb.EpisodeMetrics{MetricsJson: string(b)}, nil
}
