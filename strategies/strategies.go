// Package strategies holds the scheduling baselines: DNS-style static
// mapping, global-hotness aggregation (the approach HCDN improves on), and a
// re-implementation of HCDN's four redirection strategies from the NSDI '26
// paper, each switchable for ablation. Medley-style substreams are a
// transport mode (sim.Transport) and combine with any of these.
package strategies

import (
	"fmt"

	"livesched/sim"
)

// Static sends every viewer to its DNS default edge: no redirection, no
// awareness of streams or load.
type Static struct{}

func (Static) Name() string                             { return "static-nearest" }
func (Static) Assign(e *sim.Engine, s *sim.Session) int { return e.DefaultNode(s) }
func (Static) OnTick(*sim.Engine)                       {}
func (Static) Proactive() bool                          { return false }

// GlobalHot aggregates streams by global popularity: globally cold streams
// are pinned to one regular edge per region (consistent hashing), globally
// hot ones are spread to the least-loaded edge. Its blind spot, which HCDN
// targets, is a stream that is hot globally but cold in this region (or the
// reverse).
type GlobalHot struct{ HotViewers int }

func (g GlobalHot) Name() string     { return fmt.Sprintf("global-hotness(h=%d)", g.HotViewers) }
func (GlobalHot) OnTick(*sim.Engine) {}
func (GlobalHot) Proactive() bool    { return false }

func (g GlobalHot) Assign(e *sim.Engine, s *sim.Session) int {
	regs := e.RegionEdges(s.Region, sim.Regular)
	if e.GlobalViewers(s.Stream) >= g.HotViewers {
		return e.LeastLoaded(regs, nil)
	}
	id := sim.RendezvousPick(int64(s.Stream), regs, func(id int) bool { return e.Nodes[id].Utilization() < 0.95 })
	if id < 0 {
		return e.LeastLoaded(regs, nil)
	}
	return id
}

// HCDNParams parameterizes the four strategies. Defaults are our reading of
// the paper's description; the paper does not publish thresholds.
type HCDNParams struct {
	Frozen        bool    `json:"frozen"`         // S1: offload frozen streams to multihomed edges
	Cold          bool    `json:"cold"`           // S2: aggregate cold streams onto few edges
	Hot           bool    `json:"hot"`            // S3: move hot streams onto the edges where they are already hot
	Offload       bool    `json:"offload"`        // S4: offload hot streams from hot nodes to best-effort edges
	Proactive     bool    `json:"proactive"`      // pre-connect feed joins
	FrozenMax     int     `json:"frozen_max"`     // regional viewers below which a stream is frozen
	ColdMax       int     `json:"cold_max"`       // regional viewers below which a stream is cold
	HotUtil       float64 `json:"hot_util"`       // an edge already carrying a hot stream takes more viewers up to this utilization
	OffloadUtil   float64 `json:"offload_util"`   // regional regular-edge utilization above which hot streams go to best-effort edges
	MaxUtil       float64 `json:"max_util"`       // never place on an edge above this utilization if any alternative exists
	ProtectLowLat bool    `json:"protect_lowlat"` // keep the auction cohort off best-effort and remote edges
	BEMaxUtil     float64 `json:"be_max_util"`    // cap on best-effort edge utilization (nominal capacity)
}

func DefaultHCDN() HCDNParams {
	return HCDNParams{Frozen: true, Cold: true, Hot: true, Offload: true, Proactive: true,
		FrozenMax: 3, ColdMax: 40, HotUtil: 0.85, OffloadUtil: 0.7, MaxUtil: 0.95, ProtectLowLat: true, BEMaxUtil: 0.9}
}

type HCDN struct {
	P   HCDNParams
	Tag string
}

func (h HCDN) Name() string {
	if h.Tag != "" {
		return h.Tag
	}
	return "hcdn"
}
func (h HCDN) OnTick(*sim.Engine) {}
func (h HCDN) Proactive() bool    { return h.P.Proactive }

func (h HCDN) Assign(e *sim.Engine, s *sim.Session) int {
	p := h.P
	c := e.RegionViewers(s.Region, s.Stream)
	under := func(max float64) func(int) bool {
		return func(id int) bool { n := e.Nodes[id]; return !n.Down && n.Utilization() < max }
	}
	regs := e.RegionEdges(s.Region, sim.Regular)

	// S1: frozen streams (a handful of viewers in this region) go to the
	// multihomed edges, consolidated across regions by consistent hashing, so
	// a stream watched by two people in each of four regions costs one
	// midgress copy instead of four.
	if p.Frozen && c < p.FrozenMax && !(p.ProtectLowLat && s.LowLat) {
		if id := sim.RendezvousPick(int64(s.Stream), e.Multihomed(), under(p.MaxUtil)); id >= 0 {
			return id
		}
	}
	// S2: cold streams are aggregated onto one regular edge of the region.
	if p.Cold && c < p.ColdMax {
		if id := sim.RendezvousPick(int64(s.Stream), regs, under(p.MaxUtil)); id >= 0 {
			return id
		}
	}
	// S4: when the region's regular edges are running hot (the expensive
	// 95th-percentile peak), offload hot streams to cheap best-effort edges,
	// preferring one that already carries the stream.
	if p.Offload && c >= p.ColdMax && e.RegionUtil(s.Region, sim.Regular) > p.OffloadUtil && !(p.ProtectLowLat && s.LowLat) {
		be := e.RegionEdges(s.Region, sim.BestEffort)
		if id := mostViewers(e, be, s.Stream, under(p.BEMaxUtil)); id >= 0 {
			return id
		}
		if id := e.LeastLoaded(be, under(p.BEMaxUtil)); id >= 0 {
			return id
		}
	}
	// S3: hot streams go to the regular edge where they are already hottest,
	// so popular streams concentrate instead of being copied to every edge.
	if p.Hot {
		if id := mostViewers(e, regs, s.Stream, under(p.HotUtil)); id >= 0 {
			return id
		}
	}
	if id := e.LeastLoaded(regs, under(p.MaxUtil)); id >= 0 {
		return id
	}
	return e.LeastLoaded(regs, nil)
}

func mostViewers(e *sim.Engine, ids []int, stream int, ok func(int) bool) int {
	best, bv := -1, 0
	for _, id := range ids {
		if !ok(id) {
			continue
		}
		if v := e.Nodes[id].StreamViewers(stream); v > bv {
			best, bv = id, v
		}
	}
	return best
}

// ByName builds a baseline strategy.
func ByName(name string) (sim.Strategy, error) {
	switch name {
	case "static":
		return Static{}, nil
	case "globalhot":
		return GlobalHot{HotViewers: 200}, nil
	case "hcdn":
		return HCDN{P: DefaultHCDN()}, nil
	}
	if len(name) > 5 && name[:5] == "hcdn-" {
		// ablation: hcdn-noS1, hcdn-noS2, hcdn-noS3, hcdn-noS4, hcdn-noproactive, hcdn-blind
		p := DefaultHCDN()
		switch name[5:] {
		case "noS1":
			p.Frozen = false
		case "noS2":
			p.Cold = false
		case "noS3":
			p.Hot = false
		case "noS4":
			p.Offload = false
		case "noproactive":
			p.Proactive = false
		case "only-proactive":
			p = HCDNParams{Proactive: true, MaxUtil: 0.95}
		case "blind":
			// scenario-blind: the auction cohort is offloaded and consolidated like everyone else
			p.ProtectLowLat = false
		default:
			return nil, fmt.Errorf("unknown ablation %q", name)
		}
		return HCDN{P: p, Tag: name}, nil
	}
	return nil, fmt.Errorf("unknown strategy %q", name)
}

// Tunable is HCDN whose parameters can be changed while a simulation runs:
// the gRPC environment's actions set them between steps.
type Tunable struct {
	P   HCDNParams
	Tag string
}

func (t *Tunable) Name() string {
	if t.Tag != "" {
		return t.Tag
	}
	return "hcdn(tunable)"
}
func (t *Tunable) OnTick(*sim.Engine) {}
func (t *Tunable) Proactive() bool    { return t.P.Proactive }
func (t *Tunable) Assign(e *sim.Engine, s *sim.Session) int {
	return HCDN{P: t.P}.Assign(e, s)
}
