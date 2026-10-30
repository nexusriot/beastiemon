'use strict';


function humanBytes(b) {
  if (b == null || b === 0) return '0 B';
  const units = ['B','KB','MB','GB','TB'];
  // Clamp at both ends. Rates are fractional (round2'd bytes/sec), so a value
  // under 1 B gives a negative exponent, and units[-1] is undefined — an idle
  // interface would label its y-axis "512.0 undefined/s".
  const i = Math.max(0, Math.min(Math.floor(Math.log2(b) / 10), units.length - 1));
  return (b / Math.pow(1024, i)).toFixed(1) + ' ' + units[i];
}

function humanDuration(secs) {
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  const s = Math.floor(secs % 60);
  if (d > 0) return `${d}d ${String(h).padStart(2,'0')}:${String(m).padStart(2,'0')}:${String(s).padStart(2,'0')}`;
  return `${String(h).padStart(2,'0')}:${String(m).padStart(2,'0')}:${String(s).padStart(2,'0')}`;
}


const COLORS = {
  red:    '#f85149',
  orange: '#d29922',
  green:  '#3fb950',
  blue:   '#58a6ff',
  purple: '#bc8cff',
  cyan:   '#39d3f2',
  muted:  '#8b949e',
};

// Read a CSS custom property so chart axis colours follow the active theme.
function cssVar(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

// mkAxes builds a fresh x/y axis pair from the current theme's CSS variables.
// Every chart gets its own pair: uPlot mutates the axis objects it is handed,
// so sharing one pair across charts lets them corrupt each other's state.
function mkAxes() {
  const stroke = cssVar('--muted', '#8b949e');
  const grid = cssVar('--border', '#30363d');
  const base = { stroke, grid: { stroke: grid, width: 1 }, ticks: { show: false }, font: '10px monospace' };
  return [{ ...base }, { ...base, size: 52 }];
}

// axesFor themes a chart's axes and installs its y-tick formatter. `size`
// widens the y gutter for labels longer than the 52px default — without it
// uPlot clips "1.0 GB/s" down to unreadable stubs like ",000,000".
function axesFor(fmt, size) {
  const [x, y] = mkAxes();
  if (fmt) y.values = (u, vals) => vals.map(v => v != null ? fmt(v) : '');
  if (size) y.size = size;
  return [x, y];
}

const baseOpts = {
  width:  100,
  height: 160,
  padding: [8, 0, 0, 0],
  cursor: { show: true, sync: { key: 'bm' } },
  select: { show: false },
  legend: { show: true, live: true },
};

// makeOpts merges a chart's options over the shared defaults. It must not
// clobber `extra.axes`: those carry the per-metric tick formatter (%, bytes,
// load), and overwriting them leaves every chart with raw unformatted numbers.
function makeOpts(extra) {
  const opts = Object.assign({}, baseOpts, extra);
  opts.axes = extra.axes || mkAxes();
  opts.cursor = baseOpts.cursor;
  return opts;
}

// Resize observer to make charts fill their container.
function autoresize(chart, el) {
  const ro = new ResizeObserver(() => {
    chart.setSize({ width: el.clientWidth, height: chart.height });
  });
  ro.observe(el);
  return ro;
}


const state = {
  range: '15m',
  charts: {},
  resizers: {},
  netIface: null,
  diskDev: null,
  latestSnap: null,
  // paused freezes the whole live view so a spike can be read and exported
  // instead of scrolling off the left edge. The SSE connection stays open,
  // so resuming is immediate.
  paused: false,
};


function buildCPUChart(data) {
  const el = document.getElementById('chart-cpu');
  if (state.charts.cpu) { state.charts.cpu.destroy(); }

  const opts = makeOpts({
    height: 180,
    series: [
      {},
      { label: 'User',  stroke: COLORS.blue,   fill: 'rgba(88,166,255,0.15)',  width: 1.5 },
      { label: 'Sys',   stroke: COLORS.orange,  fill: 'rgba(210,153,34,0.15)', width: 1.5 },
      { label: 'Idle',  stroke: COLORS.muted,   fill: 'rgba(139,148,158,0.07)',width: 1 },
      { label: 'Total', stroke: COLORS.red,     width: 2, dash: [4,2] },
      // Total min/max envelope from /api/series?band=1. Full-resolution (live)
      // points have min=max=total so the band collapses onto the line; on wide
      // ranges the rolled-up buckets spread it into a spike envelope.
      { label: 'min', stroke: 'rgba(248,81,73,0.35)', width: 0.5, points: { show: false } },
      { label: 'max', stroke: 'rgba(248,81,73,0.35)', width: 0.5, points: { show: false } },
    ],
    bands: [{ series: [6, 5], fill: 'rgba(248,81,73,0.10)' }],
    axes: axesFor(v => v.toFixed(0) + '%'),
    scales: { y: { range: [0, 100] } },
  });

  state.charts.cpu = new uPlot(opts, data, el);
  if (state.resizers.cpu) state.resizers.cpu.disconnect();
  state.resizers.cpu = autoresize(state.charts.cpu, el);
}

function buildLoadChart(data) {
  const el = document.getElementById('chart-load');
  if (state.charts.load) state.charts.load.destroy();

  const opts = makeOpts({
    height: 120,
    series: [
      {},
      { label: '1m',  stroke: COLORS.red,    width: 2 },
      { label: '5m',  stroke: COLORS.orange, width: 1.5 },
      { label: '15m', stroke: COLORS.muted,  width: 1 },
    ],
    axes: axesFor(v => v.toFixed(2)),
  });

  state.charts.load = new uPlot(opts, data, el);
  if (state.resizers.load) state.resizers.load.disconnect();
  state.resizers.load = autoresize(state.charts.load, el);
}

function buildMemChart(data) {
  const el = document.getElementById('chart-mem');
  if (state.charts.mem) state.charts.mem.destroy();

  const opts = makeOpts({
    height: 180,
    series: [
      {},
      { label: 'Used',      stroke: COLORS.red,    fill: 'rgba(248,81,73,0.2)',  width: 1.5 },
      { label: 'Free',      stroke: COLORS.green,  fill: 'rgba(63,185,80,0.12)', width: 1.5 },
      { label: 'Swap Used', stroke: COLORS.orange, width: 1.5, dash: [4,2] },
    ],
    axes: axesFor(humanBytes, 66),
  });

  state.charts.mem = new uPlot(opts, data, el);
  if (state.resizers.mem) state.resizers.mem.disconnect();
  state.resizers.mem = autoresize(state.charts.mem, el);
}

function buildNetChart(data) {
  const el = document.getElementById('chart-net');
  if (state.charts.net) state.charts.net.destroy();

  const opts = makeOpts({
    height: 160,
    series: [
      {},
      { label: 'RX', stroke: COLORS.green, fill: 'rgba(63,185,80,0.12)', width: 1.5 },
      { label: 'TX', stroke: COLORS.blue,  fill: 'rgba(88,166,255,0.1)', width: 1.5 },
    ],
    axes: axesFor(v => humanBytes(v) + '/s', 74),
  });

  state.charts.net = new uPlot(opts, data, el);
  if (state.resizers.net) state.resizers.net.disconnect();
  state.resizers.net = autoresize(state.charts.net, el);
}

function buildDiskChart(data) {
  const el = document.getElementById('chart-disk');
  if (state.charts.disk) state.charts.disk.destroy();

  const opts = makeOpts({
    height: 160,
    series: [
      {},
      { label: 'Read',  stroke: COLORS.cyan,   fill: 'rgba(57,211,242,0.1)', width: 1.5 },
      { label: 'Write', stroke: COLORS.purple,  fill: 'rgba(188,140,255,0.1)',width: 1.5 },
    ],
    axes: axesFor(v => humanBytes(v) + '/s', 74),
  });

  state.charts.disk = new uPlot(opts, data, el);
  if (state.resizers.disk) state.resizers.disk.disconnect();
  state.resizers.disk = autoresize(state.charts.disk, el);
}

// buildDiskLatChart plots busy % on the left axis and per-request latency on
// the right: they answer the same question ("is this disk the bottleneck?")
// in units that share no scale. With no device selected each point is the
// worst device, matching /api/series?metric=disklat.
function buildDiskLatChart(data) {
  const el = document.getElementById('chart-disklat');
  if (state.charts.disklat) state.charts.disklat.destroy();

  const [x, y] = axesFor(v => v.toFixed(0) + '%');
  const [, yr] = axesFor(v => v.toFixed(1) + ' ms', 60);
  y.scale = '%';
  yr.scale = 'ms';
  yr.side = 1;
  yr.grid = { show: false };

  const opts = makeOpts({
    height: 160,
    scales: { '%': { range: [0, 100] }, ms: { range: (u, lo, hi) => [0, Math.max(1, hi)] } },
    series: [
      {},
      { label: 'Busy',    scale: '%',  stroke: COLORS.orange, fill: 'rgba(210,153,34,0.12)', width: 1.5 },
      { label: 'Read ms', scale: 'ms', stroke: COLORS.cyan,   width: 1.5 },
      { label: 'Write ms', scale: 'ms', stroke: COLORS.purple, width: 1.5 },
    ],
    axes: [x, y, yr],
  });

  state.charts.disklat = new uPlot(opts, data, el);
  if (state.resizers.disklat) state.resizers.disklat.disconnect();
  state.resizers.disklat = autoresize(state.charts.disklat, el);
}


async function fetchSeries(metric, extra = '') {
  const r = await fetch(`/api/series?metric=${metric}&range=${state.range}${extra}`);
  if (!r.ok) throw new Error(r.statusText);
  return r.json();
}

async function loadCPU() {
  // cores=0: the per-core columns (and their band pairs) are not plotted
  // here — the live strip under the chart renders them from the stream —
  // so on a 16-core box this drops 48 unused columns per range change.
  const d = await fetchSeries('cpu', '&band=1&cores=0');
  const col = {};
  d.labels.forEach((l, i) => { col[l] = d.data[i]; });
  const n = (col.ts || []).length;
  const pick = k => col[k] || new Array(n).fill(null);
  buildCPUChart([col.ts || [], pick('user'), pick('sys'), pick('idle'),
    pick('total'), pick('total_min'), pick('total_max')]);
}
async function loadLoad() { const d = await fetchSeries('load'); buildLoadChart(d.data); }
async function loadMem()  { const d = await fetchSeries('mem');  buildMemChart(d.data); }

async function loadNet(iface = '') {
  const extra = iface ? `&iface=${encodeURIComponent(iface)}` : '';
  const d = await fetchSeries('net', extra);
  buildNetChart(d.data);
}

async function loadDisk(dev = '') {
  const extra = dev ? `&dev=${encodeURIComponent(dev)}` : '';
  const [d, lat] = await Promise.all([fetchSeries('disk', extra), fetchSeries('disklat', extra)]);
  buildDiskChart(d.data);
  buildDiskLatChart(lat.data);
}

async function loadAll() {
  await Promise.allSettled([loadCPU(), loadLoad(), loadMem(),
    loadNet(state.netIface), loadDisk(state.diskDev)]);
}


function appendToChart(chart, ts, values) {
  if (!chart) return;
  const data = chart.data;
  // uPlot requires a strictly ascending x series (its cursor and range code
  // binary-search it). The newest row seeded from /api/series is the same
  // sample the first live frame carries, so drop anything that doesn't
  // advance the timeline rather than plotting it twice.
  const last = data[0][data[0].length - 1];
  if (last != null && ts <= last) return;
  // Append to each series.
  data[0].push(ts);
  for (let i = 0; i < values.length; i++) {
    if (data[i + 1]) data[i + 1].push(values[i] ?? null);
  }
  // Trim to keep only points within selected range.
  const cutoff = ts - rangeSeconds();
  while (data[0].length > 1 && data[0][0] < cutoff) {
    for (const s of data) s.shift();
  }
  chart.setData(data);
}

function rangeSeconds() {
  const s = state.range;
  if (s.endsWith('m')) return parseInt(s) * 60;
  if (s.endsWith('h')) return parseInt(s) * 3600;
  return 900;
}

function applyLiveSnap(snap) {
  // Frozen: drop the frame rather than buffering it. Catching up on resume
  // would replay a burst of points the operator never asked to see, and the
  // seeded history is re-fetched by loadAll() anyway.
  if (state.paused) return;
  state.latestSnap = snap;
  // Seconds with the fraction kept, matching the timestamps /api/series
  // returns, so seeded history and live points share one grid. Flooring here
  // would make every sample inside one second collide, and appendToChart
  // drops anything that does not advance the timeline — at interval = "500ms"
  // that silently threw away every other live point.
  const ts = new Date(snap.ts).getTime() / 1000;

  // CPU summary
  const cpuPct = snap.cpu?.total ?? 0;
  const el = document.getElementById('cpu-pct');
  if (el) {
    el.textContent = cpuPct.toFixed(1) + '%';
    el.style.color = cpuPct >= 90 ? COLORS.red : cpuPct >= 70 ? COLORS.orange : COLORS.green;
  }
  appendToChart(state.charts.cpu, ts,
    [snap.cpu?.user, snap.cpu?.sys, snap.cpu?.idle, snap.cpu?.total,
     snap.cpu?.total, snap.cpu?.total]);

  // Load
  document.getElementById('load1').textContent  = snap.load?.load1?.toFixed(2)  ?? '—';
  document.getElementById('load5').textContent  = snap.load?.load5?.toFixed(2)  ?? '—';
  document.getElementById('load15').textContent = snap.load?.load15?.toFixed(2) ?? '—';
  appendToChart(state.charts.load, ts,
    [snap.load?.load1, snap.load?.load5, snap.load?.load15]);

  // Memory
  const memPct = snap.mem?.used_pct ?? 0;
  const mp = document.getElementById('mem-pct');
  if (mp) {
    mp.textContent = memPct.toFixed(1) + '%';
    // The normal colour is the theme's text colour (what .card-val uses), read
    // live so it follows the light/dark toggle. It must be a real value: the
    // CSSOM silently ignores a style assignment of undefined, so a missing one
    // would leave the figure stuck on its last warning colour after recovery.
    mp.style.color = memPct >= 90 ? COLORS.red
      : memPct >= 70 ? COLORS.orange
      : cssVar('--text', '#e6edf3');
  }
  appendToChart(state.charts.mem, ts,
    [snap.mem?.used, snap.mem?.free, snap.mem?.swap_used]);

  // Net — accumulate all ifaces or filter by selected
  let rx = 0, tx = 0;
  for (const n of (snap.net || [])) {
    if (!state.netIface || n.iface === state.netIface) {
      rx += n.rx_bps ?? 0;
      tx += n.tx_bps ?? 0;
    }
  }
  appendToChart(state.charts.net, ts, [rx, tx]);

  // Disk
  let rd = 0, wr = 0, busy = 0, rms = 0, wms = 0;
  for (const d of (snap.disk || [])) {
    if (!state.diskDev || d.dev === state.diskDev) {
      rd += d.read_bps ?? 0;
      wr += d.write_bps ?? 0;
      busy = Math.max(busy, d.busy_pct ?? 0);
      rms = Math.max(rms, d.read_ms ?? 0);
      wms = Math.max(wms, d.write_ms ?? 0);
    }
  }
  appendToChart(state.charts.disk, ts, [rd, wr]);
  appendToChart(state.charts.disklat, ts, [busy, rms, wms]);

  // Uptime
  const uEl = document.getElementById('uptime-line');
  if (uEl && snap.uptime) uEl.textContent = 'up ' + humanDuration(snap.uptime);

  // Temperatures
  renderTemps(snap.temps || []);

  // Filesystems
  renderFS(snap.fs || []);

  // Top processes
  renderProcs(snap.procs || []);

  // ZFS pools / ARC and jails (FreeBSD-only; cards hide when absent).
  renderZFS(snap.zfs || [], snap.arc);
  renderJails(snap.jails || []);
  renderVMs(snap.vms || []);
  renderHealth(snap.health);

  // Per-core distribution, pf, and watched processes.
  renderCores(snap.cpu?.per_core || []);
  renderPF(snap.pf);
  renderWatch(snap.watch || []);

  // Build iface / dev tabs if not yet built.
  buildIfaceTabs(snap.net || [], snap.disk || []);
}

// renderCores draws one small bar per core. This is deliberately a live-only
// view: per-core history exists at /api/series?metric=cpu but costs a column
// per core, and what an operator wants from it — "is one core pinned while
// the rest idle?" — is a question about right now.
function renderCores(cores) {
  const el = document.getElementById('cpu-cores');
  if (!el) return;
  if (!cores.length) { el.innerHTML = ''; return; }
  el.innerHTML = cores.map((v, i) => {
    const cls = v >= 90 ? 'crit' : v >= 70 ? 'warn' : '';
    return `<div class="core-cell">
      <div class="core-label"><span>cpu${i}</span><span>${v.toFixed(0)}%</span></div>
      <div class="core-track"><div class="core-fill ${cls}" style="width:${Math.max(0, Math.min(100, v)).toFixed(1)}%"></div></div>
    </div>`;
  }).join('');
}

function renderPF(pf) {
  const card = document.getElementById('card-pf');
  if (!card) return;
  if (!pf) { card.style.display = 'none'; return; }
  card.style.display = '';

  const head = document.getElementById('pf-states');
  if (head) {
    head.textContent = pf.enabled ? `${pf.states} states` : 'disabled';
    head.style.color = pf.enabled ? cssVar('--text', '#e6edf3') : COLORS.orange;
  }

  const rows = [];
  if (pf.state_limit) {
    // State-table utilisation is the number worth watching: exhausting it
    // drops connections with nothing in the logs to explain it.
    const cls = pf.state_pct >= 90 ? 'crit' : pf.state_pct >= 75 ? 'warn' : '';
    rows.push(['state table', `${pf.states} / ${pf.state_limit} (${(pf.state_pct ?? 0).toFixed(1)}%)`, cls]);
  }
  rows.push(['match', `${(pf.match_rate ?? 0).toFixed(0)}/s`, '']);
  rows.push(['searches', `${(pf.search_rate ?? 0).toFixed(0)}/s`, '']);
  rows.push(['inserts', `${(pf.insert_rate ?? 0).toFixed(0)}/s`, '']);
  rows.push(['drops (memory/state-limit/congestion)',
    `${(pf.drop_rate ?? 0).toFixed(0)}/s`, pf.drop_rate > 0 ? 'crit' : '']);

  document.getElementById('pf-grid').innerHTML = rows.map(([k, v, cls]) =>
    `<div class="pf-row"><span class="pf-key">${escapeHtml(k)}</span><span class="pf-val ${cls}">${escapeHtml(v)}</span></div>`
  ).join('');
}

function renderWatch(watch) {
  const el = document.getElementById('watch-grid');
  if (!el) return;
  el.innerHTML = watch.map(w => {
    const up = w.count > 0;
    return `<div class="watch-row">
      <span>${escapeHtml(w.name)}</span>
      <span class="watch-state ${up ? 'up' : 'down'}">${up ? `running (${w.count})` : 'NOT RUNNING'}</span>
    </div>`;
  }).join('');
}

function renderZFS(pools, arc) {
  const card = document.getElementById('card-zfs');
  if (!card) return;
  if (!pools.length) { card.style.display = 'none'; return; }
  card.style.display = '';

  const arcEl = document.getElementById('arc-val');
  if (arcEl) arcEl.textContent = arc
    ? `ARC ${humanBytes(arc.size)} · ${arc.hit_rate.toFixed(0)}% hit`
    : '';

  document.getElementById('zfs-grid').innerHTML = pools.map(p => {
    const cls = p.used_pct >= 90 ? 'crit' : p.used_pct >= 75 ? 'warn' : '';
    const health = p.health || '';
    const healthCls = health && health !== 'ONLINE' ? 'crit' : '';
    return `<div class="fs-row">
      <div class="fs-header">
        <span class="fs-mount">${escapeHtml(p.pool)}
          <span class="fs-info ${healthCls}">${escapeHtml(health)}</span></span>
        <span class="fs-info">${humanBytes(p.alloc)} / ${humanBytes(p.size)} (${p.used_pct.toFixed(1)}%)</span>
      </div>
      <div class="fs-bar-track"><div class="fs-bar-fill ${cls}" style="width:${p.used_pct.toFixed(1)}%"></div></div>
      ${scanLine(p)}
    </div>`;
  }).join('');

  renderDatasets();
}

// scanLine summarises a pool's scrub/resilver state. A pool that has never
// been scrubbed, or one whose last scrub is months old, is flagged: silent
// bit rot is exactly what scrubbing exists to catch.
function scanLine(p) {
  if (!p.scan) return '';
  if (p.scan === 'scrub' || p.scan === 'resilver') {
    return `<div class="fs-scan">${p.scan} in progress — ${(p.scan_pct ?? 0).toFixed(1)}% done</div>`;
  }
  if (p.scan === 'none') {
    return '<div class="fs-scan warn">never scrubbed</div>';
  }
  if (!p.scan_known) return '';
  const hours = p.scan_age_hours ?? 0;
  const age = hours >= 48 ? `${Math.round(hours / 24)} days` : `${Math.round(hours)} hours`;
  const label = p.scan === 'resilver_done' ? 'resilvered' : 'scrubbed';
  // 35 days: a month of monthly-scrub schedule plus slack.
  return `<div class="fs-scan ${hours > 35 * 24 ? 'warn' : ''}">${label} ${age} ago</div>`;
}

function renderDatasets() {
  const el = document.getElementById('dataset-grid');
  if (!el) return;
  const sets = state.latestSnap?.datasets || [];
  el.innerHTML = sets.map(d => `<div class="dataset-row">
    <span class="ds-name">${escapeHtml(d.name)}</span>
    <span>used ${humanBytes(d.used)} · avail ${humanBytes(d.avail)}</span>
  </div>`).join('');
}

function renderJails(jails) {
  const card = document.getElementById('card-jails');
  if (!card) return;
  if (!jails.length) { card.style.display = 'none'; return; }
  card.style.display = '';

  const rows = jails.map(j => `<tr>
    <td class="proc-pid">${j.jid}</td>
    <td class="proc-name">${escapeHtml(j.name)}</td>
    <td class="proc-name">${escapeHtml(j.host || '')}</td>
    <td class="proc-num">${j.procs}</td>
  </tr>`).join('');
  document.getElementById('jails-grid').innerHTML = `<table class="proc-table">
    <thead><tr><th>JID</th><th>Name</th><th>Hostname</th><th class="proc-num">Procs</th></tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

function renderVMs(vms) {
  const card = document.getElementById('card-vms');
  if (!card) return;
  if (!vms.length) { card.style.display = 'none'; return; }
  card.style.display = '';

  const rows = vms.map(v => {
    if (v.state !== 'running') {
      // A vmm instance with no bhyve behind it still holds the guest's
      // wired memory until someone destroys it.
      return `<tr>
        <td class="proc-name">${escapeHtml(v.name)}</td>
        <td class="proc-cpu crit" colspan="4">orphaned — bhyvectl --destroy --vm=${escapeHtml(v.name)}</td>
      </tr>`;
    }
    const cls = v.cpu_pct >= 90 ? 'crit' : v.cpu_pct >= 70 ? 'warn' : '';
    return `<tr>
      <td class="proc-name">${escapeHtml(v.name)}</td>
      <td class="proc-pid">${v.pid}</td>
      <td class="proc-num proc-cpu ${cls}">${(v.cpu_pct ?? 0).toFixed(1)}%</td>
      <td class="proc-num">${humanBytes(v.rss)}</td>
      <td class="proc-num">${humanDuration(v.uptime_sec || 0)}</td>
    </tr>`;
  }).join('');
  document.getElementById('vms-grid').innerHTML = `<table class="proc-table">
    <thead><tr><th>Name</th><th>PID</th><th class="proc-num">CPU</th><th class="proc-num">RSS</th><th class="proc-num">Uptime</th></tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

function ageLabel(hours) {
  return hours >= 48 ? `${Math.round(hours / 24)}d` : `${Math.round(hours)}h`;
}

// renderHealth shows the slow host-health check. Each part renders only once
// it has a result, and "unknown" is said out loud rather than shown as green.
function renderHealth(h) {
  const card = document.getElementById('card-health');
  if (!card) return;
  if (!h) { card.style.display = 'none'; return; }
  card.style.display = '';

  const rows = [];
  if (!h.vuln_known) {
    rows.push(['vulnerable packages', 'unknown (no vuln.xml)', 'warn']);
  } else {
    rows.push(['vulnerable packages', String(h.vulnerable), h.vulnerable > 0 ? 'crit' : 'ok']);
    for (const p of (h.vuln_pkgs || [])) rows.push(['', p, 'sub']);
    if (h.vulnerable > (h.vuln_pkgs || []).length) {
      rows.push(['', `… ${h.vulnerable - h.vuln_pkgs.length} more`, 'sub']);
    }
    // periodic(8) refreshes vuln.xml daily; much older means it stopped.
    rows.push(['vuln.xml age', ageLabel(h.vulndb_age_hours || 0), h.vulndb_age_hours > 72 ? 'warn' : '']);
  }
  if (h.kernel_known) {
    rows.push(['kernel', h.reboot_required
      ? `reboot required (${h.running_kernel} → ${h.installed_kernel})`
      : h.running_kernel, h.reboot_required ? 'warn' : 'ok']);
    if (h.userland) rows.push(['userland', h.userland, '']);
  }
  if (h.be_count) {
    const pending = h.be_next && h.be_next !== h.be_active;
    rows.push(['boot environment', `${h.be_active} (${h.be_count} total)`, '']);
    if (pending) rows.push(['next boot', h.be_next, 'warn']);
  }
  const ago = Math.max(0, Date.now() / 1000 - Date.parse(h.checked_at) / 1000);
  rows.push(['checked', humanDuration(ago) + ' ago', '']);

  document.getElementById('health-grid').innerHTML = rows.map(([k, v, cls]) =>
    `<div class="pf-row"><span class="pf-key">${escapeHtml(k)}</span><span class="pf-val ${cls}">${escapeHtml(v)}</span></div>`
  ).join('');
}


function renderTemps(temps) {
  const grid = document.getElementById('temp-grid');
  if (!temps.length) {
    grid.innerHTML = '<div style="color:var(--muted)">No sensors detected.<br>Load <code>coretemp</code> or <code>amdtemp</code>.</div>';
    return;
  }
  grid.innerHTML = temps.map(t => {
    const pct = Math.min(100, Math.max(0, (t.celsius - 20) / 80 * 100));
    const cls = t.celsius >= 80 ? 'hot' : t.celsius >= 65 ? 'warm' : 'ok';
    return `<div class="temp-row">
      <span class="temp-name">${escapeHtml(t.name)}</span>
      <div class="temp-bar-track"><div class="temp-bar-fill temp-${cls}" style="width:${pct.toFixed(1)}%"></div></div>
      <span class="temp-val temp-${cls}">${t.celsius.toFixed(1)}°C</span>
    </div>`;
  }).join('');
}

function renderFS(fsList) {
  const grid = document.getElementById('fs-grid');
  grid.innerHTML = fsList.map(f => {
    const cls = f.used_pct >= 90 ? 'crit' : f.used_pct >= 75 ? 'warn' : '';
    return `<div class="fs-row">
      <div class="fs-header">
        <span class="fs-mount">${escapeHtml(f.mount)}</span>
        <span class="fs-info">${humanBytes(f.used)} / ${humanBytes(f.total)} (${f.used_pct.toFixed(1)}%)</span>
      </div>
      <div class="fs-bar-track">
        <div class="fs-bar-fill ${cls}" style="width:${f.used_pct.toFixed(1)}%"></div>
      </div>
    </div>`;
  }).join('');
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function renderProcs(procs) {
  const el = document.getElementById('proc-table');
  if (!el) return;
  if (!procs.length) {
    el.innerHTML = '<div style="color:var(--muted)">No data yet — needs two samples.</div>';
    return;
  }
  const rows = procs.map(p => {
    const cls = p.cpu_pct >= 90 ? 'crit' : p.cpu_pct >= 50 ? 'warn' : '';
    return `<tr>
      <td class="proc-pid">${p.pid}</td>
      <td class="proc-name">${escapeHtml(p.name)}</td>
      <td class="proc-num proc-cpu ${cls}">${p.cpu_pct.toFixed(1)}%</td>
      <td class="proc-num">${p.mem_pct.toFixed(1)}%</td>
      <td class="proc-num">${humanBytes(p.rss)}</td>
    </tr>`;
  }).join('');
  el.innerHTML = `<table class="proc-table">
    <thead><tr>
      <th>PID</th><th>Command</th>
      <th class="proc-num">CPU</th><th class="proc-num">MEM</th><th class="proc-num">RSS</th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

let tabsBuilt = false;
function renderTabButtons(el, names, current) {
  el.innerHTML = ['(all)', ...names].map((name, i) => {
    const val = i === 0 ? '' : name;
    const active = val === (current ?? '') ? 'active' : '';
    return `<button class="tab-btn ${active}" data-val="${escapeHtml(val)}">${escapeHtml(name)}</button>`;
  }).join('');
}

function buildIfaceTabs(nets, disks) {
  if (tabsBuilt) return;
  tabsBuilt = true;

  const ifaces = [...new Set(nets.map(n => n.iface))].sort();
  if (ifaces.length > 1) {
    renderTabButtons(document.getElementById('iface-tabs'), ifaces, state.netIface);
  }
  const devs = [...new Set(disks.map(d => d.dev))].sort();
  if (devs.length > 1) {
    renderTabButtons(document.getElementById('dev-tabs'), devs, state.diskDev);
  }
}

// bindTabClicks attaches one delegated click handler per tab strip, once at
// init. The buttons themselves are rebuilt by buildIfaceTabs after each range
// change; re-adding the listener there would stack a duplicate per rebuild.
function bindTabClicks(id, apply) {
  const el = document.getElementById(id);
  el.addEventListener('click', e => {
    if (!e.target.matches('.tab-btn')) return;
    el.querySelectorAll('.tab-btn').forEach(b => b.classList.toggle('active', b === e.target));
    apply(e.target.dataset.val || null);
  });
}


// loadAlerts polls /api/alerts (rule states change on the daemon's schedule,
// not the snapshot stream, so a slow poll is enough). The card stays hidden
// until alerts are configured or history exists.
async function loadAlerts() {
  let d;
  try {
    const r = await fetch('/api/alerts');
    if (!r.ok) return;
    d = await r.json();
  } catch { return; }
  renderAlerts(d);
}

function renderAlerts(d) {
  const card = document.getElementById('card-alerts');
  if (!card) return;
  const rules = d.rules || [], events = d.events || [];
  if (!d.enabled && !events.length) { card.style.display = 'none'; return; }
  card.style.display = '';

  const firing = rules.filter(r => r.state === 'firing').length;
  const sum = document.getElementById('alerts-summary');
  sum.textContent = firing ? `${firing} firing` : 'all clear';
  sum.style.color = firing ? COLORS.red : COLORS.green;

  const condition = r => {
    const field = r.field ? `.${r.field}` : '';
    return `${r.metric}${field} ${r.op} ${r.threshold}`;
  };
  document.getElementById('alerts-rules').innerHTML = rules.length ? `<table class="proc-table">
    <thead><tr><th>Rule</th><th>Condition</th><th>State</th><th class="proc-num">Value</th></tr></thead>
    <tbody>${rules.map(r => `<tr>
      <td class="proc-name">${escapeHtml(r.name || r.metric)}</td>
      <td class="proc-pid">${escapeHtml(condition(r))}</td>
      <td><span class="alert-badge alert-${r.state}">${r.state}</span></td>
      <td class="proc-num">${(r.value ?? 0).toFixed(2)}</td>
    </tr>`).join('')}</tbody>
  </table>` : '<div style="color:var(--muted)">No rules configured.</div>';

  const evTitle = document.getElementById('alerts-events-title');
  evTitle.style.display = events.length ? '' : 'none';
  document.getElementById('alerts-events').innerHTML = events.slice(0, 10).map(ev => `
    <div class="alert-event">
      <span class="ev-time">${new Date(ev.ts).toLocaleString()}</span>
      <span class="ev-state ${escapeHtml(ev.state)}">${escapeHtml(ev.state)}</span>
      <span class="ev-what">${escapeHtml(ev.rule || ev.metric)}: ${escapeHtml(ev.metric)}${ev.field ? '.' + escapeHtml(ev.field) : ''} = ${(ev.value ?? 0).toFixed(2)} (${escapeHtml(ev.op)} ${ev.threshold})</span>
    </div>`).join('');
}

function connectSSE() {
  const dot = document.getElementById('live-dot');
  const es  = new EventSource('/api/stream');

  es.onopen = () => dot.classList.remove('dead');
  es.onerror = () => {
    dot.classList.add('dead');
    es.close();
    setTimeout(connectSSE, 5000);
  };
  es.onmessage = e => {
    try { applyLiveSnap(JSON.parse(e.data)); } catch {}
  };
}


// ---- Freeze & export ------------------------------------------------

function setPaused(on) {
  state.paused = on;
  document.body.classList.toggle('paused', on);
  const btn = document.getElementById('pause-toggle');
  if (btn) {
    btn.textContent = on ? '▶' : '⏸';
    btn.classList.toggle('paused', on);
    btn.title = on ? 'Resume live updates' : 'Freeze the live view';
  }
  // Resuming re-seeds from history so the gap that accrued while frozen is
  // filled in, rather than leaving a flat line across it.
  if (!on) loadAll();
}

// exportCSV downloads what a chart currently shows. The point is the visible
// range: whatever you froze and were looking at is what lands in the file.
function exportCSV(key) {
  const chart = state.charts[key];
  if (!chart || !chart.data || !chart.data[0]) return;
  const data = chart.data;
  const headers = ['time', ...chart.series.slice(1).map(s => s.label || '')];
  const rows = [headers.join(',')];
  for (let i = 0; i < data[0].length; i++) {
    const cells = [new Date(data[0][i] * 1000).toISOString()];
    for (let c = 1; c < data.length; c++) {
      const v = data[c][i];
      cells.push(v == null ? '' : v);
    }
    rows.push(cells.join(','));
  }
  const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
  downloadBlob(`beastiemon-${key}-${stamp}.csv`, rows.join('\n'));
}

function downloadBlob(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/csv' }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoke on the next turn of the event loop: Safari aborts the download
  // if the object URL goes away in the same tick as the click.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  const btn = document.getElementById('theme-toggle');
  if (btn) btn.textContent = t === 'light' ? '☀' : '☾';
}

// initTheme picks localStorage first, else the OS preference, defaulting dark.
function initTheme() {
  const saved = localStorage.getItem('theme');
  const prefersLight = window.matchMedia &&
    window.matchMedia('(prefers-color-scheme: light)').matches;
  applyTheme(saved || (prefersLight ? 'light' : 'dark'));
}

function toggleTheme() {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  localStorage.setItem('theme', next);
  applyTheme(next);
  loadAll(); // rebuild charts so axis colours match the new theme
}

async function init() {
  // Theme before charts so the first render uses the right axis colours.
  initTheme();

  // Host info
  try {
    const h = await fetch('/api/host').then(r => r.json());
    const hl = document.getElementById('host-line');
    if (hl) hl.textContent = `${h.hostname}  •  ${h.platform} ${h.platformVersion}  •  ${h.kernelVersion}`;
  } catch {}

  // Seed charts from history
  await loadAll();

  // Alert states: initial load + slow poll.
  loadAlerts();
  setInterval(loadAlerts, 10000);

  // Connect live stream
  connectSSE();

  // Range selector
  document.getElementById('range-select').addEventListener('change', e => {
    state.range = e.target.value;
    tabsBuilt = false;
    loadAll();
  });

  // Tab strips: one delegated listener each, for the lifetime of the page.
  bindTabClicks('iface-tabs', v => { state.netIface = v; loadNet(v); });
  bindTabClicks('dev-tabs',   v => { state.diskDev = v;  loadDisk(v); });

  // Theme toggle
  document.getElementById('theme-toggle').addEventListener('click', toggleTheme);

  // Freeze / resume
  document.getElementById('pause-toggle')
    .addEventListener('click', () => setPaused(!state.paused));

  // One delegated handler for every card's CSV button, so cards added later
  // need no extra wiring.
  document.addEventListener('click', e => {
    const key = e.target?.dataset?.export;
    if (key) exportCSV(key);
  });

  // Space toggles the freeze, the way a media player would — but not while
  // the range picker has focus, where space opens the dropdown.
  document.addEventListener('keydown', e => {
    if (e.code !== 'Space' || e.target !== document.body) return;
    e.preventDefault();
    setPaused(!state.paused);
  });
}

document.addEventListener('DOMContentLoaded', init);
