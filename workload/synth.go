// Package workload generates viewer sessions: a synthetic Twitch-like trace
// (heavy-tailed stream popularity, regional audiences, diurnal load, lognormal
// sessions), scenario overlays for LIVE Matches and countdown auctions, and a
// loader for the public Twitch dataset format.
package workload

import (
	"math"
	"math/rand/v2"
	"sort"

	"livesched/sim"
)

type Scenario string

const (
	Shows    Scenario = "shows"
	Matches  Scenario = "matches"
	Auctions Scenario = "auctions"
)

type SynthConfig struct {
	Seed             uint64
	Streams          int
	Hours            float64
	PeakConcurrent   int
	ZipfS            float64
	RegionWeights    []float64
	HomeAffinity     float64 // share of a stream's viewers in its home region
	MedianSessionSec float64
	SessionSigma     float64
	FeedShare        float64
	Scenario         Scenario
	AuctionShare     float64 // share of streams that are auctions (auctions scenario)
	BidderShare      float64 // share of an auction's viewers in the latency-critical cohort
}

func DefaultSynth() SynthConfig {
	return SynthConfig{Seed: 1, Streams: 3000, Hours: 12, PeakConcurrent: 30000, ZipfS: 1.1,
		RegionWeights: []float64{0.4, 0.3, 0.2, 0.1}, HomeAffinity: 0.7, MedianSessionSec: 600, SessionSigma: 1.0,
		FeedShare: 0.6, Scenario: Shows, AuctionShare: 0.05, BidderShare: 0.3}
}

type window struct{ start, end int }

type Synth struct {
	cfg     SynthConfig
	rng     *rand.Rand
	streams []sim.Stream
	weight  []float64
	live    []window
	T       int
	nextID  int64
	battles []battle
	auction map[int][]window // auction stream -> countdown windows
	cum     []float64
	cumIdx  []int
	cumAt   int
	base    float64 // sum of live weights without overlay boosts
}

type battle struct {
	a, b       int
	start, end int
}

func pickWeighted(r *rand.Rand, w []float64) int {
	var t float64
	for _, x := range w {
		t += x
	}
	u := r.Float64() * t
	for i, x := range w {
		u -= x
		if u < 0 {
			return i
		}
	}
	return len(w) - 1
}

func NewSynth(cfg SynthConfig) *Synth {
	s := &Synth{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed*31+1)), T: int(cfg.Hours * 3600), auction: map[int][]window{}, cumAt: -1}
	perm := s.rng.Perm(cfg.Streams)
	bitrates := []float64{1.2, 2.5, 4.5}
	for i := 0; i < cfg.Streams; i++ {
		st := sim.Stream{ID: i, Bitrate: bitrates[pickWeighted(s.rng, []float64{0.3, 0.5, 0.2})], Home: pickWeighted(s.rng, cfg.RegionWeights), MatchOf: -1}
		s.streams = append(s.streams, st)
		s.weight = append(s.weight, 1/math.Pow(float64(perm[i]+1), cfg.ZipfS))
		// live windows: 1-5 hours, starting anywhere from 2h before the run to its end
		start := -7200 + s.rng.IntN(s.T+7200)
		s.live = append(s.live, window{start, start + 3600 + s.rng.IntN(4*3600)})
	}
	// Popular streams are long-running channels: keep the top 2% live the whole run.
	for i := 0; i < cfg.Streams; i++ {
		if perm[i] < cfg.Streams/50 {
			s.live[i] = window{-1, s.T + 1}
		}
	}
	if cfg.Scenario == Matches {
		// LIVE Matches: every 30 minutes, 4 pairs of popular live streams battle
		// for 5 minutes. Both audiences join (correlated), and gifting in the
		// final 30 s, when the winner is decided, brings a surge of joins.
		top := s.topStreams(80)
		for t := 600; t+300 < s.T; t += 1800 {
			s.rng.Shuffle(len(top), func(i, j int) { top[i], top[j] = top[j], top[i] })
			for k := 0; k+1 < 8 && k+1 < len(top); k += 2 {
				s.battles = append(s.battles, battle{top[k], top[k+1], t, t + 300})
			}
		}
	}
	if cfg.Scenario == Auctions {
		// TikTok Shop countdown auctions: mid-popularity streams run 2-minute
		// countdowns every 10 minutes; with Extended mode, late bids reset the
		// timer, so joins surge in the last 30 s.
		for i := 0; i < cfg.Streams; i++ {
			if r := perm[i]; r >= 20 && s.rng.Float64() < cfg.AuctionShare*2 && r < cfg.Streams/2 {
				s.streams[i].Auction = true
				s.live[i] = window{-1, s.T + 1}
				off := s.rng.IntN(600)
				for t := off; t+120 < s.T; t += 600 {
					s.auction[i] = append(s.auction[i], window{t, t + 120})
				}
			}
		}
	}
	return s
}

func (s *Synth) topStreams(n int) []int {
	idx := make([]int, len(s.weight))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return s.weight[idx[a]] > s.weight[idx[b]] })
	if n > len(idx) {
		n = len(idx)
	}
	return append([]int(nil), idx[:n]...)
}

func (s *Synth) Name() string          { return string(s.cfg.Scenario) }
func (s *Synth) Streams() []sim.Stream { return s.streams }
func (s *Synth) Ticks() int            { return s.T }

// diurnal load shape over the run: 40% at the edges, 100% in the middle.
func (s *Synth) load(t int) float64 { return 0.4 + 0.6*math.Sin(math.Pi*float64(t)/float64(s.T)) }

func (s *Synth) boost(i, t int) float64 {
	b := 1.0
	for _, bt := range s.battles {
		if (bt.a == i || bt.b == i) && t >= bt.start && t < bt.end {
			b *= 2
			if t >= bt.end-30 {
				b *= 3
			}
		}
	}
	for _, w := range s.auction[i] {
		if t >= w.start && t < w.end {
			b *= 2
			if t >= w.end-30 {
				b *= 2
			}
		}
	}
	return b
}

func (s *Synth) rebuild(t int) {
	s.cum, s.cumIdx = s.cum[:0], s.cumIdx[:0]
	var c float64
	s.base = 0
	for i := range s.streams {
		if t >= s.live[i].start && t < s.live[i].end {
			s.base += s.weight[i]
			c += s.weight[i] * s.boost(i, t)
			s.cum = append(s.cum, c)
			s.cumIdx = append(s.cumIdx, i)
		}
	}
	s.cumAt = t
}

func (s *Synth) meanSession() float64 {
	return s.cfg.MedianSessionSec * math.Exp(s.cfg.SessionSigma*s.cfg.SessionSigma/2)
}

func (s *Synth) session(t int, warm bool) *sim.Session {
	if len(s.cum) == 0 {
		return nil
	}
	u := s.rng.Float64() * s.cum[len(s.cum)-1]
	st := s.cumIdx[sort.SearchFloat64s(s.cum, u)]
	region := s.streams[st].Home
	if s.rng.Float64() > s.cfg.HomeAffinity {
		region = pickWeighted(s.rng, s.cfg.RegionWeights)
	}
	d := s.cfg.MedianSessionSec * math.Exp(s.cfg.SessionSigma*s.rng.NormFloat64())
	if warm {
		d *= s.rng.Float64() // residual life of a session already in progress
	}
	end := t + 1 + int(d)
	if e := s.live[st].end; end > e {
		end = e
	}
	s.nextID++
	return &sim.Session{ID: s.nextID, Region: region, Stream: st, Start: t, End: end,
		LowLat: s.streams[st].Auction && s.rng.Float64() < s.cfg.BidderShare, FromFeed: s.rng.Float64() < s.cfg.FeedShare}
}

// Arrivals is called once per tick in order.
func (s *Synth) Arrivals(t int) []*sim.Session {
	period := 60
	if len(s.battles) > 0 || len(s.auction) > 0 {
		period = 5 // overlays change weights at a finer grain
	}
	if s.cumAt < 0 || t-s.cumAt >= period {
		s.rebuild(t)
	}
	var out []*sim.Session
	if t == 0 {
		for i := 0; i < int(float64(s.cfg.PeakConcurrent)*s.load(0)); i++ {
			if x := s.session(0, true); x != nil {
				out = append(out, x)
			}
		}
	}
	lambda := float64(s.cfg.PeakConcurrent) * s.load(t) / s.meanSession()
	// overlays add arrivals: the rate scales with the extra weight they add
	if s.base > 0 && len(s.cum) > 0 {
		lambda *= s.cum[len(s.cum)-1] / s.base
	}
	n := poisson(s.rng, lambda)
	for i := 0; i < n; i++ {
		if x := s.session(t, false); x != nil {
			out = append(out, x)
		}
	}
	return out
}

func poisson(r *rand.Rand, l float64) int {
	if l > 30 {
		return int(math.Max(0, math.Round(l+math.Sqrt(l)*r.NormFloat64())))
	}
	L, k, p := math.Exp(-l), 0, 1.0
	for {
		p *= r.Float64()
		if p <= L {
			return k
		}
		k++
	}
}
