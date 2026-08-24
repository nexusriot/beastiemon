package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nexusriot/beastiemon/internal/collect"
)

func snapAt(t time.Time, cpu float64) collect.Snapshot {
	return collect.Snapshot{Time: t, CPU: collect.CPUStats{Total: cpu}}
}

func openTestDB(t *testing.T, retention, resolution time.Duration) *SQLite {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"),
		Options{Retention: retention, Resolution: resolution})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRollupPersistAndSince(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Minute)
	base := time.Now().Truncate(time.Minute)

	// Three samples in three distinct buckets → three rows, oldest first.
	db.ingest(snapAt(base, 1))
	db.ingest(snapAt(base.Add(2*time.Minute), 2))
	db.ingest(snapAt(base.Add(4*time.Minute), 3))
	db.flush() // the last (open) bucket only lands on flush

	got := db.Since(base.Add(-time.Hour))
	if len(got) != 3 {
		t.Fatalf("want 3 rows, got %d", len(got))
	}
	if got[0].CPU.Total != 1 || got[2].CPU.Total != 3 {
		t.Fatalf("ordering/content wrong: %v", got)
	}

	// Since excludes rows older than the cutoff.
	if n := len(db.Since(base.Add(3 * time.Minute))); n != 1 {
		t.Fatalf("want 1 row after cutoff, got %d", n)
	}
}

func TestRollupCollapsesBucket(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Minute)
	base := time.Now().Truncate(time.Minute)

	// Several samples inside one resolution window collapse to a single row
	// whose average/min/max reflect the whole bucket.
	db.ingest(snapAt(base, 10))
	db.ingest(snapAt(base.Add(10*time.Second), 30))
	db.ingest(snapAt(base.Add(20*time.Second), 50))
	db.flush()

	avg, lo, hi := db.RollupSince(base.Add(-time.Hour))
	if len(avg) != 1 || len(lo) != 1 || len(hi) != 1 {
		t.Fatalf("want a single rolled-up bucket, got %d rows", len(avg))
	}
	if avg[0].CPU.Total != 30 {
		t.Fatalf("avg CPU: want 30, got %v", avg[0].CPU.Total)
	}
	if lo[0].CPU.Total != 10 || hi[0].CPU.Total != 50 {
		t.Fatalf("min/max envelope wrong: min=%v max=%v", lo[0].CPU.Total, hi[0].CPU.Total)
	}
	// The averaged row is timestamped at the bucket boundary.
	if !avg[0].Time.Equal(base) {
		t.Fatalf("bucket ts: want %v, got %v", base, avg[0].Time)
	}
	// Since returns the average series.
	if got := db.Since(base.Add(-time.Hour)); len(got) != 1 || got[0].CPU.Total != 30 {
		t.Fatalf("Since should return the average row, got %v", got)
	}
}

func TestRollupPrune(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Minute)
	now := time.Now().Truncate(time.Minute)

	db.ingest(snapAt(now.Add(-2*time.Hour), 1)) // older than retention
	db.ingest(snapAt(now, 2))                   // rolls over the first bucket
	db.flush()

	db.prune()

	got := db.Since(now.Add(-24 * time.Hour))
	if len(got) != 1 || got[0].CPU.Total != 2 {
		t.Fatalf("prune should leave only the recent row, got %v", got)
	}
}

func TestPushAsyncRollover(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Nanosecond) // tiny resolution: every sample is its own bucket
	base := time.Now().Truncate(time.Second)

	for i := 0; i < 5; i++ {
		db.Push(snapAt(base.Add(time.Duration(i)*time.Second), float64(i)))
	}

	// The writer is async and flushes each bucket when the next one starts, so
	// the first four land here; the fifth stays open until Close. Poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(db.Since(base.Add(-time.Hour))) == 4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("async writes did not land: got %d rows", len(db.Since(base.Add(-time.Hour))))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCoarsenMergesAgedBuckets(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{
		Retention:        100 * 24 * time.Hour,
		Resolution:       time.Minute,
		CoarseAfter:      24 * time.Hour,
		CoarseResolution: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Direct ingest bypasses the writer goroutine; wait out its startup
	// maintenance pass so it can't coarsen our half-written buckets.
	<-db.ready

	// Six one-minute rows, two days old: three in coarse bucket A, two in
	// bucket B, one alone in bucket C.
	base := time.Now().Add(-48 * time.Hour).Truncate(5 * time.Minute)
	for _, s := range []struct {
		off time.Duration
		cpu float64
	}{
		{0, 10}, {1 * time.Minute, 30}, {2 * time.Minute, 50}, // bucket A
		{5 * time.Minute, 20}, {6 * time.Minute, 40}, // bucket B
		{10 * time.Minute, 5}, // bucket C (single row: left alone)
	} {
		db.ingest(snapAt(base.Add(s.off), s.cpu))
	}
	db.flush()
	if n := len(db.Since(base.Add(-time.Hour))); n != 6 {
		t.Fatalf("precondition: want 6 fine rows, got %d", n)
	}

	db.maintain()

	avg, lo, hi := db.RollupSince(base.Add(-time.Hour))
	if len(avg) != 3 {
		t.Fatalf("want 3 rows after coarsening (A, B, C), got %d", len(avg))
	}
	if !avg[0].Time.Equal(base) {
		t.Fatalf("bucket A ts: want %v, got %v", base, avg[0].Time)
	}
	if avg[0].CPU.Total != 30 || lo[0].CPU.Total != 10 || hi[0].CPU.Total != 50 {
		t.Fatalf("bucket A: want avg=30 min=10 max=50, got %v/%v/%v",
			avg[0].CPU.Total, lo[0].CPU.Total, hi[0].CPU.Total)
	}
	if avg[1].CPU.Total != 30 || lo[1].CPU.Total != 20 || hi[1].CPU.Total != 40 {
		t.Fatalf("bucket B: want avg=30 min=20 max=40, got %v/%v/%v",
			avg[1].CPU.Total, lo[1].CPU.Total, hi[1].CPU.Total)
	}
	if avg[2].CPU.Total != 5 {
		t.Fatalf("bucket C should be untouched, got %v", avg[2].CPU.Total)
	}

	// Idempotent: a second pass finds only single-row buckets and no-ops.
	db.maintain()
	if got := db.Since(base.Add(-time.Hour)); len(got) != 3 {
		t.Fatalf("second maintain changed row count: %d", len(got))
	}
}

func TestCoarsenLeavesRecentRows(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{
		Retention:        100 * 24 * time.Hour,
		Resolution:       time.Minute,
		CoarseAfter:      24 * time.Hour,
		CoarseResolution: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	<-db.ready

	base := time.Now().Add(-10 * time.Minute).Truncate(5 * time.Minute)
	db.ingest(snapAt(base, 1))
	db.ingest(snapAt(base.Add(time.Minute), 2))
	db.flush()

	db.maintain()
	if n := len(db.Since(base.Add(-time.Hour))); n != 2 {
		t.Fatalf("recent rows must stay at fine resolution, got %d", n)
	}
}

func TestAlertEventsRoundTrip(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Minute)
	now := time.Now()
	db.SaveAlertEvent(now.Add(-2*time.Second), "cpu-hot", "firing", []byte(`{"rule":"cpu-hot","state":"firing"}`))
	db.SaveAlertEvent(now.Add(-1*time.Second), "cpu-hot", "resolved", []byte(`{"rule":"cpu-hot","state":"resolved"}`))
	db.SaveAlertEvent(now, "watchdog", "firing", []byte(`{"rule":"watchdog","state":"firing"}`))

	evs := db.AlertEvents(2)
	if len(evs) != 2 {
		t.Fatalf("want 2 events (limit), got %d", len(evs))
	}
	// Newest first.
	if string(evs[0]) != `{"rule":"watchdog","state":"firing"}` {
		t.Fatalf("order: newest first expected, got %s", evs[0])
	}
	if got := db.AlertEvents(0); len(got) != 3 {
		t.Fatalf("default limit should return all 3, got %d", len(got))
	}
}
