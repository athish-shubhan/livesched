package workload

import (
	"bufio"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"

	"livesched/sim"
)

// Twitch replays the public Twitch dataset (Rappaz, McAuley, Aberer, RecSys
// 2021): CSV rows "user_id,stream_id,streamer_username,time_start,time_stop"
// where times count 10-minute presence snapshots. A row means the user was
// seen watching the streamer from time_start to time_stop.
//
// The dataset has no geography, ISP or bitrate. Those are synthetic and
// documented: region = hash(user) weighted by RegionWeights, bitrate and
// home region per streamer by hash. Start and stop are spread uniformly
// inside their 10-minute snapshot.
type Twitch struct {
	name    string
	streams []sim.Stream
	T       int
	byTick  map[int][]*sim.Session
}

func hashStr(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func pickByHash(h uint64, w []float64) int {
	var t float64
	for _, x := range w {
		t += x
	}
	u := float64(h%1_000_000) / 1_000_000 * t
	for i, x := range w {
		u -= x
		if u < 0 {
			return i
		}
	}
	return len(w) - 1
}

// LoadTwitch reads rows whose interval overlaps [fromSnap, fromSnap+snaps).
func LoadTwitch(path string, fromSnap, snaps int, regionWeights []float64, feedShare float64, seed uint64) (*Twitch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rng := rand.New(rand.NewPCG(seed, 99))
	tw := &Twitch{name: "twitch", T: snaps * 600, byTick: map[int][]*sim.Session{}}
	ids := map[string]int{}
	bitrates := []float64{1.2, 2.5, 4.5}
	sc := bufio.NewScanner(f)
	var n int64
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		if len(parts) != 5 {
			continue
		}
		t0, e1 := strconv.Atoi(parts[3])
		t1, e2 := strconv.Atoi(parts[4])
		if e1 != nil || e2 != nil {
			continue
		}
		if t1 <= fromSnap || t0 >= fromSnap+snaps {
			continue
		}
		name := parts[2]
		sid, ok := ids[name]
		if !ok {
			sid = len(tw.streams)
			ids[name] = sid
			h := hashStr(name)
			tw.streams = append(tw.streams, sim.Stream{ID: sid, Bitrate: bitrates[pickByHash(h>>8, []float64{0.3, 0.5, 0.2})],
				Home: pickByHash(h>>20, regionWeights), MatchOf: -1})
		}
		start := (t0-fromSnap)*600 + rng.IntN(600)
		end := (t1-fromSnap)*600 + rng.IntN(600)
		if start < 0 {
			start = 0
		}
		if end > tw.T {
			end = tw.T
		}
		if end <= start {
			continue
		}
		n++
		region := pickByHash(hashStr(parts[0]), regionWeights)
		tw.byTick[start] = append(tw.byTick[start], &sim.Session{ID: n, Region: region, Stream: sid, Start: start, End: end,
			FromFeed: rng.Float64() < feedShare})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("no rows in snapshot window [%d, %d)", fromSnap, fromSnap+snaps)
	}
	return tw, nil
}

func (t *Twitch) Name() string                     { return t.name }
func (t *Twitch) Streams() []sim.Stream            { return t.streams }
func (t *Twitch) Ticks() int                       { return t.T }
func (t *Twitch) Arrivals(tick int) []*sim.Session { return t.byTick[tick] }

// WriteTwitchSample writes a synthetic trace in the Twitch CSV format, used by
// tests and to exercise the loader without the 3 GB download.
func WriteTwitchSample(path string, users, streamers, snaps int, seed uint64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	rng := rand.New(rand.NewPCG(seed, 5))
	z := make([]float64, streamers)
	for i := range z {
		z[i] = 1 / float64(i+1)
	}
	for u := 0; u < users; u++ {
		for k := 0; k < 3; k++ {
			s := pickWeighted(rng, z)
			t0 := rng.IntN(snaps)
			fmt.Fprintf(w, "%d,%d,streamer%d,%d,%d\n", u, 1000+s, s, t0, t0+1+rng.IntN(6))
		}
	}
	return w.Flush()
}
