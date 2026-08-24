# BeastieMon — Architecture

This document is the engineering reference for BeastieMon: design
intent, runtime architecture, on-disk layout, component contracts,
concurrency model, and the trade-offs behind every non-obvious choice.

For build & operate instructions, see [README.md](README.md).

---

## Table of Contents

1. [Goals & Non-Goals](#1-goals--non-goals)
2. [System Overview](#2-system-overview)
3. [Repository Layout](#3-repository-layout)
4. [Data Model](#4-data-model)
5. [Collector Subsystem](#5-collector-subsystem)
6. [Storage — `store.Ring`](#6-storage--storering)
7. [HTTP API](#7-http-api)
8. [Streaming (SSE) Pipeline](#8-streaming-sse-pipeline)
9. [Frontend](#9-frontend)
10. [CLI (`beastie`)](#10-cli-beastie)
11. [Configuration](#11-configuration)
12. [Concurrency Model](#12-concurrency-model)
13. [Process Lifecycle & rc.d](#13-process-lifecycle--rcd)
14. [Packaging](#14-packaging)
15. [Security Model](#15-security-model)
16. [Performance Characteristics](#16-performance-characteristics)
17. [Error Handling Strategy](#17-error-handling-strategy)
18. [Cross-Platform Story](#18-cross-platform-story)
19. [Future Extensions](#19-future-extensions)
20. [Design Decisions Log](#20-design-decisions-log)

---

## 1. Goals & Non-Goals

### Goals

| # | Goal | Implication |
|---|------|-------------|
| G1 | **Single static binary** for the daemon | `go:embed` for assets, no `libc`-bound deps |
| G2 | **Self-contained** — no Node, no proxy required | uPlot vendored, HTML/JS/CSS embedded |
| G3 | **Cheap at idle** — < 20 MB RSS, < 1 % of one core | In-memory ring; no DB; lean JSON encoding |
| G4 | **FreeBSD-native** — `rc.d`, `pkg(8)`, system user | Ships an `.in` template, manifest, prestart hook |
| G5 | **Live UX** — sub-second graph updates | Server-Sent Events fan-out |
| G6 | **Operationally simple** | One config, one log, one PID file, one user |
| G7 | **CLI usable without the daemon** | CLI imports the same `internal/collect` package |

### Non-Goals

- **No mandatory auth, no TLS.** Authentication is opt-in via `[auth]`
  (off by default); TLS is always the reverse proxy's job. Bind to
  `127.0.0.1` and front with nginx / Caddy for anything untrusted.
- **No long-term storage *by default*.** The ring holds one hour in RAM.
  Opt into `[store]` for bounded SQLite history, or scrape `/metrics` into
  Prometheus / Influx for real retention — the daemon itself is not a TSDB.
- **No complex rule engine.** `[alerts]` covers simple sustained
  threshold → webhook; anything richer (correlation, flapping suppression,
  routing) belongs in Alertmanager downstream of `/metrics`.
- **Not a fleet tool.** One daemon, one host. Federation is the operator's job.
- **No multi-tenancy.** Single dashboard, no per-user views.

The non-goals exist on purpose. They are what keeps the binary small,
the surface area auditable, and the operational complexity near zero. The
optional subsystems (`[auth]`, `[store]`, `[alerts]`, ZFS/jails) are all
off by default, so a default install stays exactly as lean as before.

---

## 2. System Overview

```
┌────────────────────────────────────────────────────────────────────────┐
│  beastied (single Go binary, runs as _beastie)                         │
│                                                                        │
│   ┌──────────────────┐   chan      ┌──────────────────────────┐        │
│   │ collect.Sampler  │ ──────────▶ │ main loop (cmd/beastied) │        │
│   │  Run(ctx)        │  Snapshot   │  for snap := range C {   │        │
│   │   • tick 1s      │   cap=4     │     server.Ingest(snap)  │        │
│   │   • collect ALL  │             │  }                       │        │
│   │   • emit to C    │             └─────────┬────────────────┘        │
│   └──────────────────┘                       │                         │
│                                              │ Ingest                  │
│                                              ▼                         │
│                              ┌───────────────────────────────┐         │
│                              │ store.Ring                    │         │
│                              │   Push (RWMutex)              │         │
│                              │   Last / Since                │         │
│                              │   3600 × Snapshot ≈ 7 MB      │         │
│                              └─────────────┬─────────────────┘         │
│                                            │                           │
│                              ┌─────────────┴─────────────────┐         │
│                              │ api.Server (net/http.ServeMux)│         │
│                              │   /api/host                   │         │
│                              │   /api/metrics  (Last)        │         │
│                              │   /api/series   (Since + xfm) │         │
│                              │   /api/alerts   (rule states) │         │
│                              │   /api/stream   (SSE)  /api/ws│         │
│                              │   /metrics (Prom)  /healthz   │         │
│                              │   /  → embed.FS (web/)        │         │
│                              └─────────────┬─────────────────┘         │
│                                            │                           │
│                              ┌─────────────┴─────────────────┐         │
│                              │ api.Broker                    │         │
│                              │   Subscribe/Unsubscribe/Publish│        │
│                              │   non-blocking fan-out        │         │
│                              └─────────────┬─────────────────┘         │
└────────────────────────────────────────────┼───────────────────────────┘
                                             │
                              ┌──────────────┴──────────────┐
                              ▼                             ▼
                       Browser (uPlot)               beastie CLI
                       SSE → live charts             (samples collect/ pkg
                       fetch /api/series             directly; --remote
                                                     reads /api/metrics)
                                                       │
                                                       ▼
                                                       host metrics
                                                       (no daemon needed)
```

### Two binaries, one collect package

The repository ships:

- **`beastied`** — long-running daemon. Owns the sampler, ring, HTTP server.
- **`beastie`** — CLI tool. **By default it bypasses the HTTP API entirely**
  and samples the host directly via the same `internal/collect` package, so
  the CLI works even when `beastied` isn't running — important when you SSH
  into a sick box at 3am. With `--remote` it instead reads the running
  daemon's `/api/metrics` (instant, warm deltas — §10, D20).

This is the single most important architectural choice: the collector
package is the contract, not the HTTP API. The API is one of two front-ends
to the same data source — and the CLI can consume either.

---

## 3. Repository Layout

```
beastiemon/
├── cmd/
│   ├── beastied/main.go             # daemon entrypoint
│   └── beastie/main.go              # CLI entrypoint (Beastie ASCII, colour bars)
├── internal/
│   ├── config/
│   │   └── config.go                # TOML decoder, defaults, duration shim, value clamping
│   ├── collect/
│   │   ├── types.go                 # Snapshot, CPUStats, … (wire schema; +ZFS/ARC/Jail)
│   │   ├── collector.go             # Sampler — orchestrates per-tick collection
│   │   ├── cpu.go                   # per-core delta CPU times
│   │   ├── mem.go                   # virtual+swap memory
│   │   ├── disk.go                  # devstat delta I/O
│   │   ├── net.go                   # per-NIC delta I/O
│   │   ├── fs.go                    # statfs(2) per mount, filtered
│   │   ├── temp.go                  # FreeBSD-only sysctl thermometers
│   │   ├── temp_other.go            # stub for non-FreeBSD builds
│   │   ├── proc.go                  # top-N processes by CPU%
│   │   ├── zfs.go / zfs_other.go    # FreeBSD-only ZFS pool + ARC (zpool + kstat)
│   │   ├── jail.go / jail_other.go  # FreeBSD-only jail enumeration (jls + ps)
│   │   └── bsdextra_parse.go        # pure parsers for zfs/jail (unit-tested anywhere)
│   ├── store/
│   │   ├── ring.go                  # in-memory circular buffer of Snapshots
│   │   ├── sqlite.go                # optional on-disk history (pure-Go SQLite; coarse tier, alert events)
│   │   └── rollup.go                # per-bucket avg/min/max aggregation
│   ├── alert/
│   │   └── alert.go                 # threshold rule engine + webhook, states/events, watchdog
│   └── api/
│       ├── server.go                # HTTP handlers + SSE/WS broker + auth gate + /api/alerts
│       ├── prometheus.go            # /metrics text exposition
│       └── *_test.go                # auth, series, alerts, prometheus, websocket tests
├── web/                             # embedded via //go:embed
│   ├── assets.go                    # embed.FS declaration
│   ├── index.html                   # dashboard scaffold
│   ├── app.js                       # uPlot wiring + SSE consumer + theme toggle
│   ├── style.css                    # dark + light themes
│   └── vendor/                      # populated by `gmake vendor-js`
│       ├── uplot.iife.min.js
│       └── uplot.min.css
├── freebsd/                         # packaging
│   ├── beastied.in                  # rc.d template (%%PREFIX%% substituted)
│   ├── beastiemon.conf              # default config
│   ├── newsyslog.conf               # log-rotation rule (→ newsyslog.conf.d/)
│   ├── +MANIFEST                    # pkg(8) manifest (%%VERSION%% substituted)
│   └── pkg-descr
├── Makefile                         # build / vendor / stage / pkg / install
├── go.mod
├── go.sum
├── LICENSE                          # MIT
├── DESIGN.md                        # this file
└── README.md                        # user docs
```

`internal/` enforces that downstream consumers cannot import any of
these packages — they are deliberately implementation detail. If we
ever expose a programmatic Go API, the wire-stable subset will move
out of `internal/`.

---

## 4. Data Model

The unit of currency throughout the entire system is a `collect.Snapshot`:

```go
// internal/collect/types.go
type Snapshot struct {
    Time   time.Time   `json:"ts"`
    CPU    CPUStats    `json:"cpu"`
    Mem    MemStats    `json:"mem"`
    Net    []NetStats  `json:"net"`
    Disk   []DiskStats `json:"disk"`
    FS     []FSStats   `json:"fs"`
    Temps  []TempStat  `json:"temps,omitempty"`
    Procs  []ProcStat  `json:"procs,omitempty"`
    ZFS    []ZFSStats  `json:"zfs,omitempty"`   // FreeBSD, opt-in
    ARC    *ARCStats   `json:"arc,omitempty"`   // FreeBSD, opt-in
    Jails  []JailStat  `json:"jails,omitempty"` // FreeBSD, opt-in
    Load   LoadStats   `json:"load"`
    Uptime uint64      `json:"uptime"`
}
```

Sub-types (all explicit; no `map[string]interface{}` anywhere):

| Field            | Approx size | Notes |
|------------------|------------:|-------|
| `CPUStats`       | ~80 B       | total/user/sys/idle %, `PerCore []float64` |
| `MemStats`       | ~64 B       | total/used/free/avail bytes + swap + percentages |
| `NetStats[]`     | ~48 B each  | per-NIC rx/tx bps + pps |
| `DiskStats[]`    | ~48 B each  | per-dev read/write bps + IOPS |
| `FSStats[]`      | ~80 B each  | per-mount bytes + percent |
| `TempStat[]`     | ~24 B each  | sensor name → °C |
| `ProcStat[]`     | ~80 B each  | top-N processes by CPU |
| `ZFSStats[]`     | ~64 B each  | per-pool size/alloc/free + health (opt-in) |
| `ARCStats`       | ~40 B       | ARC size/target/max + lifetime hit rate (opt-in) |
| `JailStat[]`     | ~72 B each  | jid, name, hostname, path, process count (opt-in) |
| `LoadStats`      | ~24 B       | 1/5/15-minute load average |
| `Uptime`         | 8 B         | seconds since boot |

A typical snapshot serialises to ~1.5–2 KB JSON. A 4-core box with 2
NICs, 4 mounts, 1 temp sensor, and `top_procs = 5` lands at ~1.7 KB.

### Why a single struct?

Two reasons:

1. **Wall-clock-aligned snapshots.** Operators correlate "CPU spiked
   when network spiked" — that only works if all metrics share a
   timestamp. Collecting CPU at T₀ and disk at T₀ + 200 ms muddies
   the picture.
2. **Simple storage and slicing.** Ring is `[]Snapshot`. `Since(t)`
   is a single bounded walk. No joins, no indexes, no per-metric
   buffers to keep in sync.

### Why typed fields and not a map?

Type safety from collector to JSON to JS. The wire format is **a
contract**: renaming `RxBps → RxBytesPerSecond` would break the
dashboard, so it's locked behind a single Go struct that the compiler
checks at every step.

---

## 5. Collector Subsystem

### Collector "interface" — there isn't one

Each collector exposes its own typed `Collect()` method:

```go
type CPUCollector  struct{ … }; func (c *CPUCollector)  Collect() CPUStats
type MemCollector  struct{};    func (m *MemCollector)  Collect() MemStats
type DiskCollector struct{ … }; func (d *DiskCollector) Collect() []DiskStats
type NetCollector  struct{ … }; func (n *NetCollector)  Collect() []NetStats
type FSCollector   struct{ … }; func (f *FSCollector)   Collect() []FSStats
type ProcCollector struct{ … }; func (p *ProcCollector) Collect() []ProcStat
```

There is **no** `type Collector interface { Collect() any }`. Adding a
new metric means: add a typed field to `Snapshot`, add a struct with a
`Collect()` method, wire it in `Sampler.collect()`. The compiler catches
anyone forgetting to plumb the new field through to the API and the
dashboard.

### Source table

| Collector | Source | Privilege | Notes |
|-----------|--------|-----------|-------|
| `cpu`     | `gopsutil/cpu.Times(true)` | none | per-core; computes deltas locally |
| `mem`     | `gopsutil/mem.VirtualMemory` + `SwapMemory` | none | absolute values, not deltas |
| `disk`    | `gopsutil/disk.IOCounters` | **`operator` group** | wraps `devstat(3)` |
| `net`     | `gopsutil/net.IOCounters(true)` | none | honours `net_exclude` |
| `fs`      | `gopsutil/disk.Partitions` + `Usage` | none | filtered by `fs_include`, dedupes mounts |
| `temp`    | direct `unix.SysctlRaw` | none | FreeBSD-only build tag |
| `proc`    | `gopsutil/process.Processes` + `.Times` | none | top-N by CPU delta |
| `load`    | `gopsutil/load.Avg` | none | wraps `getloadavg(3)` |
| `uptime`  | `gopsutil/host.Uptime` | none | seconds since boot |
| `zfs`     | `zpool list` (exec) | none | FreeBSD-only, opt-in; per-pool capacity |
| `arc`     | `kstat.zfs.misc.arcstats.*` sysctl | none | FreeBSD-only, opt-in; ARC size + lifetime hit rate |
| `jails`   | `jls` + `ps -axo jid=` (exec) | none | FreeBSD-only, opt-in; per-jail process count |

The last three are FreeBSD-only and gated behind `[collect] zfs` / `jails`
(default off), because each shells out every tick. Their exec/sysctl wrappers
live in `//go:build freebsd` files with non-FreeBSD stubs; the output parsing
sits in `bsdextra_parse.go` (no build tag) so it is unit-tested on any host.

### Delta-based collectors

CPU times, disk I/O, network I/O, and per-process CPU are **cumulative
counters** in the kernel. The collector retains the previous sample
and computes:

```
rate = (current_counter - previous_counter) / elapsed_seconds
```

Consequences:

- **First sample is empty.** No previous to diff against. `Sampler.Run`
  primes once and sleeps one interval before publishing.
- **Counter resets are clamped.** When an interface or device is destroyed
  and recreated under the same name, its kernel counter restarts at zero and
  `current - previous` on a uint64 would wrap to ~1.8 × 10¹⁹, poisoning
  charts, stored roll-up envelopes, and alerts. `deltaU64` returns 0 for
  that tick instead (per-process CPU has the equivalent `pct < 0` clamp).
  Genuine 64-bit wrap-around (~1.8 EB of traffic) is not a practical concern.
- **Stopped processes disappear** from `proc.prev` on the next tick;
  no per-process state survives a PID exit.

### Temperature collector — FreeBSD-only

`gopsutil`'s sensor support on FreeBSD is incomplete. `temp.go` reads
the relevant `sysctl` MIBs directly via `golang.org/x/sys/unix.SysctlRaw`,
decodes the kernel's *deci-Kelvin* (tenths of a Kelvin) format, and
returns °C:

```go
//go:build freebsd

dk := uint32(raw[0]) | uint32(raw[1])<<8 |
      uint32(raw[2])<<16 | uint32(raw[3])<<24
celsius := float64(dk)/10.0 - 273.15
```

It probes `dev.cpu.N.temperature` for N=0..63 (stops on first error)
and `hw.acpi.thermal.tzN.temperature` for N=0..15. The file is gated
by `//go:build freebsd`; a stub (`temp_other.go`) lets the package
compile on Linux for development.

### Process collector — two-pass ranking

`proc.go` runs in two passes per tick:

1. **Pass 1.** Iterate all PIDs, compute CPU% from delta of
   `times.User + times.System`. Skip processes without a previous
   sample (new PIDs this tick).
2. **Sort + truncate** to top-N (default 5).
3. **Pass 2.** Only for the survivors, call `Name()`, `MemoryInfo()`,
   `MemoryPercent()`. Each is an extra syscall, and we don't pay them
   for the long tail we won't show.

`topN` is configurable via `[collect] top_procs` (default 5). Setting
it to 0 also yields 5 (defensive default inside `NewProcCollector`).

---

## 6. Storage — `store.Ring`

Fixed-capacity circular buffer of `Snapshot`:

```go
type Ring struct {
    mu    sync.RWMutex
    buf   []collect.Snapshot
    head  int     // next write index
    cap_  int
    count int
}
```

| Operation     | Complexity | Locking |
|---------------|------------|---------|
| `Push(s)`     | O(1)       | write   |
| `Last()`      | O(1)       | read    |
| `Since(t)`    | O(N)       | read    |
| `All()`       | O(N)       | read    |

Default capacity is 3600 = one hour at 1 s. Each snapshot is ~2 KB in
memory (Go structs, not JSON), so the buffer caps at ~7 MB and stays
bounded forever regardless of uptime.

The ring itself has no on-disk persistence — a restart loses its hour of
1 s-resolution history, and that keeps the hot path at ~60 lines with zero I/O.

### Optional SQLite history — `store.SQLite`

For retention beyond the ring, `[store]` enables a **parallel** on-disk store
(`store/sqlite.go`), using the pure-Go `modernc.org/sqlite` driver so the
static-binary / `CGO_ENABLED=0` guarantee (G1) holds. It:

- receives every snapshot via a non-blocking `Push` (buffered channel, drops
  on overflow — the ring is authoritative for the live view), so it never
  stalls the sampler;
- **rolls up** each `resolution` window (default 1 m) into a single row via an
  in-memory accumulator (`rollup.go`): rather than keeping one arbitrary
  sample and discarding the rest, it stores the window's element-wise
  **average, minimum, and maximum** as three JSON `Snapshot`s (columns `data`,
  `dmin`, `dmax`), keyed by the truncated bucket timestamp. Averaging alone
  would erase the short spikes that make history useful; the min/max envelope
  preserves them (surfaced by `/api/series?band=1`, §7). Set-valued fields that
  can't be averaged — processes, jails, ARC — are carried from the last sample
  in the bucket. The still-filling current bucket is flushed on rollover or
  `Close`; the ring covers that recent window meanwhile;
- **prunes** rows older than `retention` (default 30 d) hourly;
- **coarsens** rows older than `coarse_after` (default 7 d) into one row per
  `coarse_resolution` (default 1 h): the writer's maintenance pass (at startup
  and hourly) merges each aged bucket group in place — exact min/max, mean of
  bucket means for the average — batch-limited and cursor-driven so an upgrade
  backlog drains without loading the whole tail into memory (D19). Buckets
  already down to a single row are skipped, so steady state does no work;
- **persists alert events** in a second table (`alert_events`, raw `Event`
  JSON) written synchronously by the engine's sink — events are rare — and
  pruned on the same `retention` schedule; `/api/alerts` reads it newest-first;
- serves `Since(t)` (averages) and `RollupSince(t)` (average + envelope) for the
  API's long-range merge (§7).

A single background goroutine owns all sample writes and maintenance;
`SetMaxOpenConns(1)` serialises those against reads from HTTP handlers and the
rare alert-event insert, sidestepping SQLite lock contention at this
~1-write/minute rate. Rows store whole `Snapshot`s as JSON, so the schema
never needs migrating when the struct grows; the two envelope columns were
added with a guarded `ALTER TABLE` that tolerates upgrading an older
two-column database in place. Prometheus (`/metrics`) remains the answer for
real long-term, queryable retention; SQLite just widens the dashboard's own
window.

### Why a custom ring instead of a library?

- Off-the-shelf ring buffers are either generic (interface{}/generics
  fighting Go's type system) or lock-per-element (unnecessary overhead).
- A 60-line bespoke struct is auditable in one sitting.
- The API package consumes the `[]Snapshot` that `Since()` returns
  directly: the slice is built fresh per call and never aliases the
  ring's backing array, and `RLock` lets multiple readers run
  concurrently.

---

## 7. HTTP API

Implemented in `internal/api/server.go`. All handlers register on a
single `*http.ServeMux` — no router library, no middleware framework.

### Endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/`                            | Serves embedded `index.html` and static assets |
| `GET`  | `/api/host`                    | Hostname, OS, platform version, kernel, process count |
| `GET`  | `/api/metrics`                 | Latest `Snapshot` (returns 503 if no data yet) |
| `GET`  | `/api/series?metric=…&range=…` | Historical time series, uPlot-shaped (`cpu\|mem\|load\|net\|disk\|temp\|fs\|proc\|zfs\|arc\|jail`) |
| `GET`  | `/api/alerts`                  | Alert rule states (ok/pending/firing, incl. the watchdog) + recent events (`?limit=`) |
| `GET`  | `/api/stream`                  | Server-Sent Events: one `Snapshot` per sample |
| `GET`  | `/api/ws`                      | WebSocket: same stream, one raw-JSON frame per sample |
| `GET`  | `/metrics`                     | Prometheus text exposition of the latest `Snapshot` |
| `GET`  | `/healthz`                     | Liveness probe — returns `ok` |

`/api/series` transparently spans the in-memory ring and, when `[store]` is
configured, the SQLite history: recent points come from the ring at full
resolution, older points from the (downsampled) store, merged by timestamp
(the ring wins on overlap). See §6.

### `/api/metrics` — current snapshot

```json
{
  "ts": "2026-06-04T15:01:23Z",
  "cpu": {"total": 42.3, "user": 35.1, "sys": 7.2, "idle": 57.7,
          "per_core": [48.0, 39.0, 41.0, 40.0]},
  "mem": {"total": 8589934592, "used": 5260902400, "free": 3329032192,
          "available": 3522502656, "used_pct": 61.2,
          "swap_total": 2147483648, "swap_used": 0, "swap_pct": 0.0},
  "net":  [{"iface": "em0", "rx_bps": 1258291.0, "tx_bps": 419430.4,
            "rx_pps": 850.0, "tx_pps": 420.0}],
  "disk": [{"dev": "ada0", "read_bps": 13002138.0, "write_bps": 5452595.2,
            "read_iops": 124.0, "write_iops": 48.0}],
  "fs":   [{"mount": "/", "dev": "/dev/ada0p2", "fstype": "ufs",
            "total": 68719476736, "used": 19541442150, "free": 49278034586,
            "used_pct": 28.4}],
  "temps":[{"name": "cpu0", "celsius": 52.3}],
  "procs":[{"pid": 845, "name": "beastied", "cpu_pct": 0.3,
            "mem_pct": 0.17, "rss": 14680064}],
  "load": {"load1": 0.82, "load5": 0.75, "load15": 0.71},
  "uptime": 442935
}
```

### `/api/series` — uPlot-shaped time series

uPlot expects `[[timestamps], [series1], [series2], …]`. The handler
returns exactly that shape so the JS doesn't need to transform the data:

```json
{
  "labels": ["ts", "user", "sys", "idle", "total", "cpu0", "cpu1", "cpu2", "cpu3"],
  "data":   [[1717513283, 1717513284, …],
             [12.3, 11.8, …],
             [ 4.1,  3.9, …],
             [83.6, 84.3, …],
             [16.4, 15.7, …],
             [10.0, 11.0, …], […], […], […]]
}
```

Query parameters:

| Parameter | Values | Default |
|-----------|--------|---------|
| `metric`  | `cpu` \| `mem` \| `load` \| `net` \| `disk` \| `temp` \| `fs` \| `proc` \| `zfs` \| `arc` \| `jail` | `cpu` |
| `range`   | Go duration (`15m`, `1h`) or integer seconds | `15m` |
| `iface`   | NIC name (when `metric=net`) | sum all |
| `dev`     | device name (when `metric=disk`) | sum all |
| `mount`   | mount point (when `metric=fs`) | all mounts |
| `pid`     | process id (when `metric=proc`) | current top-N |
| `pool`    | pool name (when `metric=zfs`) | all pools |
| `jid`     | jail id (when `metric=jail`) | current jails |
| `band`    | `1`/`true` → append `<label>_min`/`_max` envelope columns | off |

Notes:

- Each metric's columns are built by one `seriesColumns` helper over a
  `[]Snapshot`; the handler prepends the shared `ts` column. `band=1` calls it
  three times — over the average, min, and max series from `snapsBanded` — and
  interleaves the envelope columns, so it reuses the exact per-metric logic
  rather than duplicating it (D18, §6).
- For `metric=net|disk` without a filter, the handler **sums all
  interfaces/devices** per timestamp. With a filter, only that one is
  returned.
- `metric=temp` returns one series per sensor name that appeared in
  any snapshot within the range; `zfs` one used-% series per pool.
- `metric=proc` builds one CPU-% series per PID in the latest snapshot's
  top-N; `jail` one process-count series per jail in the latest snapshot.
- `metric=arc` returns fixed `size`/`target` (bytes) and `hit_rate` (%)
  columns — consumers pick columns or chart on separate axes.
- Ranges longer than the ring's contents return only what's available.
- Unknown metrics → 400.

### `/api/alerts` — rule states + recent events

Serves the alert engine's live view (via the `AlertSource` interface the
daemon wires with `SetAlerts`): every rule's `ok`/`pending`/`firing` state
with its last evaluated value, the `stale_after` watchdog as a synthetic
`watchdog` rule, and recent events (`?limit=`, default 50, cap 500). Events
come from the store's `alert_events` table when persistence is on — they
survive restarts — else from the engine's capped in-memory history.
`enabled: false` with empty arrays when no alerts are configured. The
dashboard's Alerts card polls this every 10 s.

### `parseDuration` quirk

For convenience, `range=300` is parsed as 300 seconds (not as Go's
`ParseDuration` which would error). Go duration syntax (`5m`, `1h`)
still works. This makes the API friendlier from `curl` and shell loops.

---

## 8. Streaming (SSE) Pipeline

### Why SSE and not WebSocket?

| Property                | SSE       | WebSocket |
|-------------------------|-----------|-----------|
| Direction               | server→client | bidirectional |
| Transport               | plain HTTP | upgraded HTTP |
| Reverse-proxy friendly  | yes       | needs `Upgrade` headers |
| Auto-reconnect          | built into `EventSource` | manual |
| Debug with `curl`       | trivial   | annoying |
| Required for BeastieMon | ✅ (one-way push) | overkill |

The dashboard only ever consumes; nothing flows browser→server. SSE is
the natural fit, and remains the dashboard default.

A WebSocket endpoint (`/api/ws`, `gorilla/websocket`) is also offered for
clients that prefer it. Both share the same `Broker`: since it now carries
**raw JSON** (framing moved into each transport), the SSE handler prepends
`data: …\n\n` while the WS handler writes the JSON as a single text frame.
The broker doesn't know or care which transport a subscriber is — exactly the
extensibility §19 predicted.

### Broker

```go
type Broker struct {
    mu      sync.Mutex
    clients map[chan []byte]struct{}
    done    chan struct{} // closed by Server.Close on shutdown
}
```

`Subscribe()` creates a buffered channel (capacity 8); `Publish(data)`
fans out **non-blockingly**. The `done` channel exists for shutdown: SSE/WS
connections are never idle, so without it `http.Server.Shutdown` would sit
out its whole deadline waiting for them — `Server.Close` closes `done`, every
streaming handler's `select` returns, and `Shutdown` completes in
milliseconds (§12):

```go
for ch := range b.clients {
    select {
    case ch <- data:        // happy path
    default:                // drop for slow consumer
    }
}
```

A slow client loses data, never blocks the pipeline. This is the right
trade-off because:

- Full history is recoverable via `/api/series` if a client misses points.
- A blocking publish would back-pressure the sampler and freeze the
  whole daemon when one browser tab is paused.

### End-to-end flow per tick

```
sampler tick ──► collect snapshot ──► sampler.C
                                          │
                                          ▼
                              main loop receives snap
                                          │
                                          ▼
                              api.Server.Ingest(snap)
                              ├─► ring.Push(snap)
                              └─► json.Marshal + broker.Publish
                                          │
                              fan-out to N subscriber channels
                                          │
                                          ▼
                              SSE handler writes "data: …\n\n"
                              and Flush()es each connection
```

Total per-tick wall time: ~30 µs JSON encode + ~5 µs ring push + N×~3 µs
broker publishes. For typical N=1–3 dashboards open this is well under
1 ms, leaving ~999 ms of the second idle.

### SSE wire format

```
data: {"ts":"2026-06-04T15:01:23Z","cpu":{...},...}\n\n

data: {"ts":"2026-06-04T15:01:24Z","cpu":{...},...}\n\n
```

No `event:` or `id:` fields — the dashboard treats every event as
"latest snapshot, replace state."

---

## 9. Frontend

### Stack

- **Vanilla JavaScript**, ~600 lines, no framework.
- **uPlot** for charts (~50 KB minified). The fastest open-source
  time-series chart library; renders 100k points in <50 ms.
- **CSS Grid** for the dashboard layout. Four-column wide, two-column
  tablet, single-column phone.
- **No build step** — the source is what runs. `gmake vendor-js`
  downloads uPlot once; everything else is pure source.

### Page boot sequence

1. `fetch('/api/host')` → fills the header strip (hostname, OS, kernel).
2. `fetch('/api/series?metric=…&range=15m')` for each card → seeds
   historical chart data.
3. `fetch('/api/alerts')` → renders the Alerts card (rule states + recent
   events); re-polled every 10 s — rule state changes on the daemon's
   schedule, not the snapshot stream, so a slow poll is enough. The card
   stays hidden until alerts are configured.
4. Open `EventSource('/api/stream')` → on each snapshot:
   - Append to each chart's data array.
   - Trim points older than the selected range.
   - Call `chart.setData()` (uPlot's incremental update path).
   - Re-render temperature gauges, filesystem bars, top-procs table.
5. Range selector (5 m / 15 m / 1 h / 6 h / 24 h) refetches series for
   the new window.
6. Per-iface / per-device tab buttons are rebuilt lazily on the first SSE
   event after load or a range change (we don't know iface names until
   then); their click handlers are delegated to the tab strips and bound
   once at init, so rebuilds never stack duplicate listeners.

### Embedded vs. CDN assets

`web/assets.go` declares an `embed.FS` covering the dashboard files.
By default uPlot loads from unpkg's CDN. Running `gmake vendor-js`:

1. Downloads `uPlot.iife.min.js` and `uPlot.min.css` to `web/vendor/`.
2. Rewrites the `<script>` / `<link>` tags in `index.html` to use the
   local paths.
3. Patches `assets.go`'s `//go:embed` directive to include `vendor/`.

After `vendor-js`, the binary embeds everything and runs offline.

---

## 10. CLI (`beastie`)

### Key design choice: standalone by default, remote on request

By default `beastie` does **not** talk to the HTTP API. It imports
`internal/collect` and `internal/config` directly, creates a sampler,
takes one sample, and prints it. This means:

- Works when `beastied` isn't running.
- Works when the dashboard port is blocked.
- Shows exactly the same numbers as the dashboard (same code path).

The cost of standalone sampling is one full `interval` of warm-up per
invocation (delta collectors need two readings), bounded by
`collectTimeout` = max(5 s, 2 × interval + 2 s); a timed-out collection warns
on stderr rather than printing zeros as if real.

`--remote` flips every command (incl. `top` and `check`) to `GET
/api/metrics` on a running daemon instead (D20): `auto` resolves the target
from the config's `server.listen`, otherwise it accepts `host:port`, a full
URL, or an absolute path dialled as beastied's Unix socket. Credentials reuse
the config's `[auth]` — bearer token first, else basic. Remote answers in
milliseconds (the daemon's deltas are warm) and sees the daemon's exact view,
including FreeBSD extras when enabled there.

### Command dispatch

```
beastie                # status (all panels)
beastie status         # explicit
beastie cpu            # CPU only, with per-core breakdown
beastie mem            # memory + swap
beastie net            # per-NIC throughput
beastie disk           # per-device I/O
beastie fs             # filesystem usage
beastie temp           # temperature sensors
beastie proc           # top-N processes by CPU
beastie load           # load average
beastie top            # like top(1) — continuous refresh
beastie version
beastie help
```

Flags (must precede the command): `-config <path>` for a non-default
config file, `--json` for machine-readable output, `--no-color` to force
plain text, `--remote <addr>` to read from a running daemon.

### JSON output (`--json`)

`--json` short-circuits the text path: no banner, no host line, no ANSI —
just JSON on stdout. Each subcommand marshals its slice of the
`Snapshot` (`snap.CPU`, `snap.Mem`, …) using the same struct tags the
daemon serves, so `beastie --json cpu` and `/api/metrics`'s `cpu` object
are byte-identical. `status` (the default) emits the whole `Snapshot`.
`top --json` emits **NDJSON** — one compact snapshot object per interval —
which pipes cleanly into `jq` or a log shipper. The `collect.Snapshot`
contract (§4) is what makes this a few lines of `switch`, not a parallel
formatter.

### Continuous mode (`top`)

The `top` subcommand runs the same one-shot snapshot in a loop,
clearing the screen between renders, sleeping one interval between
frames. With local sampling each frame also pays the delta warm-up, so
the effective cadence is ~2× the interval; with `--remote` the fetch is
instant and frames land every interval. `Ctrl-C` stops it.

### Banner & colour guard

Each interactive invocation prints the Beastie mascot in ANSI red over a
`BeastieMon v<version>` line. Colour and the banner are emitted **only** when
stdout is a terminal (`os.Stdout.Stat()` char-device check) and neither
`NO_COLOR` nor `--no-color` is set; otherwise `applyColor(false)` blanks the
escape vars and the banner is suppressed, so pipes/`less`/files get clean
text. The colour constants are therefore `var`s, and the banner is built at
print time from them (see D-note in `cmd/beastie`).

### Check mode

`beastie check [--warn N] [--crit N] <metric>` turns one metric into a
nagios/Icinga plugin: it prints a single `STATUS: label = value | perfdata`
line and exits `0/1/2/3`. `checkValue` maps the metric to a scalar (cpu total,
mem/swap %, load1, worst fs %, hottest sensor, total net/disk bytes-per-sec)
and `evalCheck` applies the thresholds (higher is worse; a threshold ≤ 0 is
"unset"). Both are pure and unit-tested; output is always plain text
regardless of TTY. A failed collection — local timeout or unreachable
`--remote` daemon — exits `UNKNOWN` (3) instead of a false `OK … = 0.00`.

---

## 11. Configuration

### Schema

```toml
[server]
listen = "127.0.0.1:8088"           # string, "host:port"

[collect]
interval    = "1s"                  # Go duration
ring_size   = 3600                  # int, snapshots
fs_include  = ["/", "/var"]         # []string, mount paths; empty = all
net_exclude = ["lo0"]               # []string, NIC names
top_procs   = 5                     # int, top-N for proc panel
zfs         = false                 # bool, opt-in ZFS pool+ARC (FreeBSD, execs)
jails       = false                 # bool, opt-in jail panel (FreeBSD, execs)

[auth]                              # optional; disabled when all empty
username    = ""                    # string; with password → HTTP Basic
password    = ""                    # string
token       = ""                    # string; → Bearer token / ?token=

[store]                             # optional; disabled when path empty
path        = ""                    # string; SQLite history file
retention   = "720h"                # Duration; prune older (default 30d)
resolution  = "1m"                  # Duration; roll-up window (default 1m)
coarse_after      = "168h"          # Duration; re-aggregate older rows (default 7d; "0s" = off)
coarse_resolution = "1h"            # Duration; coarse bucket width (default 1h)

[alerts]                            # optional; disabled with no rules and no stale_after
webhook     = ""                    # string; default webhook for rules (and the watchdog)
format      = ""                    # string; default payload: raw|slack|discord
stale_after = "0s"                  # Duration; sampler watchdog window (default off)
# [[alerts.rule]] name, metric, field, op, threshold, for,
#                 repeat, hysteresis, webhook, format
```

### Loader

```go
// internal/config/config.go
func Load(path string) (Config, error) {
    cfg := Default()
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return cfg, nil          // missing file = defaults
    }
    _, err := toml.DecodeFile(path, &cfg)
    if err == nil {
        cfg.normalize()          // clamp nonsensical values to defaults
    }
    return cfg, err
}
```

Properties:

- Missing config file is non-fatal — defaults work everywhere.
- Partial files are merged onto defaults — you only need to specify
  what you want to change.
- Nonsensical values are clamped back to defaults rather than crashing the
  daemon: `interval = "0s"` would panic `time.NewTicker`, `ring_size = 0`
  would panic the ring's first `Push`. `normalize` covers interval,
  ring_size, top_procs, retention, and resolution. (`coarse_after = "0s"` is
  *not* clamped — zero legitimately means "coarse tier off".)
- The `Duration` shim (exported so external packages/tests can build rule
  values) implements `encoding.TextUnmarshaler` so the TOML can use natural
  strings like `"500ms"`.

### Defaults

| Field             | Default                            |
|-------------------|------------------------------------|
| `listen`          | `127.0.0.1:8088`                   |
| `interval`        | `1s`                               |
| `ring_size`       | `3600`                             |
| `fs_include`      | `["/", "/var", "/usr", "/tmp"]`    |
| `net_exclude`     | `["lo0"]`                          |
| `top_procs`       | `5`                                |
| `zfs` / `jails`   | `false` (opt-in FreeBSD panels)    |
| `auth.*`          | empty (authentication disabled)    |
| `store.path`      | empty (persistence disabled)       |
| `store.retention` | `720h` (30 days)                   |
| `store.resolution`| `1m`                               |
| `store.coarse_after` | `168h` (7 days; `0s` disables the coarse tier) |
| `store.coarse_resolution` | `1h`                      |
| `alerts.rule[]`   | none                               |
| `alerts.stale_after` | `0s` (watchdog disabled)        |

---

## 12. Concurrency Model

### Goroutines

The daemon has these kinds of goroutines:

1. **Main goroutine** — `select` over sampler channel / SIGHUP / a 1 s
   watchdog tick / shutdown; `server.Ingest(snap)` then `alerts.Eval(snap)`
   per tick, `alerts.CheckStale(now)` per watchdog tick (the watchdog can't
   ride on snapshots — a wedged sampler stops producing them).
2. **Sampler goroutine** — runs `sampler.Run(ctx)`, ticks every interval.
   Under a child context so a SIGHUP reload can cancel and replace it.
3. **HTTP server goroutine** — `http.Server.Serve(ln)`'s accept loop.
4. **One per HTTP connection** — `net/http` spawns these; the SSE/WS handlers
   block on `select { ctx.Done() / broker.done / msg := <-ch }` (the WS
   handler also has a short-lived reader goroutine to drain control frames /
   detect close).
5. **SQLite writer goroutine** (only when `[store]` is set) — drains the
   store's buffered channel, persists downsampled rows, and runs maintenance
   (retention prune + coarse-tier re-aggregation) at startup and hourly.
6. **Webhook POSTs** (only with `[alerts]`) — each fired event POSTs on its
   own short-lived goroutine so a slow endpoint never stalls `Eval`.

There are no worker pools or work queues; fan-in is the ring's RWMutex and the
broker's mutex.

### Synchronisation primitives

| Resource                | Primitive    | Discipline |
|-------------------------|--------------|------------|
| `store.Ring.buf`        | `sync.RWMutex` | Push takes write; Last/Since take read |
| `api.Broker.clients`    | `sync.Mutex` | held briefly for fan-out + map mutation |
| `api.Server.auth`/`.alerts` | `sync.RWMutex` | handlers read; SIGHUP reload swaps (SetAuth/SetAlerts) |
| `alert.Engine` state    | `sync.Mutex` | Eval/CheckStale mutate on the main loop; States/Events read from HTTP handlers |
| Sampler→main delivery   | `chan Snapshot` cap 4 | non-blocking send; drop on overflow |
| Broker→SSE/WS delivery  | `chan []byte` cap 8 per client | raw JSON; non-blocking send; drop on overflow |
| Broker shutdown         | `done chan struct{}` | closed once by `Server.Close`; unblocks streaming handlers |
| main→SQLite writer      | `chan Snapshot` cap 64 | non-blocking Push; drop on overflow (ring is authoritative) |
| `SQLite.agg`            | `sync.Mutex` | guards the accumulating bucket (ingest/flush) |
| SQLite DB handle        | `SetMaxOpenConns(1)` | one connection serialises writer, HTTP reads, and alert-event inserts |

### Shutdown & reload

```go
rootCtx, stop := signal.NotifyContext(ctx, SIGINT, SIGTERM)
sampCtx, sampCancel := context.WithCancel(rootCtx)
go sampler.Run(sampCtx)
httpSrv := &http.Server{Handler: srv}; go httpSrv.Serve(ln)
signal.Notify(hup, SIGHUP)
for {
    select {
    case <-rootCtx.Done():           // SIGINT/SIGTERM
        sampCancel()
        srv.Close()                  // closes broker.done → SSE/WS handlers return
        httpSrv.Shutdown(ctx5s)      // graceful; returns promptly (streams already gone)
        return                       // deferred store.Close() flushes + closes DB
    case <-hup:                      // SIGHUP: hot reload
        newCfg := config.Load(path)
        srv.SetAuth(newCfg.Auth); buildAlerts(newCfg.Alerts) // engine + sink + SetAlerts
        sampCancel(); sampler = collect.NewSampler(newCfg); go sampler.Run(newSampCtx)
    case now := <-staleTick.C:       // 1 s wall-clock tick
        alerts.CheckStale(now)       // sampler watchdog (no-op unless stale_after set)
    case snap := <-sampler.C:
        srv.Ingest(snap); alerts.Eval(snap)
    }
}
```

**Graceful shutdown** (now that a store can hold open a DB handle):
`srv.Close()` first unblocks the never-idle SSE/WS handlers (via the broker's
`done` channel — without this, `Shutdown` would always sit out its full 5 s
deadline with a dashboard open), then `http.Server.Shutdown` drains the rest,
then the deferred `store.Close()` stops the writer (flushing the open bucket)
and closes SQLite. **SIGHUP reload** re-reads the config and applies the
hot-reloadable parts in place — auth, the alert engine (rules, `stale_after`,
re-wired to the API and the event sink via `buildAlerts`; note the rebuilt
engine starts with fresh firing state), and the sampler (interval,
`fs_include`, `net_exclude`, `top_procs`, `zfs`, `jails`) by cancelling and
re-launching it under a fresh child context. `listen`, `ring_size`, and the
store path require a restart (the daemon logs a warning if they changed). The
`select` re-reads `sampler.C` each iteration, so swapping the `sampler`
variable is race-free.

### What can go wrong

- **Slow SSE client backs up** — fan-out drops, browser misses points,
  recovers via `/api/series` next time it requests history.
- **Sampler runs slow** (e.g. devstat call stalls) — ticker drops ticks
  (`time.Ticker` semantics); we get fewer samples but no goroutine
  pile-up.
- **Many SSE connections** — each gets a buffered chan; broker
  publishing is O(N) in client count. At 100 clients × 1 KB snapshot
  × 1 Hz that's 100 KB/s of writes per tick — fine.

---

## 13. Process Lifecycle & rc.d

### `freebsd/beastied.in` — two non-obvious details

#### 1. `procname=/usr/sbin/daemon`, **not** `…/beastied`

`daemon(8) -P /var/run/beastied.pid` writes its own (supervisor) PID,
not the child's. `rc.subr`'s status / stop machinery uses
`pgrep -F pidfile procname` — so `procname` must match the process
that owns the pidfile.

If `procname` pointed at the `beastied` binary:

- `service status` would always report "not running" while the daemon
  was alive (PID mismatch).
- `service stop` would fail to find the supervisor, leaving an
  unkillable auto-restarting tree.

This is documented inline in `beastied.in` to spare the next maintainer
the debugging session.

#### 2. The "runas" variable is `beastied_runas`, **not** `beastied_user`

`rc.subr` treats `${name}_user` as a **magic** variable. When set, it
wraps the *entire* `command` line in `su -m <user> -c …`, which means
`daemon(8)` itself runs as `_beastie` — and `_beastie` cannot write to
`/var/run/`. The startup fails with:

```
daemon: ppidfile ``/var/run/beastied.pid'': Permission denied
```

Sidestep: call the variable `beastied_runas` so `rc.subr` ignores it,
and pass `-u ${beastied_runas}` to `daemon(8)` instead. `daemon(8)`
writes the PID file as root, then drops privileges before exec'ing
`beastied`.

#### 3. Two pidfiles: supervisor (`-P`) and child (`-p`)

`daemon(8)` writes the supervisor PID to `-P /var/run/beastied.pid` (used by
`service status`/`stop`, per detail #1) **and** the child PID to
`-p /var/run/beastied_child.pid`. The `reload_cmd` sends `SIGHUP` to the child
PID so `service beastied reload` reaches `beastied` itself (config reload),
not the supervisor. `newsyslog` still signals the *supervisor* pidfile, which
makes `daemon(8)` reopen its `-o` log after a rotate — the two signals have
distinct targets and don't collide.

#### 4. Unix-domain socket listener

`listen` starting with `/` binds a Unix socket instead of TCP (main's
`listen()` helper removes any stale socket, `net.Listen("unix", …)`, then
`chmod 0660`). Useful for local-only access — e.g. an nginx `proxy_pass` to
the socket — with no TCP port exposed.

### Sample → publish loop

```
main:
    cfg     = config.Load(path)               // normalize() clamps bad values
    ring    = store.NewRing(cfg.Collect.RingSize)
    srv     = api.New(ring, webFS, cfg.Auth)
    if cfg.Store.Enabled():  srv.SetStore(store.OpenSQLite(path, store.Options{…}))
    buildAlerts(cfg.Alerts)                    // engine + event sink → store, srv.SetAlerts
    ln = listen(cfg.Server.Listen)            // TCP or Unix socket
    go http.Server{Handler: srv}.Serve(ln)
    go sampler.Run(sampCtx)                    // ticks → sampler.C
    // main select loop: sampler.C → Ingest + alerts.Eval; 1 s tick →
    // alerts.CheckStale; SIGHUP → reload; ctx.Done → srv.Close +
    // graceful Shutdown. See §12 for the full loop.
```

The buffered channel between sampler and main absorbs short JSON-encode
stalls; capacity 4 means up to 4 seconds of buffering before drops kick in.

---

## 14. Packaging

### `freebsd/+MANIFEST`

`pkg(8)` reads a UCL manifest:

```yaml
name: beastiemon
version: "%%VERSION%%"
origin: sysutils/beastiemon
prefix: /usr/local
files: {
  /usr/local/bin/beastied: "-",
  /usr/local/bin/beastie:  "-",
  /usr/local/etc/rc.d/beastied: "-",
  /usr/local/etc/beastiemon.conf.sample: "-",
  /usr/local/etc/newsyslog.conf.d/beastied.conf: "-",
}
scripts: {
  pre-install:  …   # pw groupadd/useradd, add to operator group
  post-install: …   # copy .conf.sample to .conf if missing
  post-deinstall: … # hint on how to remove the system user
}
```

**No `directories:` stanza.** `/var/log` is base-system; the rc.d
prestart hook creates the log file at runtime. If the manifest declared
ownership of `/var/log`, `pkg create` would refuse to build the package
because the path doesn't exist in the stage tree.

### Makefile pipeline

```
$ gmake VERSION=0.2.0 pkg
   ├── deps          (go mod download / tidy)
   ├── vendor-js     (download uPlot, rewrite index.html, patch assets.go)
   ├── build         (GOOS=freebsd GOARCH=amd64; produces beastied, beastie)
   ├── stage         (lay out .stage/ with bins, rc.d, conf.sample)
   └── pkg           (pkg create --format txz → .pkg/beastiemon-0.2.0.pkg)
```

### Install paths

| Source                             | Installed to                                   |
|------------------------------------|------------------------------------------------|
| `beastied`                         | `/usr/local/bin/beastied`                      |
| `beastie`                          | `/usr/local/bin/beastie`                       |
| `freebsd/beastied.in`              | `/usr/local/etc/rc.d/beastied`                 |
| `freebsd/beastiemon.conf`          | `/usr/local/etc/beastiemon.conf.sample`        |
| (post-install hook copies sample)  | `/usr/local/etc/beastiemon.conf` (only if absent) |
| `freebsd/newsyslog.conf`           | `/usr/local/etc/newsyslog.conf.d/beastied.conf` |

### System user

`pre-install`:

```sh
pw groupadd _beastie -g 874 2>/dev/null || :
pw useradd  _beastie -u 874 -g 874 \
    -d /nonexistent -s /usr/sbin/nologin -c "BeastieMon daemon" \
    2>/dev/null || :
pw groupmod operator -m _beastie 2>/dev/null || :   # devstat access
```

`post-deinstall` does **not** remove the user — uninstalling a user
that owns files on disk is dangerous. The hint is printed instead.

---

## 15. Security Model

### Posture

| Property                     | Stance                                  |
|------------------------------|-----------------------------------------|
| Authentication               | Optional built-in (`[auth]`), off by default. |
| Transport encryption         | None — by design. Use a proxy.          |
| Default bind                 | `127.0.0.1:8088`                        |
| Privileges                   | `_beastie` (uid/gid 874), nologin shell |
| Elevated capabilities        | `operator` group for `devstat(3)` only  |
| Disk writes                  | `/var/log/beastied.log`; `[store]` SQLite file if configured |
| Config readability           | `0640` root:_beastie                    |
| Log file                     | `0640` root:_beastie                    |
| PID file                     | `0644` root:wheel                       |
| Outbound network             | None, **unless `[alerts]` is set** — then webhook POSTs to the configured URL(s) |

`/metrics` and `/api/ws` sit behind the same `[auth]` gate as every other
endpoint except `/healthz`. Enabling `[store]` adds one writable path (the
SQLite file, which must be owned by `_beastie`); enabling `[alerts]` is the
only thing that makes `beastied` originate outbound connections, and only to
the operator-supplied webhook URLs.

### Optional authentication (`[auth]`)

Implemented as a single gate in `Server.ServeHTTP`, ahead of the mux (the
auth config is copied out under the server's RWMutex first, because SIGHUP
reload can swap it concurrently):

```go
s.mu.RLock()
auth := s.auth
s.mu.RUnlock()
if auth.Enabled() && r.URL.Path != "/healthz" && !authorized(r, auth) {
    if auth.BasicEnabled() {
        w.Header().Set("WWW-Authenticate", `Basic realm="BeastieMon"`)
    }
    http.Error(w, "unauthorized", http.StatusUnauthorized)
    return
}
```

Properties:

- **Off by default.** `AuthConfig.Enabled()` is false when all three
  fields are empty, so the gate is a no-op and behaviour is unchanged.
- **Two mechanisms, either suffices.** HTTP Basic (username+password)
  and a bearer token coexist; `authorized()` returns true if *any*
  configured credential matches. Basic makes browsers prompt natively
  (the `WWW-Authenticate` header is only sent when Basic is enabled, so
  token-only deployments don't trigger a useless browser prompt).
- **Constant-time comparison.** Both checks use
  `crypto/subtle.ConstantTimeCompare` so a matching prefix can't be
  recovered from response timing.
- **`?token=` escape hatch.** `EventSource` can't set headers, so the
  token is also accepted as a query parameter — at the documented cost
  of the token appearing in proxy/access logs.
- **`/healthz` is exempt** so liveness probes keep working unauthenticated.
- **Still no TLS.** Credentials cross the wire in clear text; built-in
  auth is access control for a trusted network, not a proxy replacement.

The gate is covered by table-driven tests in
`internal/api/server_test.go`: auth-disabled pass-through, Basic auth
(missing → 401 + challenge, wrong password → 401, correct → 200), bearer
token via header and `?token=`, the token-only path *not* emitting a
Basic challenge, and `/healthz` staying open.

### Threat model

- **Untrusted local user with shell.** Can read the dashboard via
  `127.0.0.1:8088` and see all metrics. The user could already see
  most of this via `ps`, `top`, `netstat` — BeastieMon doesn't expand
  their privileges. Process names in `procs[]` are world-readable on
  FreeBSD by default.
- **Untrusted remote attacker.** Cannot reach the daemon unless the
  operator changed `listen` from `127.0.0.1`. If they did, the operator
  is expected to put a reverse proxy in front. The README documents this.
- **Compromised `_beastie` account.** Limited to: read sysctl, run
  `gopsutil`'s syscalls, write `/var/log/beastied.log`. No outbound
  network. No persistence outside the log file. Config is `0640`,
  group-readable but not writable by `_beastie`.

### Things deliberately not done

- **CSRF protection** — no state-changing endpoints exist.
- **Rate limiting** — read-only endpoints; ring buffer ops are O(1)
  or O(N) on bounded N.
- **CORS lockdown** — `Access-Control-Allow-Origin: *` on the SSE
  endpoint only, to support reverse-proxy origin mismatch during
  development. Other endpoints don't set CORS headers; the dashboard
  is same-origin.

---

## 16. Performance Characteristics

Measured on FreeBSD 14.0 / amd64 / 4-core / SSD / 8 GB RAM at default
1 s sampling:

| Metric                      | Value         |
|-----------------------------|---------------|
| Steady-state RSS            | ~14 MB        |
| CPU at 1 s sampling         | ~0.3 % of one core |
| JSON encode per snapshot    | ~30 µs        |
| `ring.Since(15m)`           | ~50 µs (900 snapshots, no copy) |
| Broker publish              | ~3 µs per subscriber |
| Web first paint             | < 200 ms over LAN |
| SSE event → chart update    | ~5 ms         |
| Cold start to first sample  | 1 × interval (~1 s) |

### Dominant costs

1. **Per-process iteration** in `proc.go` — O(P) where P is the process
   count. On a 200-process box that's ~200 `Times()` calls per tick;
   each is one `kvm_getprocs(3)`-equivalent under `gopsutil`. This is
   the largest single cost; capping `top_procs` doesn't help because
   ranking requires Pass 1 over everything.
2. **Per-NIC + per-disk syscalls** for delta counters. Linear in
   `len(interfaces) + len(devices)`.
3. **Per-core CPU times** read in one call (`cpu.Times(true)` returns
   a single buffer).

### Scaling guidance

- `interval = "1s"` is the cheapest meaningful rate.
- `interval = "5s"` drops cost ~5× linearly.
- `interval = "500ms"` doubles cost but gives smoother live UX on slow
  hosts (where >1 sample/sec is rare anyway).
- `ring_size = 86400` (24h at 1s) uses ~170 MB and is rarely worth it
  — Prometheus is the right answer at that scale.

---

## 17. Error Handling Strategy

### Principle: degrade, don't fail

The daemon **never panics** during steady-state operation. Each
collector returns nil / zero values on failure:

```go
times, err := psutil.Times(true)
if err != nil || len(times) == 0 {
    return CPUStats{}      // empty struct; dashboard shows 0
}
```

This means a missing sensor (e.g. `coretemp` not loaded) produces an
empty `temps[]` array, the dashboard shows "No sensors detected",
everything else keeps working.

### Fatal paths

The daemon **does** call `log.Fatalf` for:

- Config parse error during startup (merely *nonsensical* values — a zero
  interval or ring size — are clamped to defaults instead, §11).
- Failure to open the SQLite history file when `[store]` is enabled.
- HTTP listen failure (port in use, address invalid).

The reasoning: these are operator misconfigurations, not transient
failures. Crashing fast surfaces them in `rc.d`'s output; `daemon(8) -r`
would auto-restart and you'd get crash loops without diagnostics
otherwise — so we let `rc.d` show the error once and stop.

### Client errors

| Condition                              | HTTP status |
|----------------------------------------|-------------|
| Unknown `metric` parameter             | 400 Bad Request |
| No data yet (`ring.Last()` empty)      | 503 Service Unavailable |
| Bad `range` parameter                  | falls back to default (15m) |
| SSE on a non-flusher writer            | 500 Internal Server Error |

---

## 18. Cross-Platform Story

The codebase is **primarily a FreeBSD project**, but the architecture
is portable.

### What's FreeBSD-only

- `internal/collect/temp.go`, `zfs.go`, `jail.go` (all `//go:build freebsd`),
  each stubbed by a `*_other.go` file on other OSes. Their pure parsers live
  in `bsdextra_parse.go` (no build tag) and are unit-tested everywhere.
- `freebsd/*` packaging (rc.d, manifest, conf path, newsyslog).
- `Makefile` uses BSD `sed -i ''` syntax in `vendor-js`.

### What's portable

- All other collectors use `gopsutil`, which supports Linux, macOS,
  Windows, FreeBSD.
- HTTP, SSE, WebSocket, embed, JSON: standard library + `gorilla/websocket`.
- SQLite history uses the **pure-Go** `modernc.org/sqlite`, so persistence is
  portable *and* CGO-free.
- Frontend: pure browser.

### Linux dev workflow

```sh
gmake build-native
./beastied -config freebsd/beastiemon.conf
```

CPU, memory, disk, network, filesystem, processes, persistence, alerts, and
the HTTP/WS/Prometheus surfaces all work on Linux. Temperatures, ZFS, and
jails are empty (their stubs return `nil`). Useful for iterating on the
dashboard and API without booting a FreeBSD VM.

---

## 19. Future Extensions

The architecture was built to make these additive — and most have now
shipped without restructuring, which is the evidence that the extension model
(typed `Snapshot`, transport-agnostic broker, `internal/` seams) works.

### Shipped

| Extension | Where it landed |
|-----------|-----------------|
| Optional web auth (`[auth]`, Basic + bearer) | `Server.ServeHTTP` gate — §15 |
| `--json` CLI mode | `cmd/beastie` — §10 |
| CLI TTY / `NO_COLOR` guard + `check` mode | `cmd/beastie` — §10 |
| Prometheus `/metrics` exporter | `api/prometheus.go` — §7 |
| WebSocket transport | `/api/ws`, shared broker — §8 |
| FS + proc history series | `/api/series` `fs`/`proc` — §7 |
| SQLite history (downsample + prune) | `store/sqlite.go` — §6 |
| ZFS pool + ARC, jail metrics | `collect/zfs.go`, `jail.go` — §5 |
| Threshold alerts + webhook | `internal/alert` — §11 |
| Graceful shutdown + SIGHUP reload | `cmd/beastied` — §12–13 |
| Unix-socket listener | `cmd/beastied` `listen()` — §13 |
| `newsyslog` log rotation | `freebsd/newsyslog.conf` — §14 |
| Dashboard light/dark theme | `web/` — §9 |
| Statistical roll-ups (avg + min/max band) | `store/rollup.go`, `/api/series?band=1` — §6, D18 |
| Alert re-notify, hysteresis, Slack/Discord payloads | `internal/alert` — §11, D17 |
| Alert state API + dashboard card + persisted events | `/api/alerts`, `Engine.States`/`Events`/`SetSink`, `store` `alert_events` table — §7, §11 |
| Sampler watchdog (`stale_after`) | `Engine.CheckStale` on a 1 s daemon tick — §11 |
| ZFS / ARC / jail history series | `/api/series` `zfs`/`arc`/`jail` — §7 |
| Tiered downsampling (coarse tier) | `store/sqlite.go` `coarsen` — §6, D19 |
| CLI remote mode (`--remote`, incl. Unix socket) | `cmd/beastie` `fetchRemote` — §10, D20 |
| `check net`/`check disk` | `cmd/beastie` `checkValue` — §10 |

### Still open

| Extension | Mechanism |
|-----------|-----------|
| Alert expressions | compound/multi-metric conditions and templated PagerDuty-style payloads (threshold, re-notify, hysteresis, watchdog, and state/event visibility have shipped) |
| Grafana dashboard | ship a `.json` built on the `/metrics` series |
| Per-jail CPU/mem | needs `rctl`/`kinfo_proc` `ki_jid`; the current jail panel is enumeration + process count only |

Deliberately **out of scope** (see §1 Non-Goals): in-daemon TLS (proxy's
job), a full rule engine beyond threshold alerts, and any fleet/federation
layer. The wire format and HTTP API haven't needed a breaking change for any
of the shipped work — all of it was additive at the data-model layer.

---

## 20. Design Decisions Log

A running record of decisions where the alternative isn't obvious.

### D1 — Go over Rust or C

- **Decision:** Go.
- **Why:** First-class FreeBSD/amd64+arm64 cross-compile from any host.
  `//go:embed` for single-binary assets. Standard library covers HTTP,
  SSE, JSON, TOML (one tiny dep), signals, contexts.
- **Trade-off:** ~10 MB binary vs ~3 MB Rust binary, ~14 MB RSS vs ~6 MB.
  Acceptable for the project's size.

### D2 — Single `Snapshot` struct, not per-metric streams

- **Decision:** One struct, all metrics, one timestamp.
- **Why:** Operators correlate metrics by time; storage stays trivial.
- **Trade-off:** When one collector is slow, all metrics are delayed.
  We accept this because the slowest collector (`proc`) is fast enough
  (<5 ms on a 200-process box).

### D3 — In-memory ring, no persistence

- **Decision:** RAM only.
- **Why:** Long-term storage is a different problem (TSDB territory).
  Keeps the daemon footprint and operational complexity minimal.
- **Trade-off:** Restart loses history. Future: Prometheus exporter.

### D4 — SSE, not WebSocket

- **Decision:** SSE.
- **Why:** One-way; proxy-friendly; built-in reconnect; debuggable
  with `curl`.

### D5 — Vanilla JS, no framework, no build step

- **Decision:** Pure browser JS.
- **Why:** The dashboard is small enough not to need React/Vue/Svelte;
  no Node toolchain on FreeBSD build hosts.
- **Trade-off:** ~600 lines of imperative DOM manipulation. We accept
  this.

### D6 — Custom ring buffer, not container/list or third-party

- **Decision:** 60-line bespoke struct.
- **Why:** Generic ring buffers either fight Go's type system or
  lock-per-element. Our needs are narrow.

### D7 — `procname=/usr/sbin/daemon` in rc.d script

- **Decision:** Match `procname` to whoever owns the pidfile, which is
  `daemon(8)` when `-P` is used.
- **Why:** Otherwise `service status` lies and `service stop` fails.

### D8 — `beastied_runas` instead of `beastied_user`

- **Decision:** Custom variable name.
- **Why:** Avoid `rc.subr`'s magic `${name}_user` handling which `su`s
  the whole command and breaks PID file writes.

### D9 — CLI does not use the HTTP API

- **Decision:** `beastie` imports `internal/collect` directly.
- **Why:** Works when the daemon doesn't; one code path for metrics.
- **Trade-off:** Two copies of the sampler running if both are active.
  Each is ~14 MB and ~0.3 % CPU — fine.

### D10 — Top-N processes ranked in two passes

- **Decision:** Pass 1 cheap (just CPU%); Pass 2 only for survivors.
- **Why:** `Name()`, `MemoryInfo()`, `MemoryPercent()` each cost a
  syscall; we don't want N×3 syscalls when we'll only display 5
  processes. Linear scan, top-N, then enrich.

### D11 — Non-blocking broker fan-out

- **Decision:** `select { case ch <- data: default: }` per subscriber.
- **Why:** A paused browser tab shouldn't back-pressure the sampler.
  Lost SSE events are recoverable via `/api/series`.

### D12 — No `directories:` stanza in `+MANIFEST`

- **Decision:** Let the rc.d prestart hook create the log file.
- **Why:** `pkg create` validates that every directory listed in the
  manifest exists in the stage tree. `/var/log` is base-system and
  shouldn't be owned by the package anyway.

### D13 — Pure-Go SQLite (`modernc.org/sqlite`), not cgo

- **Decision:** `modernc.org/sqlite` for `[store]`.
- **Why:** Keeps `CGO_ENABLED=0`, the static single binary, and the
  cross-compile-from-any-host story (G1) — a cgo driver (`mattn/go-sqlite3`)
  would break all three.
- **Trade-off:** Larger binary and a heavier dependency tree, and it raises
  the minimum Go toolchain (pinned to a modernc release whose floor is Go
  1.23). Worth it to hold the core invariant.

### D14 — Broker carries raw JSON; transports frame it

- **Decision:** `Ingest` publishes raw `Snapshot` JSON; SSE prepends
  `data: …`, WS sends a text frame.
- **Why:** Adding WebSocket (D#) shouldn't fork the fan-out. One payload, many
  transports — the broker stays transport-agnostic (D11 already made it
  content-agnostic).

### D15 — ZFS/jail collectors shell out, gated + parser-split

- **Decision:** `zpool`/`jls`/`ps` via `os/exec` (ARC via `kstat` sysctl),
  behind `[collect] zfs`/`jails` (default off), parsing split into a
  build-tag-free file.
- **Why:** `libzfs`/`jail_get(2)` mean cgo or fragile syscall marshaling;
  base-system tools are robust and keep CGO off. Gating avoids spawning
  subprocesses every tick on hosts that don't use them. Splitting the parser
  out lets it be unit-tested on Linux even though the collector is FreeBSD-only.

### D16 — Alerts fire webhooks asynchronously

- **Decision:** each fired event POSTs on its own goroutine; `post` is an
  injected func so tests observe events without HTTP.
- **Why:** a slow/hung webhook must never stall `Eval` (and thus the sampler).
  Mirrors the non-blocking philosophy of D11.

### D17 — Alert re-notify + hysteresis as per-rule state, formats at the edge

- **Decision:** `repeat` (re-notify cadence while firing) and `hysteresis`
  (recovery margin) live as extra fields on `ruleState`; the `Eval` state
  machine gained a `firing` branch that re-emits on the `repeat` clock and only
  resolves once `cleared()` (threshold ∓ hysteresis) is true. Payload shaping
  (`raw`/`slack`/`discord`) happens in `renderPayload` at the HTTP edge, not in
  the state machine — `post` carries the format alongside the `Event`.
- **Why:** hysteresis and re-notify are the two smallest knobs that cover the
  real operational failure modes (flapping at the boundary; a fired-once alert
  going unnoticed) without a general rule-expression language (a Non-Goal, §1).
  Keeping formatting a pure function of `(format, Event)` leaves the engine and
  its tests format-agnostic and makes the chat shapes trivially unit-testable.

### D18 — History roll-ups store avg + min/max, not one raw sample

- **Decision:** each `resolution` bucket is aggregated in memory into three
  snapshots (average, min, max) rather than keeping the first raw sample; the
  envelope rides in two nullable JSON columns and surfaces via
  `/api/series?band=1`.
- **Why:** downsampling to one raw sample per minute silently drops the spikes
  that motivate keeping history at all. The average gives a stable line; the
  min/max band restores the peaks — at 3× the JSON per row, negligible against
  the ~1-write/minute rate. Set-valued fields (procs/jails/ARC) aren't
  meaningfully averageable, so they carry from the last sample.

### D19 — Coarse tier merges rows in place, mean-of-means

- **Decision:** rows older than `coarse_after` are re-aggregated in place
  (delete N fine rows, insert one coarse row per `coarse_resolution` bucket)
  by the store's own writer goroutine, batch-limited and cursor-driven so a
  restart backlog drains without loading the whole tail into memory. The
  merged average is the mean of bucket means; min/max merge exactly.
- **Why:** no schema change, no second table, and readers (`Since`,
  `RollupSince`) stay oblivious — a coarse row is just a snapshot row. The
  mean-of-means skew is negligible because fine buckets hold near-equal
  sample counts (fixed interval, fixed resolution). Single-row buckets are
  skipped, so steady state does no work.
- **Trade-off:** exact weighted averages would need a sample-count column;
  not worth the migration for a monitoring trend line.

### D20 — CLI remote mode reuses `/api/metrics` and the config's `[auth]`

- **Decision:** `beastie --remote` fetches the daemon's latest snapshot over
  the existing API (TCP or the Unix socket) instead of growing a private
  RPC; credentials come from the same config file the daemon reads.
- **Why:** local sampling pays a full interval of warm-up per invocation and
  diverges from what the daemon sees (top-N ranking, FreeBSD extras);
  remote mode is instant, consistent, and works over SSH tunnels. One wire
  format (`Snapshot` JSON) keeps CLI and API output byte-identical.
- **Trade-off:** remote `check` depends on the daemon being up — which is
  itself signal: connection failure maps to nagios `UNKNOWN`, and the
  `stale_after` watchdog covers the wedged-sampler case from inside.
