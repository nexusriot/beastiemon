package store

import (
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
	return out
}

// All returns every stored snapshot, oldest first.
func (r *Ring) All() []collect.Snapshot {
	return r.Since(time.Time{})
}
