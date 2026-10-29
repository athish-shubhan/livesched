// Package sim is a flow-level, discrete-time simulator of a hierarchical live
// streaming CDN: origin -> L2 parents -> edge nodes -> viewers.
//
// The unit of decision is a viewer join: a scheduling strategy maps the new
// session to an edge node (an HTTP 302 redirection in HCDN's terms). The
// engine then accounts for, every simulated second:
//
//   - egress: bitrate of every viewer on every edge, billed per edge at the
//     95th percentile of 5-minute averages (the multi-CDN billing model);
//   - midgress: one copy of a stream per (edge, stream) pair that has viewers
//     (L2 -> edge), and one copy per (L2, stream) pair that serves any edge
//     (origin -> L2). A cache miss pulls the full stream, even for a
//     substream request, which is the HCDN observation about substreams;
//   - QoE: startup delay at join, stalls from overloaded nodes and from
//     best-effort node outages, and glass-to-glass latency per session.
//
// It is a fluid model at node granularity (all viewers on a node share its
// bandwidth deficit), not a packet simulator. docs/DESIGN.md lists what that
// does and does not capture.
package sim

import "math"

type NodeKind uint8

const (
	Regular NodeKind = iota
	Multihomed
	BestEffort
)

func (k NodeKind) String() string { return [...]string{"regular", "multihomed", "best-effort"}[k] }

type Node struct {
	ID       int
	Kind     NodeKind
	Region   int
	Parent   int     // index into L2 parents (regional)
	CapMbps  float64 // egress capacity
	Price    float64 // per Mbps at the 95th percentile
	Down     bool    // best-effort outage in progress
	downTill int
	effCap   float64 // capacity actually available this minute (best-effort links fluctuate)

	// live state, maintained by the engine
	LoadMbps float64
	Viewers  int
	streams  map[int]int // stream -> viewers on this node
	bufN     float64     // fluid buffer (s) of normal-latency viewers
	bufLL    float64     // fluid buffer (s) of low-latency viewers
	llView   int
}

// Utilization of egress capacity.
func (n *Node) Utilization() float64 { return n.LoadMbps / n.CapMbps }

// HasStream reports whether the edge already carries the stream (a cache hit
// for a new viewer of it).
func (n *Node) HasStream(s int) bool { return n.streams[s] > 0 }

// StreamViewers is the number of viewers of s on this node.
func (n *Node) StreamViewers(s int) int { return n.streams[s] }

type Stream struct {
	ID      int
	Bitrate float64 // Mbps
	Home    int     // region where most of its audience is
	Auction bool    // e-commerce countdown auction
	MatchOf int     // paired stream id during a LIVE Match, -1 if none
}

// Session is one viewer watching one stream.
type Session struct {
	ID       int64
	Region   int
	Stream   int
	Start    int // tick (s)
	End      int
	LowLat   bool // latency-critical cohort (auction bidders)
	FromFeed bool // join came from the recommendation feed (pre-connectable)

	node    int
	startMs float64
	// auction cohort: glass-to-glass latency = g2gStatic + drift, where drift
	// is the time this viewer has fallen behind live (stalls add to it, the
	// low-latency player's catch-up playback removes it)
	g2gStatic float64
	drift     float64
	llIdx     int
}

// Topology parameters.
type TopoConfig struct {
	Regions         int
	RegularPerReg   int
	MultihomedPer   int
	BestEffortPer   int
	L2PerRegion     int
	RegularCap      float64
	MultihomedCap   float64
	BestEffortCap   float64
	RegularPrice    float64
	MultihomedPrice float64
	BestEffortPrice float64
	MidgressPrice   float64 // L2 -> edge, per Mbps p95
	OriginPrice     float64 // origin -> L2, per Mbps p95
	RTTLocal        float64 // ms viewer -> edge in its region
	RTTRemote       float64 // ms viewer -> multihomed edge in another region
	RTTEdgeL2       float64
	RTTL2Origin     float64
	BEMTBF          float64 // s, mean time between best-effort outages per node
	BEOutage        int     // s
	BEFloor         float64 // best-effort capacity each minute is uniform in [BEFloor, 1] x nominal
}

func DefaultTopo() TopoConfig {
	return TopoConfig{
		Regions: 4, RegularPerReg: 6, MultihomedPer: 2, BestEffortPer: 3, L2PerRegion: 3,
		RegularCap: 6000, MultihomedCap: 4000, BestEffortCap: 2500,
		RegularPrice: 1.0, MultihomedPrice: 1.1, BestEffortPrice: 0.4,
		MidgressPrice: 0.6, OriginPrice: 0.8,
		RTTLocal: 20, RTTRemote: 55, RTTEdgeL2: 10, RTTL2Origin: 40,
		BEMTBF: 2 * 3600, BEOutage: 60, BEFloor: 0.6,
	}
}

// QoE model constants (ms unless noted). See docs/DESIGN.md for sources.
type QoEConfig struct {
	SetupRTTs       float64 // TCP + TLS 1.3 + HTTP request
	RedirectRTTs    float64 // connect to the default edge + 302 round trip
	GOPWaitMs       float64 // waiting for a keyframe on a cache miss
	FirstFrameMs    float64
	FeedShare       float64 // share of joins from the feed (pre-connectable)
	G2GBase         float64 // s: capture, encode, ingest
	G2GPerHop       float64 // s per relay hop
	BufferNormal    float64 // s player buffer target
	BufferLowLat    float64 // s
	SubstreamJitter float64 // s extra reassembly buffer per session with K>1
	RefillRate      float64 // s of buffer regained per s when not constrained
	AuctionG2G      float64 // s, latency bound for the auction cohort
	CatchUp         float64 // s of drift a low-latency player removes per s (1.1x playback)
	BEExtraHop      float64 // s, best-effort edges sit one relay hop further from the L2
}

func DefaultQoE() QoEConfig {
	return QoEConfig{SetupRTTs: 3, RedirectRTTs: 4, GOPWaitMs: 150, FirstFrameMs: 100, FeedShare: 0.6,
		G2GBase: 1.0, G2GPerHop: 0.1, BufferNormal: 2.0, BufferLowLat: 0.6, SubstreamJitter: 0.3,
		RefillRate: 0.5, AuctionG2G: 2.0, CatchUp: 0.1, BEExtraHop: 0.1}
}

func p95(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	// insertion-free: small slices (one entry per 5 minutes)
	sortFloats(s)
	i := int(math.Ceil(0.95*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}
