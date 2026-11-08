package sim_test

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"livesched/sim"
	"livesched/strategies"
	"livesched/workload"
)

func small(seed uint64, sc workload.Scenario) *workload.Synth {
	cfg := workload.DefaultSynth()
	cfg.Seed, cfg.Hours, cfg.PeakConcurrent, cfg.Streams, cfg.Scenario = seed, 0.5, 3000, 400, sc
	return workload.NewSynth(cfg)
}

func run(st sim.Strategy, tr sim.Transport, seed uint64) (sim.Metrics, *sim.Engine) {
	w := small(seed, workload.Shows)
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), tr, w, 7)
	return e.Run(st, w), e
}

func TestDeterministic(t *testing.T) {
	a, _ := run(strategies.HCDN{P: strategies.DefaultHCDN()}, sim.Transport{Substreams: 1}, 3)
	b, _ := run(strategies.HCDN{P: strategies.DefaultHCDN()}, sim.Transport{Substreams: 1}, 3)
	if a.Cost != b.Cost || a.StallSec != b.StallSec || a.Sessions != b.Sessions {
		t.Fatalf("same seed, different result: %v vs %v", a, b)
	}
}

// Static mapping never redirects, and its startup delay is setup + first
// frame for a hit, plus the L2 (and origin) fetch for a miss.
func TestStaticNeverRedirects(t *testing.T) {
	m, _ := run(strategies.Static{}, sim.Transport{Substreams: 1}, 1)
	if m.Redirects != 0 {
		t.Fatalf("static redirected %d times", m.Redirects)
	}
	q, topo := sim.DefaultQoE(), sim.DefaultTopo()
	hit := q.SetupRTTs*topo.RTTLocal + q.FirstFrameMs
	if m.StartupP50 != hit {
		t.Fatalf("static p50 startup %.0f, want a cache hit's %.0f", m.StartupP50, hit)
	}
}

// Every redirect costs RedirectRTTs local round trips at startup unless the
// connection was pre-established from the feed.
func TestRedirectPenalty(t *testing.T) {
	p := strategies.DefaultHCDN()
	p.Proactive = false
	m, _ := run(strategies.HCDN{P: p}, sim.Transport{Substreams: 1}, 1)
	pre := strategies.DefaultHCDN()
	m2, _ := run(strategies.HCDN{P: pre}, sim.Transport{Substreams: 1}, 1)
	if m.Redirects == 0 {
		t.Fatal("hcdn should redirect")
	}
	if m2.StartupP50 >= m.StartupP50 {
		t.Fatalf("pre-connection should cut median startup: %.0f vs %.0f", m2.StartupP50, m.StartupP50)
	}
}

// The HCDN observation: substreams without substream-aware caching cost more
// internal traffic than whole streams, because a miss pulls the full stream.
func TestSubstreamMissAmplification(t *testing.T) {
	st := strategies.HCDN{P: strategies.DefaultHCDN()}
	whole, _ := run(st, sim.Transport{Substreams: 1}, 2)
	naive, _ := run(st, sim.Transport{Substreams: 3}, 2)
	aware, _ := run(st, sim.Transport{Substreams: 3, SubstreamAware: true}, 2)
	if !(naive.AvgMidMbps > whole.AvgMidMbps && aware.AvgMidMbps <= whole.AvgMidMbps) {
		t.Fatalf("midgress whole=%.0f naive=%.0f aware=%.0f", whole.AvgMidMbps, naive.AvgMidMbps, aware.AvgMidMbps)
	}
}

// Aggregating streams onto fewer edges must lower midgress relative to the
// static mapping on the same trace.
func TestAggregationCutsMidgress(t *testing.T) {
	st, _ := run(strategies.Static{}, sim.Transport{Substreams: 1}, 4)
	hc, _ := run(strategies.HCDN{P: strategies.DefaultHCDN()}, sim.Transport{Substreams: 1}, 4)
	if hc.MidEgressRatio >= st.MidEgressRatio {
		t.Fatalf("hcdn midgress/egress %.3f not below static %.3f", hc.MidEgressRatio, st.MidEgressRatio)
	}
}

// After the run every session has left, so all incrementally kept state must
// be back to zero (catches accounting leaks in attach/detach).
func TestAccountingReturnsToZero(t *testing.T) {
	cfg := workload.DefaultSynth()
	cfg.Hours, cfg.PeakConcurrent, cfg.Streams = 0.25, 2000, 300
	w := &drain{workload.NewSynth(cfg), 0}
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), sim.Transport{Substreams: 1}, w, 1)
	e.Run(strategies.HCDN{P: strategies.DefaultHCDN()}, w)
	for _, n := range e.Nodes {
		if n.Viewers != 0 || math.Abs(n.LoadMbps) > 1e-6 {
			t.Fatalf("node %d still has %d viewers / %.3f Mbps", n.ID, n.Viewers, n.LoadMbps)
		}
	}
}

// drain extends a workload with silence long enough for every session to end.
type drain struct {
	*workload.Synth
	extra int
}

func (d *drain) Ticks() int { return d.Synth.Ticks() + 6*3600 }
func (d *drain) Arrivals(t int) []*sim.Session {
	if t >= d.Synth.Ticks() {
		return nil
	}
	return d.Synth.Arrivals(t)
}

func TestTwitchLoader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "twitch.csv")
	if err := workload.WriteTwitchSample(p, 2000, 300, 36, 1); err != nil {
		t.Fatal(err)
	}
	w, err := workload.LoadTwitch(p, 0, 12, []float64{0.4, 0.3, 0.2, 0.1}, 0.6, 1)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for tick := 0; tick < w.Ticks(); tick++ {
		for _, s := range w.Arrivals(tick) {
			n++
			if s.Start != tick || s.End <= s.Start || s.End > w.Ticks() {
				t.Fatalf("bad session %+v", s)
			}
		}
	}
	if n == 0 || len(w.Streams()) == 0 {
		t.Fatal("no sessions loaded")
	}
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), sim.Transport{Substreams: 1}, w, 1)
	if m := e.Run(strategies.HCDN{P: strategies.DefaultHCDN()}, w); m.Sessions != int64(n) {
		t.Fatalf("simulated %d sessions, loaded %d", m.Sessions, n)
	}
}

// A candidate that never answers is cut off by the timeout and the run
// completes on the default edge instead of hanging.
func TestExternalTimeoutFallsBack(t *testing.T) {
	x, err := strategies.NewExternal("hang", []string{"sh", "-c", "sleep 30"})
	if err != nil {
		t.Skipf("cannot start helper process: %v", err)
	}
	x.Timeout = 50 * time.Millisecond
	w := small(5, workload.Shows)
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), sim.Transport{Substreams: 1}, w, 7)
	m := e.Run(x, w)
	if why := x.Close(); why != "timeout" {
		t.Fatalf("expected the candidate to be disabled by timeout, got %q", why)
	}
	if m.Sessions == 0 || x.Fallback == 0 {
		t.Fatalf("sessions=%d fallbacks=%d", m.Sessions, x.Fallback)
	}
}
