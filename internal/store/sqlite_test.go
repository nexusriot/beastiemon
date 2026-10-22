package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// A coarse bucket holding more fine rows than one coarsen batch used to be
// folded piecewise: each batch wrote a partial aggregate at the bucket
// timestamp and the following INSERT OR REPLACE discarded it, so the surviving
// row covered only the final slice. The bucket is now left at fine resolution
// instead, and maintain still makes forward progress past it.
func TestCoarsenSkipsBucketWiderThanOneBatch(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{
		Retention:        100 * 24 * time.Hour,
		Resolution:       time.Second,
		CoarseAfter:      24 * time.Hour,
		CoarseResolution: time.Hour,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	<-db.ready
	db.batchSize = 4 // a bucket of 6 rows now exceeds one batch

	// Six one-second rows inside a single hourly bucket, then two rows in the
	// next hour that must still be reachable after the oversized one.
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 6; i++ {
		db.ingest(snapAt(base.Add(time.Duration(i)*time.Second), float64(10*i)))
	}
	next := base.Add(time.Hour)
	db.ingest(snapAt(next, 1))
	db.ingest(snapAt(next.Add(time.Second), 3))
	db.flush()
	if n := len(db.Since(base.Add(-time.Hour))); n != 8 {
		t.Fatalf("precondition: want 8 fine rows, got %d", n)
	}

	db.maintain() // must terminate, not loop on the oversized bucket

	got := db.Since(base.Add(-time.Hour))
	// 6 untouched fine rows + the following bucket merged into 1.
	if len(got) != 7 {
		t.Fatalf("want 6 untouched rows + 1 merged bucket, got %d", len(got))
	}
	for i := 0; i < 6; i++ {
		if got[i].CPU.Total != float64(10*i) {
			t.Fatalf("row %d was rewritten: got %v, want %v", i, got[i].CPU.Total, 10*i)
		}
	}
	// The bucket after the oversized one was still reached and coarsened.
	if !got[6].Time.Equal(next) || got[6].CPU.Total != 2 {
		t.Fatalf("following bucket: want ts=%v avg=2, got ts=%v avg=%v",
			next, got[6].Time, got[6].CPU.Total)
	}

	// Idempotent: a second pass changes nothing.
	db.maintain()
	if n := len(db.Since(base.Add(-time.Hour))); n != 7 {
		t.Fatalf("second maintain changed row count: %d", n)
	}
}

// The rollup has to carry the fields added alongside throughput: the error
// and drop rates average like any other rate, while pf's enabled flag and
// hard limit are configuration that must survive rather than be averaged.
func TestRollupCarriesNewFields(t *testing.T) {
	db := openTestDB(t, time.Hour, time.Minute)
	base := time.Now().Truncate(time.Minute)

	mk := func(off time.Duration, rxErr, drop float64, states uint64) collect.Snapshot {
		s := snapAt(base.Add(off), 10)
		s.Net = []collect.NetStats{{Interface: "em0", RxBps: 100, RxErr: rxErr}}
		s.PF = &collect.PFStats{Enabled: true, States: states, StateLimit: 10000, DropRate: drop}
		s.Watch = []collect.ProcWatch{{Name: "sshd", Count: 1}}
		s.ZFS = []collect.ZFSStats{{Pool: "zroot", UsedPct: 50, Health: "DEGRADED",
			Scan: "scrub_done", ScanAgeHours: 12, ScanKnown: true}}
		return s
	}
	db.ingest(mk(0, 2, 0, 100))
	db.ingest(mk(10*time.Second, 4, 6, 300))
	db.flush()

	avg, lo, hi := db.RollupSince(base.Add(-time.Hour))
	if len(avg) != 1 {
		t.Fatalf("want one bucket, got %d", len(avg))
	}
	if avg[0].Net[0].RxErr != 3 || lo[0].Net[0].RxErr != 2 || hi[0].Net[0].RxErr != 4 {
		t.Errorf("net error envelope: avg=%v min=%v max=%v",
			avg[0].Net[0].RxErr, lo[0].Net[0].RxErr, hi[0].Net[0].RxErr)
	}
	if avg[0].PF == nil || avg[0].PF.States != 200 || avg[0].PF.DropRate != 3 {
		t.Errorf("pf rollup: %+v", avg[0].PF)
	}
	// Not measurements, so they are carried rather than averaged.
	if !avg[0].PF.Enabled || avg[0].PF.StateLimit != 10000 {
		t.Errorf("pf config fields should survive the rollup: %+v", avg[0].PF)
	}
	if hi[0].PF.States != 300 {
		t.Errorf("pf max states = %d, want 300", hi[0].PF.States)
	}
	// Strings and flags come from the last sample in the bucket.
	if avg[0].ZFS[0].Health != "DEGRADED" || avg[0].ZFS[0].Scan != "scrub_done" ||
		!avg[0].ZFS[0].ScanKnown || avg[0].ZFS[0].ScanAgeHours != 12 {
		t.Errorf("zfs scan state lost in rollup: %+v", avg[0].ZFS[0])
	}
	if len(avg[0].Watch) != 1 || avg[0].Watch[0].Name != "sshd" {
		t.Errorf("watch list lost in rollup: %+v", avg[0].Watch)
	}
}

// SQLite reports a history directory it cannot write as "unable to open
// database file: out of memory (14)", which sends an operator hunting a memory
// leak that isn't there. It is also the likeliest store misconfiguration,
// since beastied runs as _beastie while /var/db belongs to root.
func TestOpenSQLiteExplainsUnusableDirectory(t *testing.T) {
	// A regular file where a directory should be: MkdirAll fails with ENOTDIR
	// for every user, root included, so this half runs everywhere.
	t.Run("parent is a file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := OpenSQLite(filepath.Join(blocker, "history.db"), Options{})
		if err == nil {
			t.Fatal("opening under a regular file should fail")
		}
		if !strings.Contains(err.Error(), "history directory") {
			t.Errorf("error should name the directory as the problem, got: %v", err)
		}
	})

	t.Run("directory not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		_, err := OpenSQLite(filepath.Join(dir, "history.db"), Options{})
		if err == nil {
			t.Fatal("opening in an unwritable directory should fail")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("error should say the directory is not writable, got: %v", err)
		}
	})

	// A usable directory still opens, so the probe never blocks the good path.
	t.Run("writable directory still opens", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{})
		if err != nil {
			t.Fatalf("a writable directory should open: %v", err)
		}
		s.Close()
	})
}

// A store that cannot write is invisible from everywhere else: the daemon
// still logs "store on", /api/series still answers from the ring, and the
// rows simply stop arriving. A full filesystem does this on every sample, so
// the reporting has to be rate-limited — a line per second would bury the log
// and keep writing to the filesystem that just filled up.
func TestStoreReportsPersistenceFailures(t *testing.T) {
	t.Run("a failed write is reported", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{
			Resolution: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		// Wait for the writer's opening maintenance pass, so the only
		// failures that follow come from the sample writes themselves.
		<-s.ready

		type report struct {
			op  string
			err error
		}
		got := make(chan report, 32)
		s.SetErrorFunc(func(op string, err error) { got <- report{op, err} })

		// Closing the handle makes every statement fail the way a full disk
		// or a revoked permission does, without needing either.
		s.db.Close()

		base := time.Now().Truncate(time.Second)
		s.Push(snapAt(base, 10))
		s.Push(snapAt(base.Add(time.Second), 20))

		// Look for the row write specifically: pruning also mentions history,
		// and a report from it would not prove the sample path is covered.
		const want = "writing a history row"
		deadline := time.After(5 * time.Second)
		for {
			select {
			case r := <-got:
				if r.op != want {
					continue
				}
				if r.err == nil {
					t.Errorf("a failed write should report an error, got %+v", r)
				}
				return
			case <-deadline:
				t.Fatalf("a failed sample write was never reported as %q", want)
			}
		}
	})

	t.Run("repeats are rate-limited and recovery is reported once", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		var mu sync.Mutex
		var fails, recoveries int
		s.SetErrorFunc(func(_ string, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				recoveries++
			} else {
				fails++
			}
		})

		boom := errors.New("disk full")
		for i := 0; i < 50; i++ {
			s.note("writing a history row", boom)
		}
		mu.Lock()
		gotFails := fails
		mu.Unlock()
		if gotFails != 1 {
			t.Errorf("50 consecutive failures produced %d reports, want 1", gotFails)
		}

		// Still failing, but long enough ago to be worth saying again.
		s.errMu.Lock()
		s.lastReport = time.Now().Add(-2 * errorReportEvery)
		s.errMu.Unlock()
		s.note("writing a history row", boom)
		mu.Lock()
		gotFails = fails
		mu.Unlock()
		if gotFails != 2 {
			t.Errorf("a failure past the interval produced %d reports, want 2", gotFails)
		}

		// Writes working again is worth exactly one line, not one per sample.
		for i := 0; i < 10; i++ {
			s.note("writing a history row", nil)
		}
		mu.Lock()
		gotRecoveries := recoveries
		mu.Unlock()
		if gotRecoveries != 1 {
			t.Errorf("recovery produced %d reports, want 1", gotRecoveries)
		}
	})

	t.Run("a healthy store stays quiet", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "history.db"), Options{
			Resolution: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		var mu sync.Mutex
		var reports int
		s.SetErrorFunc(func(string, error) { mu.Lock(); reports++; mu.Unlock() })

		base := time.Now().Truncate(time.Second)
		for i := 0; i < 5; i++ {
			s.Push(snapAt(base.Add(time.Duration(i)*time.Second), float64(i)))
		}
		s.Since(base.Add(-time.Hour))
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		if reports != 0 {
			t.Errorf("a working store reported %d problems", reports)
		}
	})
}
