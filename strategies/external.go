package strategies

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"livesched/sim"
)

// External runs a candidate strategy written in any language as a separate,
// sandboxed process (policy as code). This is how model-generated strategies
// are evaluated without trusting them: the process gets no network (its own
// network namespace via `unshare -rn`), CPU-time and memory limits
// (`prlimit`), an empty environment, and only a read-only observation per
// join over stdin. It answers with a node id on stdout. A slow, crashing or
// invalid candidate is cut off and the rest of the run falls back to the
// default edge, which shows up as a bad score rather than a hung search.
//
// Protocol, one JSON object per line:
//
//	-> {"t":123,"session":{"region":0,"stream":42,"lowlat":false,"feed":true},
//	    "region_viewers":17,"global_viewers":40,"region_util":0.61,
//	    "candidates":[{"id":3,"kind":"regular","region":0,"util":0.42,"has_stream":true,"stream_viewers":5}, ...],
//	    "default":2}
//	<- {"node":3}
type External struct {
	name     string
	cmd      *exec.Cmd
	in       *bufio.Writer
	stdin    io.WriteCloser
	out      *bufio.Reader
	Timeout  time.Duration
	failed   string
	Calls    int64
	Fallback int64
}

// SandboxCommand wraps argv with the sandbox used for untrusted candidates.
func SandboxCommand(cpuSec, memBytes int, argv ...string) []string {
	return append([]string{"prlimit", fmt.Sprintf("--cpu=%d", cpuSec), fmt.Sprintf("--as=%d", memBytes), "--nofile=32",
		"--", "unshare", "-rn", "--"}, argv...)
}

func NewExternal(name string, argv []string) (*External, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = []string{"PATH=/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE=1"}
	c.Stderr = os.Stderr
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &External{name: name, cmd: c, in: bufio.NewWriter(stdin), stdin: stdin, out: bufio.NewReader(stdout), Timeout: 2 * time.Second}, nil
}

func (x *External) Name() string       { return x.name }
func (x *External) OnTick(*sim.Engine) {}
func (x *External) Proactive() bool    { return true }

type extCand struct {
	ID            int     `json:"id"`
	Kind          string  `json:"kind"`
	Region        int     `json:"region"`
	Util          float64 `json:"util"`
	HasStream     bool    `json:"has_stream"`
	StreamViewers int     `json:"stream_viewers"`
}

type extReq struct {
	T       int `json:"t"`
	Session struct {
		Region int  `json:"region"`
		Stream int  `json:"stream"`
		LowLat bool `json:"lowlat"`
		Feed   bool `json:"feed"`
	} `json:"session"`
	RegionViewers int       `json:"region_viewers"`
	GlobalViewers int       `json:"global_viewers"`
	RegionUtil    float64   `json:"region_util"`
	Candidates    []extCand `json:"candidates"`
	Default       int       `json:"default"`
}

func (x *External) Assign(e *sim.Engine, s *sim.Session) int {
	def := e.DefaultNode(s)
	if x.failed != "" {
		x.Fallback++
		return def
	}
	x.Calls++
	var r extReq
	r.T = e.Now()
	r.Session.Region, r.Session.Stream, r.Session.LowLat, r.Session.Feed = s.Region, s.Stream, s.LowLat, s.FromFeed
	r.RegionViewers, r.GlobalViewers, r.RegionUtil, r.Default = e.RegionViewers(s.Region, s.Stream), e.GlobalViewers(s.Stream), e.RegionUtil(s.Region, sim.Regular), def
	for _, n := range e.Nodes {
		if !e.CanServe(n.ID, s) {
			continue
		}
		r.Candidates = append(r.Candidates, extCand{n.ID, n.Kind.String(), n.Region, n.Utilization(), n.HasStream(s.Stream), n.StreamViewers(s.Stream)})
	}
	b, _ := json.Marshal(r)
	type reply struct {
		node int
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		if _, err := x.in.Write(append(b, '\n')); err != nil {
			ch <- reply{-1, err}
			return
		}
		if err := x.in.Flush(); err != nil {
			ch <- reply{-1, err}
			return
		}
		line, err := x.out.ReadString('\n')
		if err != nil {
			ch <- reply{-1, err}
			return
		}
		var out struct {
			Node int `json:"node"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &out); err != nil {
			ch <- reply{-1, fmt.Errorf("bad reply %q", line)}
			return
		}
		ch <- reply{out.Node, nil}
	}()
	select {
	case rp := <-ch:
		if rp.err != nil {
			x.fail(rp.err.Error())
			return def
		}
		return rp.node // the engine validates it and falls back if it cannot serve
	case <-time.After(x.Timeout):
		x.fail("timeout")
		return def
	}
}

func (x *External) fail(why string) {
	x.failed = why
	fmt.Fprintf(os.Stderr, "external strategy %s disabled: %s\n", x.name, why)
	x.cmd.Process.Kill()
}

// Close stops the process and reports why it failed, if it did.
func (x *External) Close() string {
	if x.cmd.Process != nil {
		x.in.Flush()
		x.stdin.Close()
		x.cmd.Process.Kill()
		x.cmd.Wait()
	}
	return x.failed
}
