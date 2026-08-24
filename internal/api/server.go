package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	psutil_host "github.com/shirou/gopsutil/v3/host"

	"github.com/nexusriot/beastiemon/internal/alert"
	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/config"
	"github.com/nexusriot/beastiemon/internal/store"
)

// wsUpgrader accepts cross-origin connections: the payload is read-only
// metrics, matching the SSE endpoint's permissive CORS posture. Front with a
// proxy + auth for anything untrusted (see DESIGN §15).
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Broker fans out snapshots to SSE clients.
type Broker struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	done    chan struct{} // closed by Close; streaming handlers return on it
}

func (b *Broker) Subscribe() chan []byte {
	ch := make(chan []byte, 8)
	b.mu.Lock()
	if b.clients == nil {
		b.clients = make(map[chan []byte]struct{})
	}
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *Broker) Publish(data []byte) {
	b.mu.Lock()
	for ch := range b.clients {
		select {
		case ch <- data:
		default:
		}
	}
	b.mu.Unlock()
}

// HistoryStore is an optional long-term store (SQLite) that the server writes
// each snapshot to and can read older-than-ring ranges from. nil when
// persistence is disabled.
type HistoryStore interface {
	Push(collect.Snapshot)
	Since(time.Time) []collect.Snapshot
}

// RollupStore is an optional extension of HistoryStore that also exposes the
// per-bucket min/max envelope, letting /api/series?band=1 draw a band around
// the average. A store may implement only HistoryStore, in which case band
// mode falls back to a flat (min=max=avg) envelope.
type RollupStore interface {
	RollupSince(time.Time) (avg, lo, hi []collect.Snapshot)
}

// AlertSource exposes the alert engine's live rule states and in-memory event
// history to /api/alerts. nil when no alerts are configured.
type AlertSource interface {
	States() []alert.RuleStatus
	Events(limit int) []alert.Event
}

// AlertEventStore is an optional HistoryStore extension serving persisted
// alert events (raw Event JSON, newest first); when present, /api/alerts
// prefers it over the engine's in-memory history because it survives restarts.
type AlertEventStore interface {
	AlertEvents(limit int) [][]byte
}

// Server wires together the ring store, broker, and HTTP mux.
type Server struct {
	ring   *store.Ring
	broker *Broker
	mux    *http.ServeMux
	store  HistoryStore

	// mu guards the fields SIGHUP reload swaps while handlers read them.
	mu     sync.RWMutex
	auth   config.AuthConfig
	alerts AlertSource
}

func New(ring *store.Ring, webFS fs.FS, auth config.AuthConfig) *Server {
	s := &Server{
		ring:   ring,
		broker: &Broker{done: make(chan struct{})},
		mux:    http.NewServeMux(),
		auth:   auth,
	}
	s.routes(webFS)
	return s
}

// SetStore attaches an optional history store. Call before serving.
func (s *Server) SetStore(h HistoryStore) { s.store = h }

// Close makes the streaming handlers (SSE, WebSocket) return. Call it before
// http.Server.Shutdown: those connections are never idle, so without this
// Shutdown always waits out its full timeout while a dashboard is open.
func (s *Server) Close() { close(s.broker.done) }

// SetAuth swaps the auth config; used by SIGHUP config reload.
func (s *Server) SetAuth(a config.AuthConfig) {
	s.mu.Lock()
	s.auth = a
	s.mu.Unlock()
}

// SetAlerts attaches (or, with nil, detaches) the alert engine serving
// /api/alerts; used at startup and by SIGHUP config reload.
func (s *Server) SetAlerts(a AlertSource) {
	s.mu.Lock()
	s.alerts = a
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	auth := s.auth
	s.mu.RUnlock()
	// /healthz stays open so liveness probes work without credentials.
	if auth.Enabled() && r.URL.Path != "/healthz" && !authorized(r, auth) {
		if auth.BasicEnabled() {
			w.Header().Set("WWW-Authenticate", `Basic realm="BeastieMon"`)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized reports whether the request carries a valid credential.
// Both checks use constant-time comparison to avoid leaking secrets via
// response timing.
func authorized(r *http.Request, auth config.AuthConfig) bool {
	if auth.Token != "" {
		tok := bearerToken(r)
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(auth.Token)) == 1 {
			return true
		}
	}
	if auth.BasicEnabled() {
		u, p, ok := r.BasicAuth()
		if ok &&
			subtle.ConstantTimeCompare([]byte(u), []byte(auth.Username)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(auth.Password)) == 1 {
			return true
		}
	}
	return false
}

func bearerToken(r *http.Request) string {
	const pfx = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx) {
		return h[len(pfx):]
	}
	return ""
}

// Ingest receives snapshots from the sampler. The broker carries raw JSON;
// each transport (SSE, WebSocket) frames it as needed.
func (s *Server) Ingest(snap collect.Snapshot) {
	s.ring.Push(snap)
	if s.store != nil {
		s.store.Push(snap)
	}
	b, _ := json.Marshal(snap)
	s.broker.Publish(b)
}

func (s *Server) routes(webFS fs.FS) {
	s.mux.Handle("/", http.FileServer(http.FS(webFS)))
	s.mux.HandleFunc("/api/host", s.handleHost)
	s.mux.HandleFunc("/api/metrics", s.handleMetrics)
	s.mux.HandleFunc("/api/series", s.handleSeries)
	s.mux.HandleFunc("/api/alerts", s.handleAlerts)
	s.mux.HandleFunc("/api/stream", s.handleStream)
	s.mux.HandleFunc("/api/ws", s.handleWS)
	s.mux.HandleFunc("/metrics", s.handleMetricsProm)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	info, _ := psutil_host.Info()
	writeJSON(w, info)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.ring.Last()
	if !ok {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, snap)
}

type alertsResp struct {
	Enabled bool               `json:"enabled"`
	Rules   []alert.RuleStatus `json:"rules"`
	Events  []alert.Event      `json:"events"`
}

// handleAlerts reports the alert engine's current rule states and recent
// events, so alert activity is visible in the dashboard and API rather than
// only at the webhook receiver. Events come from the SQLite store when
// persistence is on (they survive restarts), else from the engine's
// in-memory history. ?limit= caps the event count (default 50, max 500).
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if limit > 500 {
		limit = 500
	}

	s.mu.RLock()
	src := s.alerts
	s.mu.RUnlock()

	resp := alertsResp{Rules: []alert.RuleStatus{}, Events: []alert.Event{}}
	if src != nil {
		resp.Enabled = true
		resp.Rules = src.States()
		resp.Events = src.Events(limit)
	}
	if es, ok := s.store.(AlertEventStore); ok {
		resp.Events = resp.Events[:0]
		for _, raw := range es.AlertEvents(limit) {
			var ev alert.Event
			if err := json.Unmarshal(raw, &ev); err == nil {
				resp.Events = append(resp.Events, ev)
			}
		}
	}
	writeJSON(w, resp)
}

type seriesResp struct {
	Labels []string    `json:"labels"`
	Data   [][]float64 `json:"data"`
}

// handleSeries returns a time series in uPlot format.
// Query params: metric (cpu|mem|load|net|disk|temp|fs|proc|zfs|arc|jail),
// range (e.g. 15m, 1h), iface= (net), dev= (disk), mount= (fs), pid= (proc),
// pool= (zfs), jid= (jail),
// band=1 (append <label>_min/<label>_max envelope columns per series).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := q.Get("metric")
	if metric == "" {
		metric = "cpu"
	}

	rangeDur := parseDuration(q.Get("range"), 15*time.Minute)
	since := time.Now().Add(-rangeDur)

	if band := q.Get("band"); band == "1" || band == "true" {
		s.writeBandedSeries(w, metric, since, q)
		return
	}

	snaps := s.snapsSince(since)
	labels, cols, ok := seriesColumns(metric, snaps, q)
	if !ok {
		http.Error(w, "unknown metric", http.StatusBadRequest)
		return
	}
	resp := seriesResp{
		Labels: append([]string{"ts"}, labels...),
		Data:   append([][]float64{timestamps(snaps)}, cols...),
	}
	writeJSON(w, resp)
}

// writeBandedSeries emits the average series plus, for each data column, a
// paired <label>_min / <label>_max column drawn from the per-bucket envelope.
func (s *Server) writeBandedSeries(w http.ResponseWriter, metric string, since time.Time, q url.Values) {
	avg, lo, hi := s.snapsBanded(since)
	labels, avgCols, ok := seriesColumns(metric, avg, q)
	if !ok {
		http.Error(w, "unknown metric", http.StatusBadRequest)
		return
	}
	_, loCols, _ := seriesColumns(metric, lo, q)
	_, hiCols, _ := seriesColumns(metric, hi, q)

	resp := seriesResp{
		Labels: []string{"ts"},
		Data:   [][]float64{timestamps(avg)},
	}
	for i, label := range labels {
		resp.Labels = append(resp.Labels, label, label+"_min", label+"_max")
		resp.Data = append(resp.Data, avgCols[i], colOr(loCols, i, avgCols[i]), colOr(hiCols, i, avgCols[i]))
	}
	writeJSON(w, resp)
}

// colOr returns cols[i], or def when the index is absent (defensive: the
// envelope series always align with the average, but never index out of range).
func colOr(cols [][]float64, i int, def []float64) []float64 {
	if i < len(cols) {
		return cols[i]
	}
	return def
}

// seriesColumns builds the non-timestamp label/column pairs for one metric over
// snaps. The caller prepends the shared "ts" column. ok is false for an unknown
// metric.
func seriesColumns(metric string, snaps []collect.Snapshot, q url.Values) (labels []string, cols [][]float64, ok bool) {
	switch metric {
	case "cpu":
		labels = []string{"user", "sys", "idle", "total"}
		user := make([]float64, len(snaps))
		sys := make([]float64, len(snaps))
		idle := make([]float64, len(snaps))
		total := make([]float64, len(snaps))
		for i, s := range snaps {
			user[i] = s.CPU.User
			sys[i] = s.CPU.Sys
			idle[i] = s.CPU.Idle
			total[i] = s.CPU.Total
		}
		cols = [][]float64{user, sys, idle, total}

		if len(snaps) > 0 && len(snaps[0].CPU.PerCore) > 0 {
			ncores := len(snaps[0].CPU.PerCore)
			for c := 0; c < ncores; c++ {
				labels = append(labels, fmt.Sprintf("cpu%d", c))
				core := make([]float64, len(snaps))
				for i, snap := range snaps {
					if c < len(snap.CPU.PerCore) {
						core[i] = snap.CPU.PerCore[c]
					}
				}
				cols = append(cols, core)
			}
		}
		return labels, cols, true

	case "mem":
		labels = []string{"used", "free", "swap_used"}
		used := make([]float64, len(snaps))
		free := make([]float64, len(snaps))
		swap := make([]float64, len(snaps))
		for i, s := range snaps {
			used[i] = float64(s.Mem.Used)
			free[i] = float64(s.Mem.Free)
			swap[i] = float64(s.Mem.SwapUsed)
		}
		return labels, [][]float64{used, free, swap}, true

	case "load":
		labels = []string{"load1", "load5", "load15"}
		l1 := make([]float64, len(snaps))
		l5 := make([]float64, len(snaps))
		l15 := make([]float64, len(snaps))
		for i, s := range snaps {
			l1[i] = s.Load.Load1
			l5[i] = s.Load.Load5
			l15[i] = s.Load.Load15
		}
		return labels, [][]float64{l1, l5, l15}, true

	case "net":
		iface := q.Get("iface")
		rx := make([]float64, len(snaps))
		tx := make([]float64, len(snaps))
		for i, snap := range snaps {
			for _, n := range snap.Net {
				if iface == "" || n.Interface == iface {
					rx[i] += n.RxBps
					tx[i] += n.TxBps
				}
			}
		}
		return []string{"rx_bps", "tx_bps"}, [][]float64{rx, tx}, true

	case "disk":
		dev := q.Get("dev")
		rd := make([]float64, len(snaps))
		wr := make([]float64, len(snaps))
		for i, snap := range snaps {
			for _, d := range snap.Disk {
				if dev == "" || d.Device == dev {
					rd[i] += d.ReadBps
					wr[i] += d.WriteBps
				}
			}
		}
		return []string{"read_bps", "write_bps"}, [][]float64{rd, wr}, true

	case "temp":
		// One series per sensor name found in any snapshot.
		for _, name := range collectTempNames(snaps) {
			labels = append(labels, name)
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, t := range snap.Temps {
					if t.Name == name {
						series[i] = t.Celsius
					}
				}
			}
			cols = append(cols, series)
		}
		return labels, cols, true

	case "fs":
		mount := q.Get("mount")
		// One used_pct series per mount (filtered to `mount` if given).
		for _, m := range collectFSMounts(snaps, mount) {
			labels = append(labels, m)
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, f := range snap.FS {
					if f.Mount == m {
						series[i] = f.UsedPct
					}
				}
			}
			cols = append(cols, series)
		}
		return labels, cols, true

	case "proc":
		pid := q.Get("pid")
		// Columns are the PIDs in the most recent snapshot (the current
		// top-N), or just `pid` if given; each series is that PID's cpu_pct
		// over the range, 0 where the process was absent that tick.
		for _, p := range latestProcs(snaps, pid) {
			labels = append(labels, fmt.Sprintf("%d %s", p.PID, p.Name))
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, pp := range snap.Procs {
					if pp.PID == p.PID {
						series[i] = pp.CPUPct
					}
				}
			}
			cols = append(cols, series)
		}
		return labels, cols, true

	case "zfs":
		pool := q.Get("pool")
		// One used_pct series per pool (filtered to `pool` if given).
		for _, p := range collectZFSPools(snaps, pool) {
			labels = append(labels, p)
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, z := range snap.ZFS {
					if z.Pool == p {
						series[i] = z.UsedPct
					}
				}
			}
			cols = append(cols, series)
		}
		return labels, cols, true

	case "arc":
		// size/target are bytes, hit_rate is a percentage — consumers chart
		// them on separate axes or pick columns.
		labels = []string{"size", "target", "hit_rate"}
		size := make([]float64, len(snaps))
		target := make([]float64, len(snaps))
		hit := make([]float64, len(snaps))
		for i, s := range snaps {
			if s.ARC != nil {
				size[i] = float64(s.ARC.Size)
				target[i] = float64(s.ARC.Target)
				hit[i] = s.ARC.HitRate
			}
		}
		return labels, [][]float64{size, target, hit}, true

	case "jail":
		jid := q.Get("jid")
		// Columns are the jails in the most recent snapshot (or just `jid`);
		// each series is that jail's process count over the range, 0 where the
		// jail was absent.
		for _, j := range latestJails(snaps, jid) {
			labels = append(labels, fmt.Sprintf("%d %s", j.JID, j.Name))
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, jj := range snap.Jails {
					if jj.JID == j.JID {
						series[i] = float64(jj.Procs)
					}
				}
			}
			cols = append(cols, series)
		}
		return labels, cols, true
	}
	return nil, nil, false
}

// timestamps returns the unix-second timestamp column shared by every series.
func timestamps(snaps []collect.Snapshot) []float64 {
	ts := make([]float64, len(snaps))
	for i, s := range snaps {
		ts[i] = float64(s.Time.Unix())
	}
	return ts
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	ch := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.broker.done:
			return
		case msg := <-ch:
			// msg is raw JSON; frame it as an SSE event.
			w.Write([]byte("data: "))
			w.Write(msg)
			w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

// handleWS is the WebSocket equivalent of handleStream: it pushes one raw-JSON
// snapshot text frame per sample. It exists alongside SSE (which remains the
// dashboard default) for clients that prefer WebSocket. The connection is
// read-only from the server's side; a reader goroutine drains control frames
// so close/ping are handled promptly.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written an error response.
	}
	defer conn.Close()

	ch := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.broker.done:
			return
		case msg := <-ch:
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}
}

// snapsSince returns snapshots at or after `since`, oldest first. It serves
// the full-resolution ring for the recent window and, when a history store is
// attached, prepends the store's (rolled-up) rows for the portion older than
// the ring holds — so long ranges work even across daemon restarts.
func (s *Server) snapsSince(since time.Time) []collect.Snapshot {
	ringSnaps := s.ring.Since(since)
	if s.store == nil {
		return ringSnaps
	}
	storeSnaps := s.store.Since(since)
	if len(ringSnaps) == 0 {
		return storeSnaps
	}
	// The ring wins where they overlap: keep store rows strictly older than
	// the oldest ring sample, then append the ring.
	cut := ringSnaps[0].Time
	out := make([]collect.Snapshot, 0, len(storeSnaps)+len(ringSnaps))
	for _, sn := range storeSnaps {
		if sn.Time.Before(cut) {
			out = append(out, sn)
		}
	}
	return append(out, ringSnaps...)
}

// snapsBanded is the band-mode counterpart of snapsSince: it returns three
// index-aligned series — average, min, and max. The store portion carries the
// real per-bucket envelope (when the store implements RollupStore); the ring
// portion is full-resolution, so its min and max are the sample itself.
func (s *Server) snapsBanded(since time.Time) (avg, lo, hi []collect.Snapshot) {
	ringSnaps := s.ring.Since(since)
	cut := time.Time{}
	if len(ringSnaps) > 0 {
		cut = ringSnaps[0].Time
	}
	older := func(t time.Time) bool { return len(ringSnaps) == 0 || t.Before(cut) }

	if rs, isRollup := s.store.(RollupStore); isRollup {
		sAvg, sLo, sHi := rs.RollupSince(since)
		for i, sn := range sAvg {
			if older(sn.Time) {
				avg = append(avg, sn)
				lo = append(lo, sLo[i])
				hi = append(hi, sHi[i])
			}
		}
	} else if s.store != nil {
		for _, sn := range s.store.Since(since) {
			if older(sn.Time) {
				avg = append(avg, sn)
				lo = append(lo, sn)
				hi = append(hi, sn)
			}
		}
	}

	for _, sn := range ringSnaps {
		avg = append(avg, sn)
		lo = append(lo, sn)
		hi = append(hi, sn)
	}
	return avg, lo, hi
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	// Accept plain integers as seconds for convenience.
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func collectTempNames(snaps []collect.Snapshot) []string {
	seen := map[string]bool{}
	var names []string
	for _, s := range snaps {
		for _, t := range s.Temps {
			if !seen[t.Name] {
				seen[t.Name] = true
				names = append(names, t.Name)
			}
		}
	}
	return names
}

// collectFSMounts returns the distinct mount points across snaps in first-seen
// order. If filter is non-empty, only that mount is returned (when present).
func collectFSMounts(snaps []collect.Snapshot, filter string) []string {
	seen := map[string]bool{}
	var names []string
	for _, s := range snaps {
		for _, f := range s.FS {
			if filter != "" && f.Mount != filter {
				continue
			}
			if !seen[f.Mount] {
				seen[f.Mount] = true
				names = append(names, f.Mount)
			}
		}
	}
	return names
}

// collectZFSPools returns the distinct pool names across snaps in first-seen
// order. If filter is non-empty, only that pool is returned (when present).
func collectZFSPools(snaps []collect.Snapshot, filter string) []string {
	seen := map[string]bool{}
	var names []string
	for _, s := range snaps {
		for _, z := range s.ZFS {
			if filter != "" && z.Pool != filter {
				continue
			}
			if !seen[z.Pool] {
				seen[z.Pool] = true
				names = append(names, z.Pool)
			}
		}
	}
	return names
}

// latestJails returns the jail set to build jail series columns from: the
// most recent snapshot's jails, or just the one matching jid if given.
func latestJails(snaps []collect.Snapshot, jid string) []collect.JailStat {
	if len(snaps) == 0 {
		return nil
	}
	last := snaps[len(snaps)-1].Jails
	if jid == "" {
		return last
	}
	for _, j := range last {
		if strconv.FormatInt(int64(j.JID), 10) == jid {
			return []collect.JailStat{j}
		}
	}
	return nil
}

// latestProcs returns the process set to build proc series columns from: the
// most recent snapshot's top-N, or just the one matching pid if given.
func latestProcs(snaps []collect.Snapshot, pid string) []collect.ProcStat {
	if len(snaps) == 0 {
		return nil
	}
	last := snaps[len(snaps)-1].Procs
	if pid == "" {
		return last
	}
	for _, p := range last {
		if strconv.FormatInt(int64(p.PID), 10) == pid {
			return []collect.ProcStat{p}
		}
	}
	return nil
}
