package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, keeps CGO_ENABLED=0

	"github.com/nexusriot/beastiemon/internal/collect"
)

// SQLite is an optional on-disk history store, parallel to the in-memory ring.
// Snapshots are rolled up into one row per resolution window and pruned past
// the retention window, so /api/series can answer ranges the ring can't hold
// and history survives daemon restarts.
//
// Each bucket stores three JSON snapshots — the element-wise average, min, and
// max — so wide ranges can show a min/max band instead of a single arbitrary
// sample (see rollup.go). JSON-encoding whole Snapshots mirrors the wire format,
// so the Snapshot struct can grow without a schema migration.
//
// Writes happen on a single background goroutine fed by a buffered channel, so
// Push never blocks the sampler's hot path (it drops if the writer is backed
// up — the ring still has the sample). The still-filling current bucket is not
// written until it rolls over (or Close flushes it); the ring covers that
// recent window in the meantime.
type SQLite struct {
	db         *sql.DB
	resolution time.Duration
	retention  time.Duration
	coarseAge  time.Duration // re-aggregate rows older than this (0 = off)
	coarseRes  time.Duration // coarse bucket width

	mu  sync.Mutex
	agg *bucketAgg // the bucket currently accumulating (nil until first sample)

	ch     chan collect.Snapshot
	done   chan struct{}
	closed chan struct{}
	ready  chan struct{} // closed once the writer's initial maintenance pass is done
}

// Options configures OpenSQLite. Zero values fall back to the documented
// defaults, except CoarseAfter where 0 disables the coarse tier.
type Options struct {
	Retention  time.Duration // prune rows older than this (default 30d)
	Resolution time.Duration // fine bucket width (default 1m)

	// CoarseAfter enables tiered downsampling: rows older than this are
	// re-aggregated into one row per CoarseResolution (default 1h), so long
	// retention costs hours-resolution rows instead of minutes-resolution
	// ones. 0 disables the tier.
	CoarseAfter      time.Duration
	CoarseResolution time.Duration
}

// OpenSQLite opens (creating parent dirs and the tables if needed) the history
// database at path and starts its background writer.
func OpenSQLite(path string, opts Options) (*SQLite, error) {
	if dir := filepath.Dir(path); dir != "" {
		os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection serialises all access, sidestepping SQLite lock
	// contention. Throughput here is ~1 write/minute, so this costs nothing.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS samples (
		ts   INTEGER PRIMARY KEY,
		data TEXT NOT NULL,
		dmin TEXT,
		dmax TEXT
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS alert_events (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		ts    INTEGER NOT NULL,
		rule  TEXT NOT NULL,
		state TEXT NOT NULL,
		data  TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	// Upgrade older two-column databases in place; the errors when the columns
	// already exist are expected and ignored.
	db.Exec(`ALTER TABLE samples ADD COLUMN dmin TEXT`)
	db.Exec(`ALTER TABLE samples ADD COLUMN dmax TEXT`)
	if opts.Retention <= 0 {
		opts.Retention = 30 * 24 * time.Hour
	}
	if opts.Resolution <= 0 {
		opts.Resolution = time.Minute
	}
	if opts.CoarseAfter > 0 && opts.CoarseResolution <= 0 {
		opts.CoarseResolution = time.Hour
	}
	if opts.CoarseResolution <= opts.Resolution {
		opts.CoarseAfter = 0 // nothing to gain; disable the tier
	}
	s := &SQLite{
		db:         db,
		resolution: opts.Resolution,
		retention:  opts.Retention,
		coarseAge:  opts.CoarseAfter,
		coarseRes:  opts.CoarseResolution,
		ch:         make(chan collect.Snapshot, 64),
		done:       make(chan struct{}),
		closed:     make(chan struct{}),
		ready:      make(chan struct{}),
	}
	go s.writer()
	return s, nil
}

// Push queues a snapshot for persistence. Non-blocking: drops on a full queue.
func (s *SQLite) Push(snap collect.Snapshot) {
	select {
	case s.ch <- snap:
	default:
	}
}

// Since returns persisted (per-bucket average) snapshots with Time >= t,
// oldest first.
func (s *SQLite) Since(t time.Time) []collect.Snapshot {
	rows, err := s.db.Query(`SELECT data FROM samples WHERE ts >= ? ORDER BY ts ASC`, t.Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []collect.Snapshot
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			continue
		}
		var snap collect.Snapshot
		if err := json.Unmarshal([]byte(data), &snap); err == nil {
			out = append(out, snap)
		}
	}
	return out
}

// RollupSince returns the per-bucket average, minimum, and maximum snapshot
// series with Time >= t, oldest first and index-aligned. Rows written before
// roll-ups existed (no envelope) fall back to the average for min and max.
func (s *SQLite) RollupSince(t time.Time) (avg, lo, hi []collect.Snapshot) {
	rows, err := s.db.Query(`SELECT data, dmin, dmax FROM samples WHERE ts >= ? ORDER BY ts ASC`, t.Unix())
	if err != nil {
		return nil, nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		var dmin, dmax sql.NullString
		if err := rows.Scan(&data, &dmin, &dmax); err != nil {
			continue
		}
		var a collect.Snapshot
		if err := json.Unmarshal([]byte(data), &a); err != nil {
			continue
		}
		avg = append(avg, a)
		lo = append(lo, unmarshalOr(dmin, a))
		hi = append(hi, unmarshalOr(dmax, a))
	}
	return avg, lo, hi
}

// unmarshalOr decodes a nullable envelope column, falling back to def when the
// column is NULL (pre-roll-up row) or unparseable.
func unmarshalOr(col sql.NullString, def collect.Snapshot) collect.Snapshot {
	if !col.Valid {
		return def
	}
	var snap collect.Snapshot
	if err := json.Unmarshal([]byte(col.String), &snap); err != nil {
		return def
	}
	return snap
}

// Close stops the writer (flushing the pending bucket first) and closes the
// database.
func (s *SQLite) Close() error {
	close(s.done)
	<-s.closed
	return s.db.Close()
}

func (s *SQLite) writer() {
	defer close(s.closed)
	s.maintain() // catch up immediately: prune + coarsen work left from before a restart
	close(s.ready)
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-s.done:
			// Drain anything still queued, then flush the open bucket.
			for {
				select {
				case snap := <-s.ch:
					s.ingest(snap)
				default:
					s.flush()
					return
				}
			}
		case snap := <-s.ch:
			s.ingest(snap)
		case <-prune.C:
			s.maintain()
		}
	}
}

// maintain runs the periodic housekeeping: retention pruning and, when the
// coarse tier is enabled, re-aggregating aged rows batch by batch until none
// remain.
func (s *SQLite) maintain() {
	s.prune()
	if s.coarseAge <= 0 {
		return
	}
	cutoff := time.Now().Add(-s.coarseAge)
	after := int64(-1 << 62)
	for {
		next, more := s.coarsen(cutoff, after)
		if !more {
			return
		}
		after = next
	}
}

// coarsenBatch bounds how many rows one coarsen pass loads into memory; a
// week of one-minute rows. maintain loops until the backlog is drained.
const coarsenBatch = 10080

// coarsen re-aggregates fine-resolution rows in (after, cutoff) into one row
// per coarse bucket, so long-tail history costs one row per hour instead of
// sixty. Buckets already holding a single row are left untouched — steady
// state does no work. The min/max envelope merges exactly; the stored average
// becomes a mean of bucket means, which is negligibly skewed because fine
// buckets hold near-equal sample counts. Returns the last timestamp examined
// and whether another batch may remain.
func (s *SQLite) coarsen(cutoff time.Time, after int64) (last int64, more bool) {
	type row struct {
		ts          int64
		avg, lo, hi collect.Snapshot
	}
	rows, err := s.db.Query(
		`SELECT ts, data, dmin, dmax FROM samples WHERE ts > ? AND ts < ? ORDER BY ts ASC LIMIT ?`,
		after, cutoff.Unix(), coarsenBatch)
	if err != nil {
		return 0, false
	}
	// Read the whole batch before writing: the single-connection pool means a
	// write while this result set is open would deadlock.
	var batch []row
	for rows.Next() {
		var r row
		var data string
		var dmin, dmax sql.NullString
		if err := rows.Scan(&r.ts, &data, &dmin, &dmax); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(data), &r.avg); err != nil {
			continue
		}
		r.lo = unmarshalOr(dmin, r.avg)
		r.hi = unmarshalOr(dmax, r.avg)
		batch = append(batch, r)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0, false
	}

	full := len(batch) == coarsenBatch
	bucketOf := func(ts int64) time.Time { return time.Unix(ts, 0).Truncate(s.coarseRes) }
	if full {
		// The trailing bucket may be cut mid-group; push it to the next batch
		// unless that would empty this one.
		lastBucket := bucketOf(batch[len(batch)-1].ts)
		trim := len(batch)
		for trim > 0 && bucketOf(batch[trim-1].ts).Equal(lastBucket) {
			trim--
		}
		if trim > 0 {
			batch = batch[:trim]
		}
	}
	last = batch[len(batch)-1].ts

	var group []row
	flush := func() {
		if len(group) < 2 {
			group = group[:0]
			return
		}
		bucket := bucketOf(group[0].ts)
		aggA, aggL, aggH := newBucketAgg(bucket), newBucketAgg(bucket), newBucketAgg(bucket)
		for _, r := range group {
			aggA.add(r.avg)
			aggL.add(r.lo)
			aggH.add(r.hi)
		}
		avg := aggA.build(func(st stat) float64 { return st.avg() })
		lo := aggL.build(func(st stat) float64 { return st.min })
		hi := aggH.build(func(st stat) float64 { return st.max })
		ad, err := json.Marshal(avg)
		if err != nil {
			group = group[:0]
			return
		}
		ld, _ := json.Marshal(lo)
		hd, _ := json.Marshal(hi)
		tx, err := s.db.Begin()
		if err != nil {
			group = group[:0]
			return
		}
		tx.Exec(`DELETE FROM samples WHERE ts >= ? AND ts <= ?`, group[0].ts, group[len(group)-1].ts)
		tx.Exec(`INSERT OR REPLACE INTO samples(ts, data, dmin, dmax) VALUES(?, ?, ?, ?)`,
			bucket.Unix(), string(ad), string(ld), string(hd))
		tx.Commit()
		group = group[:0]
	}
	for _, r := range batch {
		if len(group) > 0 && !bucketOf(group[0].ts).Equal(bucketOf(r.ts)) {
			flush()
		}
		group = append(group, r)
	}
	flush()
	return last, full
}

// ingest folds one snapshot into the current bucket, flushing the previous
// bucket to disk when the snapshot crosses into a new resolution window. Safe
// to call directly in tests.
func (s *SQLite) ingest(snap collect.Snapshot) {
	bucket := snap.Time.Truncate(s.resolution)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agg != nil && !bucket.Equal(s.agg.bucket) {
		s.writeRow(s.agg)
		s.agg = nil
	}
	if s.agg == nil {
		s.agg = newBucketAgg(bucket)
	}
	s.agg.add(snap)
}

// flush writes any pending bucket. Safe to call directly in tests.
func (s *SQLite) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agg != nil {
		s.writeRow(s.agg)
		s.agg = nil
	}
}

// writeRow persists one finalized bucket. Caller holds s.mu.
func (s *SQLite) writeRow(a *bucketAgg) {
	avg, lo, hi := a.finalize()
	ad, err := json.Marshal(avg)
	if err != nil {
		return
	}
	ld, _ := json.Marshal(lo)
	hd, _ := json.Marshal(hi)
	s.db.Exec(`INSERT OR REPLACE INTO samples(ts, data, dmin, dmax) VALUES(?, ?, ?, ?)`,
		avg.Time.Unix(), string(ad), string(ld), string(hd))
}

// prune deletes rows older than the retention window; alert events age out on
// the same schedule.
func (s *SQLite) prune() {
	cutoff := time.Now().Add(-s.retention).Unix()
	s.db.Exec(`DELETE FROM samples WHERE ts < ?`, cutoff)
	s.db.Exec(`DELETE FROM alert_events WHERE ts < ?`, cutoff)
}

// SaveAlertEvent persists one alert event as raw JSON. Events are rare, so
// this writes synchronously; the single-connection pool serialises it against
// the sampler writer.
func (s *SQLite) SaveAlertEvent(ts time.Time, rule, state string, data []byte) {
	s.db.Exec(`INSERT INTO alert_events(ts, rule, state, data) VALUES(?, ?, ?, ?)`,
		ts.Unix(), rule, state, string(data))
}

// AlertEvents returns up to limit persisted alert events as raw JSON, newest
// first.
func (s *SQLite) AlertEvents(limit int) [][]byte {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT data FROM alert_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err == nil {
			out = append(out, []byte(data))
		}
	}
	return out
}
