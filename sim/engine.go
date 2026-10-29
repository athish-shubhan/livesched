package sim

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
)

// Strategy decides where viewers go. Assign is called for every join and for
// every re-join after a best-effort outage; it must return the id of an edge
// that may serve the session (an edge in the viewer's region, or any
// multihomed edge). Strategies get read access to the engine.
type Strategy interface {
	Name() string
	Assign(e *Engine, s *Session) int
	OnTick(e *Engine)
	Proactive() bool // pre-connect feed joins (HCDN proactive QoE assurance)
}

// Transport describes how a stream travels inside the CDN.
type Transport struct {
	Substreams     int  // K: 1 = whole stream; K > 1 = Medley-style substreams on K L2 nodes
	SubstreamAware bool // false: an L2 miss on a substream pulls the full stream (HCDN's finding)
	WholeAuctions  bool // scenario-aware: auction streams travel whole even when K > 1
}

// Workload produces sessions.
type Workload interface {
	Streams() []Stream
	Ticks() int
	Arrivals(t int) []*Session
	Name() string
}

type Engine struct {
	Topo    TopoConfig
	QoE     QoEConfig
	Trans   Transport
	Nodes   []*Node
	streams []Stream
	t       int
	rng     *rand.Rand

	regionEdges [][3][]int    // region -> kind -> node ids
	mhAll       []int         // every multihomed edge
	regionCount []map[int]int // region -> stream -> viewers in that region
	globalCount map[int]int
	l2Edges     []map[int]int // global L2 id -> stream -> number of child edges carrying it
	regionL2    []map[int]int // region -> stream -> number of its L2s carrying it
	midR        []float64     // region -> current L2->edge Mbps
	originR     []float64     // region -> current origin->L2 Mbps
	onNode      [][]*Session  // node -> sessions on it
	ending      map[int][]*Session

	m          Metrics
	win        int
	winEgress  []float64
	winOrigin  []float64
	winMid     []float64
	egressSer  [][]float64
	originSer  [][]float64
	midSer     [][]float64
	startups   []float64
	llStartups []float64

	ll      []*Session // auction-cohort sessions currently attached
	g2gHist []int64    // 10 ms bins of sampled auction-cohort glass-to-glass latency
	cumCost float64    // price-weighted Mbps-seconds so far (average-billing cost)

	st   Strategy
	w    Workload
	T    int
	done bool
}

// Metrics of one run.
type Metrics struct {
	Strategy       string  `json:"strategy"`
	Workload       string  `json:"workload"`
	Cost           float64 `json:"cost"` // p95 billing, egress + midgress
	EgressCost     float64 `json:"egress_cost"`
	AvgBillCost    float64 `json:"avg_billing_cost"` // same traffic billed on averages (comparable with the LP bound)
	MidgressCost   float64 `json:"midgress_cost"`    // L2->edge + origin->L2
	AvgEgressMbps  float64 `json:"avg_egress_mbps"`
	AvgMidMbps     float64 `json:"avg_midgress_mbps"` // L2->edge + origin->L2
	MidEgressRatio float64 `json:"midgress_egress_ratio"`
	Sessions       int64   `json:"sessions"`
	WatchSec       float64 `json:"watch_sec"`
	StallSec       float64 `json:"stall_sec"`
	StallRatio     float64 `json:"stall_ratio"`
	StartupP50     float64 `json:"startup_p50_ms"`
	StartupP95     float64 `json:"startup_p95_ms"`
	Redirects      int64   `json:"redirects"`
	RedirectShare  float64 `json:"redirect_share"`
	MissShare      float64 `json:"cold_join_share"` // joins that found the stream absent at the edge
	LLSessions     int64   `json:"auction_cohort_sessions"`
	LLInBound      float64 `json:"auction_cohort_g2g_within_bound"`
	LLStallRatio   float64 `json:"auction_cohort_stall_ratio"`
	LLStartupP95   float64 `json:"auction_cohort_startup_p95_ms"`
	LLG2GP50       float64 `json:"auction_cohort_g2g_p50_ms"`
	LLG2GP95       float64 `json:"auction_cohort_g2g_p95_ms"`
	LLG2GP99       float64 `json:"auction_cohort_g2g_p99_ms"`
	Outages        int64   `json:"best_effort_outages"`
	Rejoins        int64   `json:"outage_rejoins"`
	PeakUtil       float64 `json:"peak_region_util"`
	SimSec         int     `json:"sim_sec"`

	CPUSec             float64 `json:"cpu_sec,omitempty"`
	ViewerMinPerCPUSec float64 `json:"viewer_minutes_per_cpu_sec,omitempty"`
	SessionsPerCPUSec  float64 `json:"sessions_per_cpu_sec,omitempty"`

	llWatch, llStall float64
	misses           int64
}

func hash64(a ...int64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for _, x := range a {
		for i := 0; i < 8; i++ {
			b[i] = byte(x >> (8 * i))
		}
		h.Write(b[:])
	}
	return h.Sum64()
}

func New(topo TopoConfig, qoe QoEConfig, tr Transport, w Workload, seed uint64) *Engine {
	if tr.Substreams < 1 {
		tr.Substreams = 1
	}
	e := &Engine{Topo: topo, QoE: qoe, Trans: tr, streams: w.Streams(), rng: rand.New(rand.NewPCG(seed, seed+7)),
		globalCount: map[int]int{}, ending: map[int][]*Session{}}
	id := 0
	e.regionEdges = make([][3][]int, topo.Regions)
	add := func(kind NodeKind, r, n int, cap, price float64) {
		for i := 0; i < n; i++ {
			nd := &Node{ID: id, Kind: kind, Region: r, CapMbps: cap, effCap: cap, Price: price, streams: map[int]int{},
				Parent: r*topo.L2PerRegion + i%topo.L2PerRegion, bufN: qoe.BufferNormal, bufLL: qoe.BufferLowLat}
			e.Nodes = append(e.Nodes, nd)
			e.regionEdges[r][kind] = append(e.regionEdges[r][kind], id)
			if kind == Multihomed {
				e.mhAll = append(e.mhAll, id)
			}
			id++
		}
	}
	for r := 0; r < topo.Regions; r++ {
		add(Regular, r, topo.RegularPerReg, topo.RegularCap, topo.RegularPrice)
		add(Multihomed, r, topo.MultihomedPer, topo.MultihomedCap, topo.MultihomedPrice)
		add(BestEffort, r, topo.BestEffortPer, topo.BestEffortCap, topo.BestEffortPrice)
	}
	e.regionCount = make([]map[int]int, topo.Regions)
	for r := range e.regionCount {
		e.regionCount[r] = map[int]int{}
	}
	e.regionL2 = make([]map[int]int, topo.Regions)
	for r := range e.regionL2 {
		e.regionL2[r] = map[int]int{}
	}
	e.midR = make([]float64, topo.Regions)
	e.originR = make([]float64, topo.Regions)
	e.l2Edges = make([]map[int]int, topo.Regions*topo.L2PerRegion)
	for i := range e.l2Edges {
		e.l2Edges[i] = map[int]int{}
	}
	e.onNode = make([][]*Session, len(e.Nodes))
	e.winEgress = make([]float64, len(e.Nodes))
	e.egressSer = make([][]float64, len(e.Nodes))
	e.winOrigin = make([]float64, topo.Regions)
	e.winMid = make([]float64, topo.Regions)
	e.originSer = make([][]float64, topo.Regions)
	e.midSer = make([][]float64, topo.Regions)
	return e
}

// ---- read API for strategies

func (e *Engine) Now() int                            { return e.t }
func (e *Engine) Stream(s int) Stream                 { return e.streams[s] }
func (e *Engine) RegionEdges(r int, k NodeKind) []int { return e.regionEdges[r][k] }
func (e *Engine) Multihomed() []int                   { return e.mhAll }
func (e *Engine) RegionViewers(r, s int) int          { return e.regionCount[r][s] }
func (e *Engine) GlobalViewers(s int) int             { return e.globalCount[s] }

// RegionUtil is the egress utilization of one kind of edge in a region.
func (e *Engine) RegionUtil(r int, k NodeKind) float64 {
	var load, cap float64
	for _, id := range e.regionEdges[r][k] {
		n := e.Nodes[id]
		if n.Down {
			continue
		}
		load += n.LoadMbps
		cap += n.CapMbps
	}
	if cap == 0 {
		return 1
	}
	return load / cap
}

// DefaultNode is where DNS-style static mapping would send the viewer: a
// regular edge of its region chosen by hashing the session. Any other choice
// costs a redirection.
func (e *Engine) DefaultNode(s *Session) int {
	ids := e.regionEdges[s.Region][Regular]
	return ids[hash64(s.ID)%uint64(len(ids))]
}

// CanServe reports whether node id may serve the session.
func (e *Engine) CanServe(id int, s *Session) bool {
	n := e.Nodes[id]
	if n.Down {
		return false
	}
	return n.Region == s.Region || n.Kind == Multihomed
}

// RendezvousPick returns the node among ids with the highest hash weight for
// key (stable assignment that moves little when the set changes).
func RendezvousPick(key int64, ids []int, ok func(int) bool) int {
	best, bh := -1, uint64(0)
	for _, id := range ids {
		if ok != nil && !ok(id) {
			continue
		}
		if h := hash64(key, int64(id)); best < 0 || h > bh {
			best, bh = id, h
		}
	}
	return best
}

// LeastLoaded returns the node with the lowest utilization, or -1.
func (e *Engine) LeastLoaded(ids []int, ok func(int) bool) int {
	best, bu := -1, math.Inf(1)
	for _, id := range ids {
		if ok != nil && !ok(id) {
			continue
		}
		if u := e.Nodes[id].Utilization(); u < bu {
			best, bu = id, u
		}
	}
	return best
}

// ---- engine

func (e *Engine) l2Of(n *Node) int { return n.Parent }

// k is the number of substreams stream s travels in.
func (e *Engine) k(s int) int {
	if e.Trans.WholeAuctions && e.streams[s].Auction {
		return 1
	}
	return e.Trans.Substreams
}

// originCopy is the origin->L2 traffic of one stream in a region as a
// function of how many of the region's L2 nodes have children carrying it.
func (e *Engine) originCopy(stream int, b float64, nl2 int) float64 {
	if nl2 == 0 {
		return 0
	}
	K := e.k(stream)
	switch {
	case K == 1:
		return b * float64(nl2) // every L2 with a child carrying s pulls a full copy
	case e.Trans.SubstreamAware:
		return b // K substreams, each pulled once by its own L2
	default:
		return b * math.Min(float64(K), float64(e.Topo.L2PerRegion)) // a substream miss pulls the full stream
	}
}

func (e *Engine) l2Change(n *Node, stream int, delta int) {
	l2 := e.l2Edges[e.l2Of(n)]
	before := l2[stream]
	l2[stream] += delta
	if l2[stream] == 0 {
		delete(l2, stream)
	}
	if (before == 0) == (l2[stream] == 0) {
		return // L2 presence unchanged
	}
	r := n.Parent / e.Topo.L2PerRegion
	b := e.streams[stream].Bitrate
	old := e.regionL2[r][stream]
	nw := old + 1
	if delta < 0 {
		nw = old - 1
	}
	if nw == 0 {
		delete(e.regionL2[r], stream)
	} else {
		e.regionL2[r][stream] = nw
	}
	e.originR[r] += e.originCopy(stream, b, nw) - e.originCopy(stream, b, old)
}

func (e *Engine) attach(s *Session, id int) {
	n := e.Nodes[id]
	b := e.streams[s.Stream].Bitrate
	if n.streams[s.Stream] == 0 {
		e.l2Change(n, s.Stream, +1)
		e.midR[n.Region] += b
	}
	n.streams[s.Stream]++
	n.LoadMbps += b
	n.Viewers++
	if s.LowLat {
		n.llView++
		s.llIdx = len(e.ll)
		e.ll = append(e.ll, s)
	}
	s.node = id
	e.onNode[id] = append(e.onNode[id], s)
	e.regionCount[s.Region][s.Stream]++
	e.globalCount[s.Stream]++
}

func (e *Engine) detach(s *Session) {
	n := e.Nodes[s.node]
	b := e.streams[s.Stream].Bitrate
	n.streams[s.Stream]--
	if n.streams[s.Stream] == 0 {
		delete(n.streams, s.Stream)
		e.l2Change(n, s.Stream, -1)
		e.midR[n.Region] -= b
	}
	n.LoadMbps -= b
	if n.LoadMbps < 1e-9 {
		n.LoadMbps = 0
	}
	n.Viewers--
	if s.LowLat {
		n.llView--
		last := e.ll[len(e.ll)-1]
		e.ll[s.llIdx] = last
		last.llIdx = s.llIdx
		e.ll = e.ll[:len(e.ll)-1]
	}
	lst := e.onNode[s.node]
	for i, x := range lst {
		if x == s {
			lst[i] = lst[len(lst)-1]
			e.onNode[s.node] = lst[:len(lst)-1]
			break
		}
	}
	e.regionCount[s.Region][s.Stream]--
	if e.regionCount[s.Region][s.Stream] == 0 {
		delete(e.regionCount[s.Region], s.Stream)
	}
	e.globalCount[s.Stream]--
	if e.globalCount[s.Stream] == 0 {
		delete(e.globalCount, s.Stream)
	}
	s.node = -1
}

// join assigns and accounts startup delay. rejoin = reconnect after outage.
func (e *Engine) join(st Strategy, s *Session, rejoin bool) {
	id := st.Assign(e, s)
	if id < 0 || id >= len(e.Nodes) || !e.CanServe(id, s) {
		// an invalid choice falls back to the default edge; counted as a miss of the strategy
		id = e.DefaultNode(s)
	}
	n := e.Nodes[id]
	q := e.QoE
	rtt := e.Topo.RTTLocal
	if n.Region != s.Region {
		rtt = e.Topo.RTTRemote
	}
	setup := q.SetupRTTs * rtt
	redirect := 0.0
	if id != e.DefaultNode(s) {
		redirect = q.RedirectRTTs * e.Topo.RTTLocal
		e.m.Redirects++
	}
	if st.Proactive() && s.FromFeed && !rejoin {
		setup, redirect = 0, 0 // connection pre-established while the previous stream played
	}
	miss := 0.0
	if !n.HasStream(s.Stream) {
		e.m.misses++
		miss = e.Topo.RTTEdgeL2 + q.GOPWaitMs
		if e.l2Edges[e.l2Of(n)][s.Stream] == 0 {
			miss += e.Topo.RTTL2Origin
		}
	}
	extra := 0.0
	if e.k(s.Stream) > 1 {
		extra = rtt // K parallel substream connections: one more round trip to the slowest
	}
	s.startMs = setup + redirect + miss + extra + q.FirstFrameMs
	if rejoin {
		e.m.StallSec += s.startMs / 1000 // the reconnect is a stall from the viewer's point of view
		if s.LowLat {
			e.m.llStall += s.startMs / 1000
		}
	} else {
		e.startups = append(e.startups, s.startMs)
		if s.LowLat {
			e.llStartups = append(e.llStartups, s.startMs)
		}
		e.m.Sessions++
		if s.LowLat {
			e.m.LLSessions++
		}
	}
	if s.LowLat {
		// glass to glass: capture/encode/ingest, origin->L2->edge relays, the
		// player's buffer, plus one hop for a remote (multihomed) or a
		// best-effort edge, plus substream reassembly jitter
		g := q.G2GBase + 2*q.G2GPerHop + q.BufferLowLat
		if n.Region != s.Region {
			g += q.G2GPerHop
		}
		if n.Kind == BestEffort {
			g += q.BEExtraHop
		}
		if e.k(s.Stream) > 1 {
			g += q.SubstreamJitter
		}
		s.g2gStatic = g
		if rejoin {
			s.drift += s.startMs / 1000 // the reconnect left the viewer behind live
		}
	}
	e.attach(s, id)
}

// Demand replays a workload without any CDN and returns, per 5-minute window,
// the average Mbps each region demands of each stream. Used by the LP bound.
func Demand(w Workload) []map[[2]int]float64 {
	streams := w.Streams()
	conc := map[[2]int]int{}
	ending := map[int][]*Session{}
	var out []map[[2]int]float64
	acc := map[[2]int]float64{}
	n := 0
	for t := 0; t < w.Ticks(); t++ {
		for _, s := range ending[t] {
			k := [2]int{s.Region, s.Stream}
			if conc[k]--; conc[k] == 0 {
				delete(conc, k)
			}
		}
		delete(ending, t)
		for _, s := range w.Arrivals(t) {
			if s.End <= t {
				continue
			}
			conc[[2]int{s.Region, s.Stream}]++
			ending[s.End] = append(ending[s.End], s)
		}
		for k, c := range conc {
			acc[k] += float64(c) * streams[k[1]].Bitrate
		}
		if n++; n == 300 {
			win := map[[2]int]float64{}
			for k, v := range acc {
				win[k] = v / 300
			}
			out = append(out, win)
			acc, n = map[[2]int]float64{}, 0
		}
	}
	return out
}

// Run simulates the workload with a strategy.
func (e *Engine) Run(st Strategy, w Workload) Metrics {
	e.Begin(st, w)
	e.Advance(e.T)
	return e.Finish()
}

// Begin, Advance and Finish run a simulation in steps (the gRPC
// environment's reset/step); Run is Begin + Advance(all) + Finish.
func (e *Engine) Begin(st Strategy, w Workload) {
	e.st, e.w, e.T, e.t, e.done = st, w, w.Ticks(), 0, false
	if e.g2gHist == nil {
		e.g2gHist = make([]int64, 3001)
	}
}

// Done reports whether the workload has been fully replayed.
func (e *Engine) Done() bool { return e.t >= e.T }

// Advance simulates up to n more seconds and reports whether the run is over.
func (e *Engine) Advance(n int) bool {
	st, w := e.st, e.w
	for end := min(e.T, e.t+n); e.t < end; e.t++ {
		// departures
		if lst, ok := e.ending[e.t]; ok {
			for _, s := range lst {
				if s.node >= 0 {
					e.detach(s)
				}
			}
			delete(e.ending, e.t)
		}
		// best-effort outages and capacity fluctuation
		for _, n := range e.Nodes {
			if n.Kind != BestEffort {
				continue
			}
			if e.t%60 == 0 {
				n.effCap = n.CapMbps * (e.Topo.BEFloor + (1-e.Topo.BEFloor)*e.rng.Float64())
			}
			if n.Down {
				if e.t >= n.downTill {
					n.Down = false
					n.bufN, n.bufLL = e.QoE.BufferNormal, e.QoE.BufferLowLat
				}
				continue
			}
			if e.Topo.BEMTBF > 0 && e.rng.Float64() < 1/e.Topo.BEMTBF {
				n.Down = true
				n.downTill = e.t + e.Topo.BEOutage
				e.m.Outages++
				victims := append([]*Session(nil), e.onNode[n.ID]...)
				for _, s := range victims {
					e.detach(s)
				}
				for _, s := range victims {
					e.m.Rejoins++
					e.join(st, s, true)
				}
			}
		}
		// arrivals
		for _, s := range w.Arrivals(e.t) {
			if s.End <= e.t {
				continue
			}
			s.node = -1
			e.join(st, s, false)
			e.ending[s.End] = append(e.ending[s.End], s)
		}
		st.OnTick(e)
		e.account()
	}
	return e.Done()
}

// Finish closes the last billing window and returns the metrics.
func (e *Engine) Finish() Metrics {
	if !e.done {
		e.flushWindow(true)
		e.done = true
	}
	return e.finish(e.st.Name(), e.w.Name(), e.T)
}

// StepStats are cumulative counters the environment differences per step.
type StepStats struct {
	Cost      float64 // average-billing cost so far (price-weighted Mbps-seconds / seconds elapsed is a rate)
	StallSec  float64
	WatchSec  float64
	Redirects int64
	Sessions  int64
}

func (e *Engine) Stats() StepStats {
	return StepStats{Cost: e.cumCost, StallSec: e.m.StallSec, WatchSec: e.m.WatchSec, Redirects: e.m.Redirects, Sessions: e.m.Sessions}
}

// Horizon is the workload length in seconds.
func (e *Engine) Horizon() int { return e.T }

// LLG2GNow is the p95 glass-to-glass latency (s) of the auction viewers attached right now.
func (e *Engine) LLG2GNow() float64 {
	if len(e.ll) == 0 {
		return 0
	}
	xs := make([]float64, len(e.ll))
	for i, s := range e.ll {
		xs[i] = s.g2gStatic + s.drift
	}
	return quantile(xs, 0.95)
}

func (e *Engine) account() {
	q := e.QoE
	for _, n := range e.Nodes {
		if n.Viewers == 0 || n.Down {
			if !n.Down {
				n.bufN, n.bufLL = q.BufferNormal, q.BufferLowLat
			}
			continue
		}
		e.m.WatchSec += float64(n.Viewers)
		e.m.llWatch += float64(n.llView)
		f := 1.0
		if n.LoadMbps > n.effCap {
			f = n.effCap / n.LoadMbps
		}
		served := math.Min(n.LoadMbps, n.effCap)
		e.winEgress[n.ID] += served
		normal := n.Viewers - n.llView
		if f < 1 {
			deficit := 1 - f
			if n.bufN >= deficit {
				n.bufN -= deficit
			} else {
				e.m.StallSec += (deficit - n.bufN) * float64(normal)
				n.bufN = 0
			}
			if n.bufLL >= deficit {
				n.bufLL -= deficit
			} else {
				per := deficit - n.bufLL
				st := per * float64(n.llView)
				e.m.StallSec += st
				e.m.llStall += st
				n.bufLL = 0
				if n.llView > 0 {
					for _, s := range e.onNode[n.ID] {
						if s.LowLat {
							s.drift += per + q.CatchUp // the catch-up below does not apply while stalled
						}
					}
				}
			}
		} else {
			n.bufN = math.Min(q.BufferNormal, n.bufN+q.RefillRate)
			n.bufLL = math.Min(q.BufferLowLat, n.bufLL+q.RefillRate)
		}
	}
	// auction cohort: catch up towards live, sample latency every 10 s
	sample := e.t%10 == 0
	for _, s := range e.ll {
		if s.drift > 0 {
			s.drift = max(0, s.drift-q.CatchUp)
		}
		if sample {
			b := int((s.g2gStatic + s.drift) * 100)
			e.g2gHist[min(b, len(e.g2gHist)-1)]++
		}
	}
	for _, n := range e.Nodes {
		if !n.Down {
			e.cumCost += math.Min(n.LoadMbps, n.effCap) * n.Price
		}
	}
	for r := 0; r < e.Topo.Regions; r++ {
		e.cumCost += e.midR[r]*e.Topo.MidgressPrice + e.originR[r]*e.Topo.OriginPrice
	}
	// midgress: L2 -> edge is one stream copy per (edge, stream); origin -> L2
	// depends on the transport (see originCopy). Both are kept incrementally.
	for r := 0; r < e.Topo.Regions; r++ {
		e.winMid[r] += e.midR[r]
		e.winOrigin[r] += e.originR[r]
		e.m.AvgMidMbps += e.midR[r] + e.originR[r]
	}
	for r := 0; r < e.Topo.Regions; r++ {
		if u := e.RegionUtil(r, Regular); u > e.m.PeakUtil {
			e.m.PeakUtil = u
		}
	}
	e.win++
	if e.win == 300 {
		e.flushWindow(false)
	}
}

func (e *Engine) flushWindow(final bool) {
	if e.win == 0 {
		return
	}
	d := float64(e.win)
	if final && e.win < 60 {
		d = 300 // a short tail window would overweight a partial average
	}
	for i := range e.winEgress {
		e.egressSer[i] = append(e.egressSer[i], e.winEgress[i]/d)
		e.m.AvgEgressMbps += e.winEgress[i]
		e.winEgress[i] = 0
	}
	for r := range e.winMid {
		e.midSer[r] = append(e.midSer[r], e.winMid[r]/d)
		e.originSer[r] = append(e.originSer[r], e.winOrigin[r]/d)
		e.winMid[r], e.winOrigin[r] = 0, 0
	}
	e.win = 0
}

func (e *Engine) finish(strategy, workload string, T int) Metrics {
	m := e.m
	m.Strategy, m.Workload, m.SimSec = strategy, workload, T
	mean := func(xs []float64) float64 {
		var t float64
		for _, x := range xs {
			t += x
		}
		if len(xs) == 0 {
			return 0
		}
		return t / float64(len(xs))
	}
	for i, ser := range e.egressSer {
		m.EgressCost += p95(ser) * e.Nodes[i].Price
		m.AvgBillCost += mean(ser) * e.Nodes[i].Price
	}
	for r := range e.midSer {
		m.MidgressCost += p95(e.midSer[r])*e.Topo.MidgressPrice + p95(e.originSer[r])*e.Topo.OriginPrice
		m.AvgBillCost += mean(e.midSer[r])*e.Topo.MidgressPrice + mean(e.originSer[r])*e.Topo.OriginPrice
	}
	m.Cost = m.EgressCost + m.MidgressCost
	m.AvgEgressMbps /= float64(T)
	m.AvgMidMbps /= float64(T)
	if m.AvgEgressMbps > 0 {
		m.MidEgressRatio = m.AvgMidMbps / m.AvgEgressMbps
	}
	if m.WatchSec > 0 {
		m.StallRatio = m.StallSec / m.WatchSec
	}
	if m.llWatch > 0 {
		m.LLStallRatio = m.llStall / m.llWatch
	}
	var tot, in int64
	for b, c := range e.g2gHist {
		tot += c
		if float64(b)/100 <= e.QoE.AuctionG2G+1e-9 {
			in += c
		}
	}
	if tot > 0 {
		m.LLInBound = float64(in) / float64(tot) // share of sampled viewer-time within the bound
		hq := func(q float64) float64 {
			target := int64(math.Ceil(q * float64(tot)))
			var c int64
			for b, n := range e.g2gHist {
				c += n
				if c >= target {
					return float64(b) * 10
				}
			}
			return float64(len(e.g2gHist)-1) * 10
		}
		m.LLG2GP50, m.LLG2GP95, m.LLG2GP99 = hq(0.5), hq(0.95), hq(0.99)
	}
	if m.Sessions > 0 {
		m.RedirectShare = float64(m.Redirects) / float64(m.Sessions+m.Rejoins)
		m.MissShare = float64(m.misses) / float64(m.Sessions+m.Rejoins)
	}
	m.StartupP50 = quantile(e.startups, 0.5)
	m.StartupP95 = quantile(e.startups, 0.95)
	m.LLStartupP95 = quantile(e.llStartups, 0.95)
	return m
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[int(q*float64(len(xs)-1))]
}

func sortFloats(xs []float64) { sort.Float64s(xs) }

func (m Metrics) String() string {
	return fmt.Sprintf("%-28s cost=%9.0f (egress %8.0f, midgress %7.0f) mid/egress=%.3f stall=%.4f%% startup p50/p95=%4.0f/%4.0fms redirects=%.0f%% cold-joins=%.1f%% auction g2g p95=%.0fms ok=%.1f%%",
		m.Strategy, m.Cost, m.EgressCost, m.MidgressCost, m.MidEgressRatio, 100*m.StallRatio, m.StartupP50, m.StartupP95,
		100*m.RedirectShare, 100*m.MissShare, m.LLG2GP95, 100*m.LLInBound)
}
