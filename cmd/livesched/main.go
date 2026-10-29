// Command livesched runs the live-CDN scheduling simulator.
//
//	livesched run     one strategy on one workload, prints metrics (JSON with -json)
//	livesched sweep   strategies x scenarios x seeds x transports, JSON lines out
//	livesched search  simulator-verified strategy search (train/validate/test split)
//	livesched sample  write a small trace in the Twitch dataset format
//	livesched serve   gRPC environment (reset/step/observe), see env/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"google.golang.org/grpc"

	"livesched/env/envpb"
	"livesched/env/server"
	"livesched/search"
	"livesched/sim"
	"livesched/strategies"
	"livesched/workload"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: livesched {run|sweep|search|sample} [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "sweep":
		err = cmdSweep(os.Args[2:])
	case "search":
		err = search.Main(os.Args[2:])
	case "sample":
		err = cmdSample(os.Args[2:])
	case "demand":
		err = cmdDemand(os.Args[2:])
	case "grid":
		err = cmdGrid(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

type common struct {
	scenario   string
	hours      float64
	peak       int
	seed       uint64
	substreams int
	aware      bool
	whole      bool
	trace      string
	traceFrom  int
	streams    int
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.scenario, "scenario", "shows", "shows | matches | auctions")
	fs.Float64Var(&c.hours, "hours", 12, "simulated hours")
	fs.IntVar(&c.peak, "peak", 30000, "peak concurrent viewers")
	fs.Uint64Var(&c.seed, "seed", 1, "workload seed (one seed = one synthetic 'week')")
	fs.IntVar(&c.substreams, "substreams", 1, "K substreams per stream (Medley); 1 = whole streams")
	fs.BoolVar(&c.aware, "aware", false, "substream-aware caching (no full-stream pull on a substream miss)")
	fs.BoolVar(&c.whole, "whole-auctions", false, "scenario-aware transport: auction streams travel whole even with -substreams > 1")
	fs.StringVar(&c.trace, "trace", "", "Twitch dataset CSV to replay instead of the synthetic trace")
	fs.IntVar(&c.traceFrom, "trace-from", 0, "first 10-minute snapshot of the replay window")
	fs.IntVar(&c.streams, "streams", 3000, "streams in the synthetic workload")
}

func (c common) workload() (sim.Workload, error) {
	if c.trace != "" {
		return workload.LoadTwitch(c.trace, c.traceFrom, int(c.hours*6), workload.DefaultSynth().RegionWeights, 0.6, c.seed)
	}
	cfg := workload.DefaultSynth()
	cfg.Seed, cfg.Hours, cfg.PeakConcurrent, cfg.Scenario, cfg.Streams = c.seed, c.hours, c.peak, workload.Scenario(c.scenario), c.streams
	return workload.NewSynth(cfg), nil
}

func simulate(c common, st sim.Strategy) (sim.Metrics, error) {
	w, err := c.workload()
	if err != nil {
		return sim.Metrics{}, err
	}
	e := sim.New(sim.DefaultTopo(), sim.DefaultQoE(), sim.Transport{Substreams: c.substreams, SubstreamAware: c.aware, WholeAuctions: c.whole}, w, c.seed*1000+7)
	t0 := cpuSeconds()
	m := e.Run(st, w)
	m.CPUSec = cpuSeconds() - t0
	if m.CPUSec > 0 {
		m.ViewerMinPerCPUSec = m.WatchSec / 60 / m.CPUSec
		m.SessionsPerCPUSec = float64(m.Sessions) / m.CPUSec
	}
	if c.substreams > 1 {
		m.Strategy = fmt.Sprintf("%s+medley(K=%d,aware=%v,whole-auctions=%v)", m.Strategy, c.substreams, c.aware, c.whole)
	}
	return m, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var c common
	c.register(fs)
	name := fs.String("strategy", "hcdn", "static | globalhot | hcdn | hcdn-noS1..noS4 | hcdn-noproactive")
	params := fs.String("params", "", "HCDN parameter JSON (overrides -strategy hcdn defaults)")
	external := fs.String("external", "", "run a strategy process (policy as code), e.g. 'python3 -I strategies/examples/cold_aggregate.py'")
	sandbox := fs.Bool("sandbox", true, "with -external: no network, CPU and memory limits")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	var st sim.Strategy
	var err error
	if *external != "" {
		argv := strings.Fields(*external)
		if *sandbox {
			argv = strategies.SandboxCommand(600, 1<<30, argv...)
		}
		x, err := strategies.NewExternal("external:"+argv[len(argv)-1], argv)
		if err != nil {
			return err
		}
		defer func() {
			if why := x.Close(); why != "" {
				fmt.Fprintf(os.Stderr, "external strategy failed after %d calls: %s (%d joins fell back to the default edge)\n", x.Calls, why, x.Fallback)
			}
		}()
		st = x
	} else if *params != "" {
		p := strategies.DefaultHCDN()
		if err := json.Unmarshal([]byte(*params), &p); err != nil {
			return err
		}
		st = strategies.HCDN{P: p, Tag: "hcdn(custom)"}
	} else if st, err = strategies.ByName(*name); err != nil {
		return err
	}
	m, err := simulate(c, st)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(m)
	}
	fmt.Println(m)
	return nil
}

func cmdSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	var c common
	c.register(fs)
	strats := fs.String("strategies", "static,globalhot,hcdn", "comma list")
	scen := fs.String("scenarios", "shows,matches,auctions", "comma list")
	seeds := fs.Int("seeds", 3, "seeds per cell")
	ks := fs.String("transports", "1", "comma list of K[:aware][:whole], e.g. 1,3,3:aware,3:aware:whole")
	out := fs.String("out", "results/sweep.jsonl", "output JSON lines")
	par := fs.Int("parallel", runtime.NumCPU(), "parallel simulations")
	fs.Parse(args)
	type job struct {
		c  common
		st string
	}
	var jobs []job
	for _, sc := range strings.Split(*scen, ",") {
		for _, k := range strings.Split(*ks, ",") {
			for _, st := range strings.Split(*strats, ",") {
				for s := 1; s <= *seeds; s++ {
					cc := c
					cc.scenario, cc.seed = sc, uint64(s)
					parts := strings.Split(k, ":")
					fmt.Sscan(parts[0], &cc.substreams)
					for _, x := range parts[1:] {
						cc.aware = cc.aware || x == "aware"
						cc.whole = cc.whole || x == "whole"
					}
					jobs = append(jobs, job{cc, st})
				}
			}
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	var mu sync.Mutex
	sem := make(chan struct{}, *par)
	var wg sync.WaitGroup
	var firstErr error
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			st, err := strategies.ByName(j.st)
			if err == nil {
				var m sim.Metrics
				m, err = simulate(j.c, st)
				if err == nil {
					mu.Lock()
					enc.Encode(struct {
						sim.Metrics
						Seed       uint64 `json:"seed"`
						Substreams int    `json:"substreams"`
						Aware      bool   `json:"aware"`
						Whole      bool   `json:"whole_auctions"`
						Base       string `json:"base_strategy"`
					}{m, j.c.seed, j.c.substreams, j.c.aware, j.c.whole, j.st})
					log.Printf("%s seed=%d %s", j.c.scenario, j.c.seed, m)
					mu.Unlock()
				}
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	return firstErr
}

// cmdGrid sweeps HCDN's two cost/QoE knobs (when to offload to best-effort
// edges, and how full to let them get) to trace the cost-stall frontier.
func cmdGrid(args []string) error {
	fs := flag.NewFlagSet("grid", flag.ExitOnError)
	var c common
	c.register(fs)
	offload := fs.String("offload-util", "0.4,0.5,0.6,0.7,0.8,0.9,1.0", "values")
	beMax := fs.String("be-max-util", "0.4,0.5,0.6,0.7,0.8,0.9", "values")
	seeds := fs.Int("seeds", 3, "seeds")
	out := fs.String("out", "results/grid.jsonl", "output")
	fs.Parse(args)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	var mu sync.Mutex
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for _, o := range strings.Split(*offload, ",") {
		for _, b := range strings.Split(*beMax, ",") {
			for s := 1; s <= *seeds; s++ {
				p := strategies.DefaultHCDN()
				fmt.Sscan(o, &p.OffloadUtil)
				fmt.Sscan(b, &p.BEMaxUtil)
				cc := c
				cc.seed = uint64(s)
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					m, err := simulate(cc, strategies.HCDN{P: p, Tag: "hcdn-grid"})
					if err != nil {
						log.Print(err)
						return
					}
					mu.Lock()
					enc.Encode(map[string]any{"offload_util": p.OffloadUtil, "be_max_util": p.BEMaxUtil, "seed": cc.seed, "scenario": cc.scenario, "metrics": m})
					mu.Unlock()
				}()
			}
		}
	}
	wg.Wait()
	return nil
}

// cmdDemand exports topology, bitrates and per-window demand for the LP
// lower bound (python/lpbound.py).
func cmdDemand(args []string) error {
	fs := flag.NewFlagSet("demand", flag.ExitOnError)
	var c common
	c.register(fs)
	out := fs.String("out", "results/demand.json", "output")
	fs.Parse(args)
	cfg := workload.DefaultSynth()
	cfg.Seed, cfg.Hours, cfg.PeakConcurrent, cfg.Scenario, cfg.Streams = c.seed, c.hours, c.peak, workload.Scenario(c.scenario), c.streams
	w := workload.NewSynth(cfg)
	topo := sim.DefaultTopo()
	e := sim.New(topo, sim.DefaultQoE(), sim.Transport{Substreams: 1}, w, 1)
	type node struct {
		ID     int     `json:"id"`
		Kind   string  `json:"kind"`
		Region int     `json:"region"`
		Parent int     `json:"parent"`
		Cap    float64 `json:"cap"`
		Price  float64 `json:"price"`
	}
	var nodes []node
	for _, n := range e.Nodes {
		nodes = append(nodes, node{n.ID, n.Kind.String(), n.Region, n.Parent, n.CapMbps, n.Price})
	}
	var bit []float64
	for _, s := range w.Streams() {
		bit = append(bit, s.Bitrate)
	}
	type cell struct {
		R int     `json:"r"`
		S int     `json:"s"`
		D float64 `json:"mbps"`
	}
	var windows [][]cell
	for _, win := range sim.Demand(w) {
		var cs []cell
		for k, v := range win {
			cs = append(cs, cell{k[0], k[1], v})
		}
		windows = append(windows, cs)
	}
	b, err := json.Marshal(map[string]any{"nodes": nodes, "bitrate": bit, "windows": windows,
		"mid_price": topo.MidgressPrice, "origin_price": topo.OriginPrice, "l2_per_region": topo.L2PerRegion, "regions": topo.Regions})
	if err != nil {
		return err
	}
	return os.WriteFile(*out, b, 0o644)
}

func cmdSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	out := fs.String("out", "traces/twitch_sample.csv", "output path")
	users := fs.Int("users", 20000, "users")
	streamers := fs.Int("streamers", 2000, "streamers")
	snaps := fs.Int("snaps", 72, "10-minute snapshots")
	fs.Parse(args)
	return workload.WriteTwitchSample(*out, *users, *streamers, *snaps, 3)
}

// cpuSeconds is this process's user+system CPU time. Simulations in a sweep
// run in parallel, so per-run CPU is only exact for single runs (livesched
// run); the throughput figure in the README comes from single runs.
func cpuSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

// cmdServe runs the gRPC environment.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:50071", "listen address")
	fs.Parse(args)
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	g := grpc.NewServer()
	envpb.RegisterEnvServer(g, server.New())
	log.Printf("livesched env listening on %s", lis.Addr())
	return g.Serve(lis)
}
