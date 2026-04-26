package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"

	psutil_host "github.com/shirou/gopsutil/v3/host"

	"github.com/nexusriot/beastiemon/internal/collect"
	"github.com/nexusriot/beastiemon/internal/store"
)

// Broker fans out snapshots to SSE clients.
type Broker struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
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

// Server wires together the ring store, broker, and HTTP mux.
type Server struct {
	ring   *store.Ring
	broker *Broker
	mux    *http.ServeMux
}

func New(ring *store.Ring, webFS fs.FS) *Server {
	s := &Server{
		ring:   ring,
		broker: &Broker{},
		mux:    http.NewServeMux(),
	}
	s.routes(webFS)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Ingest receives snapshots from the sampler.
func (s *Server) Ingest(snap collect.Snapshot) {
	s.ring.Push(snap)
	b, _ := json.Marshal(snap)
	msg := append([]byte("data: "), b...)
	msg = append(msg, '\n', '\n')
	s.broker.Publish(msg)
}

func (s *Server) routes(webFS fs.FS) {
	s.mux.Handle("/", http.FileServer(http.FS(webFS)))
	s.mux.HandleFunc("/api/host", s.handleHost)
	s.mux.HandleFunc("/api/metrics", s.handleMetrics)
	s.mux.HandleFunc("/api/series", s.handleSeries)
	s.mux.HandleFunc("/api/stream", s.handleStream)
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

// handleSeries returns a time series in uPlot format.
// Query params: metric (cpu|mem|net|disk), range (e.g. 15m, 1h), iface=, dev=
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := q.Get("metric")
	if metric == "" {
		metric = "cpu"
	}

	rangeDur := parseDuration(q.Get("range"), 15*time.Minute)
	since := time.Now().Add(-rangeDur)
	snaps := s.ring.Since(since)

	type seriesResp struct {
		Labels []string    `json:"labels"`
		Data   [][]float64 `json:"data"`
	}

	var resp seriesResp

	switch metric {
	case "cpu":
		resp.Labels = []string{"ts", "user", "sys", "idle", "total"}
		ts := make([]float64, len(snaps))
		user := make([]float64, len(snaps))
		sys := make([]float64, len(snaps))
		idle := make([]float64, len(snaps))
		total := make([]float64, len(snaps))
		for i, s := range snaps {
			ts[i] = float64(s.Time.Unix())
			user[i] = s.CPU.User
			sys[i] = s.CPU.Sys
			idle[i] = s.CPU.Idle
			total[i] = s.CPU.Total
		}
		resp.Data = [][]float64{ts, user, sys, idle, total}

		// Append per-core series
		if len(snaps) > 0 && len(snaps[0].CPU.PerCore) > 0 {
			ncores := len(snaps[0].CPU.PerCore)
			for c := 0; c < ncores; c++ {
				resp.Labels = append(resp.Labels, fmt.Sprintf("cpu%d", c))
				core := make([]float64, len(snaps))
				for i, snap := range snaps {
					if c < len(snap.CPU.PerCore) {
						core[i] = snap.CPU.PerCore[c]
					}
				}
				resp.Data = append(resp.Data, core)
			}
		}

	case "mem":
		resp.Labels = []string{"ts", "used", "free", "swap_used"}
		ts := make([]float64, len(snaps))
		used := make([]float64, len(snaps))
		free := make([]float64, len(snaps))
		swap := make([]float64, len(snaps))
		for i, s := range snaps {
			ts[i] = float64(s.Time.Unix())
			used[i] = float64(s.Mem.Used)
			free[i] = float64(s.Mem.Free)
			swap[i] = float64(s.Mem.SwapUsed)
		}
		resp.Data = [][]float64{ts, used, free, swap}

	case "load":
		resp.Labels = []string{"ts", "load1", "load5", "load15"}
		ts := make([]float64, len(snaps))
		l1 := make([]float64, len(snaps))
		l5 := make([]float64, len(snaps))
		l15 := make([]float64, len(snaps))
		for i, s := range snaps {
			ts[i] = float64(s.Time.Unix())
			l1[i] = s.Load.Load1
			l5[i] = s.Load.Load5
			l15[i] = s.Load.Load15
		}
		resp.Data = [][]float64{ts, l1, l5, l15}

	case "net":
		iface := q.Get("iface")
		resp.Labels = []string{"ts", "rx_bps", "tx_bps"}
		ts := make([]float64, len(snaps))
		rx := make([]float64, len(snaps))
		tx := make([]float64, len(snaps))
		for i, snap := range snaps {
			ts[i] = float64(snap.Time.Unix())
			for _, n := range snap.Net {
				if iface == "" || n.Interface == iface {
					rx[i] += n.RxBps
					tx[i] += n.TxBps
				}
			}
		}
		resp.Data = [][]float64{ts, rx, tx}

	case "disk":
		dev := q.Get("dev")
		resp.Labels = []string{"ts", "read_bps", "write_bps"}
		ts := make([]float64, len(snaps))
		rd := make([]float64, len(snaps))
		wr := make([]float64, len(snaps))
		for i, snap := range snaps {
			ts[i] = float64(snap.Time.Unix())
			for _, d := range snap.Disk {
				if dev == "" || d.Device == dev {
					rd[i] += d.ReadBps
					wr[i] += d.WriteBps
				}
			}
		}
		resp.Data = [][]float64{ts, rd, wr}

	case "temp":
		resp.Labels = []string{"ts"}
		ts := make([]float64, len(snaps))
		for i, snap := range snaps {
			ts[i] = float64(snap.Time.Unix())
		}
		resp.Data = [][]float64{ts}

		// One series per sensor name found in any snapshot.
		names := collectTempNames(snaps)
		for _, name := range names {
			resp.Labels = append(resp.Labels, name)
			series := make([]float64, len(snaps))
			for i, snap := range snaps {
				for _, t := range snap.Temps {
					if t.Name == name {
						series[i] = t.Celsius
					}
				}
			}
			resp.Data = append(resp.Data, series)
		}

	default:
		http.Error(w, "unknown metric", http.StatusBadRequest)
		return
	}

	writeJSON(w, resp)
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
		case msg := <-ch:
			w.Write(msg)
			flusher.Flush()
		}
	}
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
