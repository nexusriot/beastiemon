package store

import (
	"sort"
	"sync"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
)

// Ring is a fixed-capacity circular buffer of Snapshots.
type Ring struct {
	mu    sync.RWMutex
	buf   []collect.Snapshot
	cap_  int
	head  int // next write index
	count int
}

func NewRing(capacity int) *Ring {
	return &Ring{
		buf:  make([]collect.Snapshot, capacity),
		cap_: capacity,
	}
}

func (r *Ring) Push(s collect.Snapshot) {
	// Drop the monotonic reading time.Now() carries. A stored sample's time is
	// a wall-clock stamp — it is what /api/series emits and what every
	// comparison here has to agree with. Keep the monotonic part and
	// Time.Before silently compares that instead, which never steps: after an
	// NTP correction backwards the samples would compare as ordered while the
	// timestamps served from them ran backwards.
	s.Time = s.Time.Round(0)

	r.mu.Lock()
	r.buf[r.head] = s
	r.head = (r.head + 1) % r.cap_
	if r.count < r.cap_ {
		r.count++
	}
	r.mu.Unlock()
}

// Last returns the most recent snapshot.
func (r *Ring) Last() (collect.Snapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.count == 0 {
		return collect.Snapshot{}, false
	}
	idx := (r.head - 1 + r.cap_) % r.cap_
	return r.buf[idx], true
}

// Since returns all snapshots with Time >= t, oldest first.
func (r *Ring) Since(t time.Time) []collect.Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.count == 0 {
		return nil
	}
	// Walk oldest→newest
	start := (r.head - r.count + r.cap_) % r.cap_
	var out []collect.Snapshot
	for i := 0; i < r.count; i++ {
		s := r.buf[(start+i)%r.cap_]
		if !s.Time.Before(t) {
			out = append(out, s)
		}
	}
	return byTime(out)
}

// byTime puts a slice taken in arrival order into wall-clock order. Push
// strips the monotonic reading, so the comparisons below are on the same clock
// the timestamps are served from.
//
// Arrival order is time order only while the clock moves forward. An NTP step
// backwards — a correction at boot, a VM resuming from suspend — leaves
// later-stamped samples sitting ahead of earlier ones, and that outlives the
// step: the mis-ordered samples stay until they age out of the ring, up to an
// hour at the default size. Two things downstream read this order as truth:
// the store/ring merge takes the first element as the oldest sample, and
// /api/series emits it as the timestamp column, which uPlot binary-searches.
// The check keeps the usual (already ordered) case to one linear pass.
func byTime(snaps []collect.Snapshot) []collect.Snapshot {
	sorted := true
	for i := 1; i < len(snaps); i++ {
		if snaps[i].Time.Before(snaps[i-1].Time) {
			sorted = false
			break
		}
	}
	if !sorted {
		sort.SliceStable(snaps, func(i, j int) bool {
			return snaps[i].Time.Before(snaps[j].Time)
		})
	}
	return snaps
}

// All returns every stored snapshot, oldest first.
func (r *Ring) All() []collect.Snapshot {
	return r.Since(time.Time{})
}
