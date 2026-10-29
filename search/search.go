// Package search is the simulator-verified strategy search: a proposer
// suggests a candidate strategy, the simulator scores it on training traces,
// the best candidates are re-checked on a validation trace, and only the
// winner is reported on held-out test traces (different seeds, i.e.
// different synthetic weeks, and a different scenario mix).
//
// Proposers are pluggable. "mutate" is an evolutionary (mu + lambda) search
// over the HCDN parameter space and needs no model. "llm" formats the
// history into a prompt and asks a Model for the next candidate as JSON;
// Model is an interface, and the only implementation shipped is CommandModel,
// which pipes the prompt to any command you configure (no provider is baked
// in). The proposer never touches the simulator: it reads metrics of past
// trials (read-only) and submits candidates, under a call and token budget.
package search

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"livesched/sim"
	"livesched/strategies"
	"livesched/workload"
)

// Candidate is what a proposer submits.
type Candidate struct {
	P strategies.HCDNParams `json:"params"`
}

type Trial struct {
	N          int       `json:"n"`
	Candidate  Candidate `json:"candidate"`
	Rationale  string    `json:"rationale,omitempty"`
	Train      Score     `json:"train"`
	Val        *Score    `json:"val,omitempty"`
	ElapsedSec float64   `json:"elapsed_sec"` // since the search started, when this trial finished
}

// Score aggregates metrics over the traces of one split.
type Score struct {
	Cost       float64 `json:"cost"`
	StallRatio float64 `json:"stall_ratio"`
	StartupP95 float64 `json:"startup_p95_ms"`
	LLInBound  float64 `json:"auction_cohort_g2g_within_bound"`
	Feasible   bool    `json:"feasible"`
	Objective  float64 `json:"objective"`
}

// Constraint is a QoE SLO: stall ratio at most Stall (absolute, e.g. 0.1%)
// and p95 startup no more than 25 ms above the reference's. The objective is
// cost, with a steep penalty for infeasible candidates so the search still
// gets a gradient. The reference (HCDN with default parameters) is a point of
// comparison and need not meet the SLO itself.
type Constraint struct {
	Stall, Startup float64
}

func (c Constraint) score(s Score) Score {
	s.Feasible = s.StallRatio <= c.Stall && s.StartupP95 <= c.Startup+25
	s.Objective = s.Cost
	if !s.Feasible {
		over := math.Max(0, s.StallRatio-c.Stall)*1e4 + math.Max(0, s.StartupP95-c.Startup-25)/10
		s.Objective = s.Cost * (1.1 + over)
	}
	return s
}

type Env struct {
	Scenarios []string
	Seeds     []uint64
	Hours     float64
	Peak      int
}

func (e Env) Eval(p strategies.HCDNParams) Score {
	var mu sync.Mutex
	var sum Score
	var wg sync.WaitGroup
	n := 0
	for _, sc := range e.Scenarios {
		for _, seed := range e.Seeds {
			wg.Add(1)
			n++
			go func(sc string, seed uint64) {
				defer wg.Done()
				cfg := workload.DefaultSynth()
				cfg.Seed, cfg.Hours, cfg.PeakConcurrent, cfg.Scenario = seed, e.Hours, e.Peak, workload.Scenario(sc)
				w := workload.NewSynth(cfg)
				m := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), sim.Transport{Substreams: 1}, w, seed*1000+7).Run(strategies.HCDN{P: p}, w)
				mu.Lock()
				sum.Cost += m.Cost
				sum.StallRatio += m.StallRatio
				sum.StartupP95 += m.StartupP95
				sum.LLInBound += m.LLInBound
				mu.Unlock()
			}(sc, seed)
		}
	}
	wg.Wait()
	f := float64(n)
	return Score{Cost: sum.Cost / f, StallRatio: sum.StallRatio / f, StartupP95: sum.StartupP95 / f, LLInBound: sum.LLInBound / f}
}

// ---- proposers

type Proposer interface {
	Name() string
	Propose(ctx context.Context, hist []Trial, ref Score) (Candidate, string, error)
}

type Mutate struct {
	rng     *rand.Rand
	Pop     int
	Explore int // random candidates before mutation starts
}

func NewMutate(seed uint64) *Mutate {
	return &Mutate{rng: rand.New(rand.NewPCG(seed, 11)), Pop: 6, Explore: 20}
}

func (m *Mutate) Name() string { return "mutate" }

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }

func (m *Mutate) random() strategies.HCDNParams {
	r := m.rng
	p := strategies.DefaultHCDN()
	p.Frozen, p.Cold, p.Hot, p.Offload = r.Float64() < 0.8, r.Float64() < 0.8, r.Float64() < 0.8, r.Float64() < 0.8
	p.FrozenMax = r.IntN(11)
	p.ColdMax = 1 + r.IntN(200)
	p.HotUtil = 0.5 + 0.45*r.Float64()
	p.OffloadUtil = 0.3 + 0.7*r.Float64()
	p.MaxUtil = 0.7 + 0.29*r.Float64()
	p.BEMaxUtil = 0.3 + 0.65*r.Float64()
	return p
}

func (m *Mutate) Propose(_ context.Context, hist []Trial, _ Score) (Candidate, string, error) {
	// explore first: uniform random candidates, then (mu + lambda) mutation
	if len(hist) <= m.Explore {
		return Candidate{P: m.random()}, "random exploration", nil
	}
	// parent: tournament among the best Pop trials by train objective
	sorted := append([]Trial(nil), hist...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Train.Objective < sorted[j].Train.Objective })
	if len(sorted) > m.Pop {
		sorted = sorted[:m.Pop]
	}
	parent := sorted[m.rng.IntN(len(sorted))].Candidate.P
	p := parent
	r := m.rng
	flip := func(b bool) bool {
		if r.Float64() < 0.1 {
			return !b
		}
		return b
	}
	p.Frozen, p.Cold, p.Hot, p.Offload, p.ProtectLowLat = flip(p.Frozen), flip(p.Cold), flip(p.Hot), flip(p.Offload), flip(p.ProtectLowLat)
	p.Proactive = true // pure win in this model; not worth search budget
	mutI := func(x, lo, hi int, sd float64) int {
		if r.Float64() < 0.5 {
			return x
		}
		return int(clamp(math.Round(float64(x)*math.Exp(sd*r.NormFloat64())+r.NormFloat64()), float64(lo), float64(hi)))
	}
	mutF := func(x, lo, hi, sd float64) float64 {
		if r.Float64() < 0.5 {
			return x
		}
		return clamp(x+sd*r.NormFloat64(), lo, hi)
	}
	p.FrozenMax = mutI(p.FrozenMax, 0, 20, 0.4)
	p.ColdMax = mutI(p.ColdMax, 1, 400, 0.5)
	p.HotUtil = mutF(p.HotUtil, 0.3, 0.98, 0.1)
	p.OffloadUtil = mutF(p.OffloadUtil, 0.3, 1.0, 0.12)
	p.MaxUtil = mutF(p.MaxUtil, 0.5, 0.99, 0.05)
	p.BEMaxUtil = mutF(p.BEMaxUtil, 0.2, 0.95, 0.12)
	return Candidate{P: p}, "mutation of a top-" + fmt.Sprint(m.Pop) + " parent", nil
}

// Model is the pluggable LLM interface. Implement it for any provider.
type Model interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// CommandModel sends the prompt on stdin to a command and reads the reply
// from stdout, e.g. a script that calls your provider of choice.
type CommandModel struct {
	Cmd     string
	Timeout time.Duration
}

func (c CommandModel) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Cmd)
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	return string(out), err
}

type LLM struct {
	Model     Model
	MaxCalls  int
	MaxTokens int // approximate prompt+reply budget (4 chars ~ 1 token)
	calls     int
	tokens    int
	fallback  *Mutate
}

func NewLLM(m Model, calls, tokens int, seed uint64) *LLM {
	return &LLM{Model: m, MaxCalls: calls, MaxTokens: tokens, fallback: NewMutate(seed)}
}

func (l *LLM) Name() string { return "llm" }

func (l *LLM) Propose(ctx context.Context, hist []Trial, ref Score) (Candidate, string, error) {
	if l.calls >= l.MaxCalls || l.tokens >= l.MaxTokens {
		c, why, err := l.fallback.Propose(ctx, hist, ref)
		return c, "budget exhausted, " + why, err
	}
	prompt := BuildPrompt(hist, ref)
	l.calls++
	l.tokens += len(prompt) / 4
	reply, err := l.Model.Complete(ctx, prompt)
	l.tokens += len(reply) / 4
	if err != nil {
		c, why, _ := l.fallback.Propose(ctx, hist, ref)
		return c, "model error (" + err.Error() + "), " + why, nil
	}
	var out struct {
		Params    strategies.HCDNParams `json:"params"`
		Rationale string                `json:"rationale"`
	}
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start || json.Unmarshal([]byte(reply[start:end+1]), &out) != nil {
		c, why, _ := l.fallback.Propose(ctx, hist, ref)
		return c, "unparseable reply, " + why, nil
	}
	return Candidate{P: out.Params}, out.Rationale, nil
}

// BuildPrompt is the only view of the system a model gets: the parameter
// schema, the constraint, and a table of past trials (read-only metrics).
func BuildPrompt(hist []Trial, ref Score) string {
	var b strings.Builder
	b.WriteString(`You are tuning a live-streaming CDN scheduling policy (HCDN-style) in a simulator.
Goal: minimize bandwidth cost (95th-percentile billing, egress + midgress) while keeping
stall ratio <= the SLO and p95 startup <= reference + 25 ms.
Parameters (JSON): frozen,cold,hot,offload,proactive,protect_lowlat (bool);
frozen_max (int 0-20, regional viewers below which a stream goes to multihomed edges);
cold_max (int 1-400, regional viewers below which a stream is pinned to one edge);
hot_util (0.3-0.98), offload_util (0.3-1.0, regional utilization above which hot streams go to cheap best-effort edges),
max_util (0.5-0.99), be_max_util (0.2-0.95, cap on best-effort edge utilization; their real capacity fluctuates).
`)
	fmt.Fprintf(&b, "Reference (HCDN defaults; may itself violate the SLO): cost=%.0f stall=%.5f startup_p95=%.0fms\n", ref.Cost, ref.StallRatio, ref.StartupP95)
	b.WriteString("Past trials (best 15 by objective, then latest 5):\n")
	sorted := append([]Trial(nil), hist...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Train.Objective < sorted[j].Train.Objective })
	show := sorted
	if len(show) > 15 {
		show = show[:15]
	}
	if len(hist) > 5 {
		show = append(show, hist[len(hist)-5:]...)
	}
	for _, t := range show {
		pj, _ := json.Marshal(t.Candidate.P)
		fmt.Fprintf(&b, "#%d %s -> cost=%.0f stall=%.5f startup_p95=%.0f feasible=%v\n", t.N, pj, t.Train.Cost, t.Train.StallRatio, t.Train.StartupP95, t.Train.Feasible)
	}
	b.WriteString(`Reply with one JSON object: {"params": {...all fields...}, "rationale": "<one sentence>"}`)
	return b.String()
}

// ---- driver

func parseSeeds(s string) []uint64 {
	var out []uint64
	for _, f := range strings.Split(s, ",") {
		var x uint64
		fmt.Sscan(f, &x)
		out = append(out, x)
	}
	return out
}

func Main(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	budget := fs.Int("budget", 60, "candidate evaluations on the training split")
	proposer := fs.String("proposer", "mutate", "mutate | llm")
	modelCmd := fs.String("model-cmd", "", "llm proposer: command that reads a prompt on stdin and prints a reply")
	maxCalls := fs.Int("max-calls", 40, "llm proposer: model call budget")
	maxTokens := fs.Int("max-tokens", 200000, "llm proposer: approximate token budget")
	train := fs.String("train", "11,12,14", "training seeds (synthetic weeks)")
	val := fs.String("val", "13", "validation seeds")
	test := fs.String("test", "21,22,23", "held-out test seeds")
	trainSc := fs.String("train-scenarios", "shows", "scenarios used for training")
	testSc := fs.String("test-scenarios", "shows,matches,auctions", "scenarios used for testing")
	hours := fs.Float64("hours", 12, "simulated hours per trace")
	peak := fs.Int("peak", 30000, "peak concurrent viewers")
	out := fs.String("out", "results/search", "output dir")
	seed := fs.Uint64("seed", 1, "proposer seed")
	stallSLO := fs.Float64("stall-slo", 0.001, "stall ratio SLO (0.001 = 0.1% of watch time)")
	fs.Parse(args)
	runtime.GOMAXPROCS(runtime.NumCPU())
	os.MkdirAll(*out, 0o755)

	envTrain := Env{Scenarios: strings.Split(*trainSc, ","), Seeds: parseSeeds(*train), Hours: *hours, Peak: *peak}
	envVal := Env{Scenarios: strings.Split(*trainSc, ","), Seeds: parseSeeds(*val), Hours: *hours, Peak: *peak}
	def := strategies.DefaultHCDN()
	ref := envTrain.Eval(def)
	cons := Constraint{Stall: *stallSLO, Startup: ref.StartupP95}
	ref = cons.score(ref)

	var prop Proposer = NewMutate(*seed)
	if *proposer == "llm" {
		if *modelCmd == "" {
			return fmt.Errorf("-proposer llm needs -model-cmd")
		}
		prop = NewLLM(CommandModel{Cmd: *modelCmd, Timeout: 2 * time.Minute}, *maxCalls, *maxTokens, *seed)
	}
	hist := []Trial{{N: 0, Candidate: Candidate{P: def}, Rationale: "reference: HCDN defaults", Train: ref}}
	logf, _ := os.Create(filepath.Join(*out, "trials.jsonl"))
	defer logf.Close()
	enc := json.NewEncoder(logf)
	enc.Encode(hist[0])
	start := time.Now()
	for i := 1; i <= *budget; i++ {
		c, why, err := prop.Propose(context.Background(), hist, ref)
		if err != nil {
			return err
		}
		s := cons.score(envTrain.Eval(c.P))
		t := Trial{N: i, Candidate: c, Rationale: why, Train: s, ElapsedSec: time.Since(start).Seconds()}
		hist = append(hist, t)
		enc.Encode(t)
		fmt.Printf("trial %3d cost=%.0f (%+.1f%% vs ref) stall=%.5f p95=%.0f feasible=%v\n", i, s.Cost, 100*(s.Cost/ref.Cost-1), s.StallRatio, s.StartupP95, s.Feasible)
	}
	// Validation: re-score the 8 best training candidates on an unseen
	// week and keep the best there (guards against fitting training noise).
	sorted := append([]Trial(nil), hist[1:]...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Train.Objective < sorted[j].Train.Objective })
	refVal := envVal.Eval(def)
	consVal := Constraint{Stall: *stallSLO, Startup: refVal.StartupP95}
	refVal = consVal.score(refVal)
	var best *Trial
	for i := 0; i < len(sorted) && i < 8; i++ {
		if !sorted[i].Train.Feasible {
			continue
		}
		v := consVal.score(envVal.Eval(sorted[i].Candidate.P))
		sorted[i].Val = &v
		fmt.Printf("validate #%d: cost=%.0f (%+.1f%% vs ref on val) feasible=%v\n", sorted[i].N, v.Cost, 100*(v.Cost/refVal.Cost-1), v.Feasible)
		if v.Feasible && (best == nil || v.Objective < best.Val.Objective) {
			best = &sorted[i]
		}
	}
	report := map[string]any{"proposer": prop.Name(), "budget": *budget, "wall_sec": time.Since(start).Seconds(),
		"train_ref": ref, "val_ref": refVal, "stall_slo": *stallSLO, "seed": *seed}
	// Search cost: the first candidate that beats the reference on the
	// training weeks at equal or better QoE (lower cost, stall ratio no
	// higher, p95 startup no more than 25 ms higher), and the first that
	// is cheaper and meets the stall SLO.
	traces := len(envTrain.Scenarios) * len(envTrain.Seeds)
	for _, t := range hist[1:] {
		if t.Train.Cost < ref.Cost && t.Train.StallRatio <= ref.StallRatio && t.Train.StartupP95 <= ref.StartupP95+25 {
			report["first_beats_reference"] = map[string]any{"trial": t.N, "elapsed_sec": t.ElapsedSec, "simulations": t.N * traces,
				"cost_change_pct": 100 * (t.Train.Cost/ref.Cost - 1)}
			break
		}
	}
	for _, t := range hist[1:] {
		if t.Train.Cost < ref.Cost && t.Train.Feasible {
			report["first_cheaper_within_slo"] = map[string]any{"trial": t.N, "elapsed_sec": t.ElapsedSec, "simulations": t.N * traces,
				"cost_change_pct": 100 * (t.Train.Cost/ref.Cost - 1)}
			break
		}
	}
	if best == nil {
		fmt.Println("no candidate stayed feasible on validation: the reference stands")
		report["winner"] = nil
	} else {
		report["winner"] = best
		// Test: held-out weeks, every scenario, separately.
		tests := map[string]any{}
		for _, sc := range strings.Split(*testSc, ",") {
			envTest := Env{Scenarios: []string{sc}, Seeds: parseSeeds(*test), Hours: *hours, Peak: *peak}
			r, w := envTest.Eval(def), envTest.Eval(best.Candidate.P)
			ct := Constraint{Stall: *stallSLO, Startup: r.StartupP95}
			r, w = ct.score(r), ct.score(w)
			tests[sc] = map[string]any{"reference": r, "winner": w, "cost_change_pct": 100 * (w.Cost/r.Cost - 1)}
			fmt.Printf("test %-8s ref cost=%.0f stall=%.5f p95=%.0f | winner cost=%.0f (%+.1f%%) stall=%.5f p95=%.0f auction-ok %.3f->%.3f\n",
				sc, r.Cost, r.StallRatio, r.StartupP95, w.Cost, 100*(w.Cost/r.Cost-1), w.StallRatio, w.StartupP95, r.LLInBound, w.LLInBound)
		}
		report["test"] = tests
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	return os.WriteFile(filepath.Join(*out, "report.json"), b, 0o644)
}
