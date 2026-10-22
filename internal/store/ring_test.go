package store

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
)

// Since promises oldest-first, and two callers read that as truth: the
// store/ring merge takes the first element as the oldest sample, and
// /api/series emits the order as its timestamp column, which uPlot
// binary-searches. Arrival order only matches time order while the clock
// moves forward — an NTP step back leaves later samples sitting ahead of
// earlier ones for as long as they stay in the ring.
func TestRingSinceIsTimeOrderedAcrossAClockStepBack(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := NewRing(100)

	// Three samples, then the clock jumps ten minutes back, then three more.
	for i := 0; i < 3; i++ {
		r.Push(collect.Snapshot{Time: base.Add(time.Duration(i) * time.Second)})
	}
	stepped := base.Add(-10 * time.Minute)
	for i := 0; i < 3; i++ {
		r.Push(collect.Snapshot{Time: stepped.Add(time.Duration(i) * time.Second)})
	}

	got := r.Since(time.Time{})
	if len(got) != 6 {
		t.Fatalf("got %d samples, want all 6", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Fatalf("Since must return time order: [%d]=%v before [%d]=%v",
				i, got[i].Time, i-1, got[i-1].Time)
		}
	}
	// Nothing is dropped: the post-step samples are simply earlier.
	if !got[0].Time.Equal(stepped) {
		t.Errorf("oldest = %v, want the earliest sample %v", got[0].Time, stepped)
	}
	if want := base.Add(2 * time.Second); !got[5].Time.Equal(want) {
		t.Errorf("newest = %v, want %v", got[5].Time, want)
	}
}

// The ordinary case must keep working — and keep its order stable — without
// the sort doing anything.
func TestRingSinceKeepsForwardOrderAndFiltersByTime(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := NewRing(10)
	for i := 0; i < 6; i++ {
		r.Push(collect.Snapshot{
			Time: base.Add(time.Duration(i) * time.Second),
			CPU:  collect.CPUStats{Total: float64(i)},
		})
	}

	got := r.Since(base.Add(3 * time.Second))
	if len(got) != 3 {
		t.Fatalf("got %d samples, want the 3 at or after the cutoff", len(got))
	}
	for i, want := range []float64{3, 4, 5} {
		if got[i].CPU.Total != want {
			t.Errorf("sample %d = %v, want %v", i, got[i].CPU.Total, want)
		}
	}
}

// A ring that has wrapped still reports oldest-first.
func TestRingSinceAfterWraparound(t *testing.T) {
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	r := NewRing(3)
	for i := 0; i < 7; i++ {
		r.Push(collect.Snapshot{
			Time: base.Add(time.Duration(i) * time.Second),
			CPU:  collect.CPUStats{Total: float64(i)},
		})
	}
	got := r.Since(time.Time{})
	if len(got) != 3 {
		t.Fatalf("got %d samples, want the last 3", len(got))
	}
	for i, want := range []float64{4, 5, 6} {
		if got[i].CPU.Total != want {
			t.Errorf("sample %d = %v, want %v", i, got[i].CPU.Total, want)
		}
	}
}

// time.Now() carries a monotonic reading, and Time.Before uses it whenever
// both operands have one. The monotonic clock never steps, so a sample taken
// after an NTP correction backwards still compares as "later" than the one
// before it — while the wall-clock timestamp served from it is earlier. The
// ring stores wall-clock stamps, so the monotonic part has to go on the way
// in, or every ordering decision is made on a different clock from the one
// the API reports.
func TestRingDropsTheMonotonicReading(t *testing.T) {
	r := NewRing(4)
	now := time.Now()
	if !strings.Contains(now.String(), "m=") {
		t.Skip("this platform's time.Now() carries no monotonic reading")
	}
	r.Push(collect.Snapshot{Time: now})

	got := r.Since(time.Time{})
	if len(got) != 1 {
		t.Fatalf("got %d samples, want 1", len(got))
	}
	if s := got[0].Time.String(); strings.Contains(s, "m=") {
		t.Errorf("the ring kept the monotonic reading: %s", s)
	}
	// The wall-clock instant itself must be untouched.
	if !got[0].Time.Equal(now) {
		t.Errorf("stored time %v, want %v", got[0].Time, now)
	}
}

// The ordering has to be decided on the wall clock the API serves, which is
// what makes a backward step visible at all.
func TestRingSinceOrdersByWallClockNotArrival(t *testing.T) {
	r := NewRing(10)
	now := time.Now()
	// Arrival order: later stamp first, as a backward step produces.
	r.Push(collect.Snapshot{Time: now, CPU: collect.CPUStats{Total: 2}})
	r.Push(collect.Snapshot{Time: now.Add(-3 * time.Minute), CPU: collect.CPUStats{Total: 1}})

	got := r.Since(time.Time{})
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2", len(got))
	}
	if got[0].CPU.Total != 1 || got[1].CPU.Total != 2 {
		t.Fatalf("want the earlier stamp first, got %v then %v",
			got[0].Time, got[1].Time)
	}
	if got[1].Time.UnixNano() < got[0].Time.UnixNano() {
		t.Error("UnixNano order disagrees with the returned order")
	}
}
