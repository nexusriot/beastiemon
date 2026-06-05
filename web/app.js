'use strict';


function humanBytes(b) {
  if (b == null || b === 0) return '0 B';
  const units = ['B','KB','MB','GB','TB'];
  const i = Math.min(Math.floor(Math.log2(b) / 10), units.length - 1);
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

const baseOpts = {
  width:  100,
  height: 160,
  padding: [8, 0, 0, 0],
  cursor: { show: true, sync: { key: 'bm' } },
  select: { show: false },
  axes: [
    {
      stroke: '#8b949e',
      grid:   { stroke: '#21262d', width: 1 },
      ticks:  { show: false },
      font:   '10px monospace',
    },
    {
      stroke: '#8b949e',
      grid:   { stroke: '#21262d', width: 1 },
      ticks:  { show: false },
      font:   '10px monospace',
      size:   52,
    },
  ],
  legend: { show: true, live: true },
};

function makeOpts(extra) {
  return Object.assign({}, baseOpts, extra,
    { axes: baseOpts.axes, cursor: baseOpts.cursor });
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
    ],
    axes: [
      baseOpts.axes[0],
      { ...baseOpts.axes[1], values: (u, vals) => vals.map(v => v != null ? v.toFixed(0)+'%' : '') },
    ],
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
    axes: [
      baseOpts.axes[0],
      { ...baseOpts.axes[1], values: (u, vals) => vals.map(v => v != null ? v.toFixed(2) : '') },
    ],
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
    axes: [
      baseOpts.axes[0],
      { ...baseOpts.axes[1], values: (u, vals) => vals.map(v => v != null ? humanBytes(v) : '') },
    ],
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
    axes: [
      baseOpts.axes[0],
      { ...baseOpts.axes[1], values: (u, vals) => vals.map(v => v != null ? humanBytes(v)+'/s' : '') },
    ],
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
    axes: [
      baseOpts.axes[0],
      { ...baseOpts.axes[1], values: (u, vals) => vals.map(v => v != null ? humanBytes(v)+'/s' : '') },
    ],
  });

  state.charts.disk = new uPlot(opts, data, el);
  if (state.resizers.disk) state.resizers.disk.disconnect();
  state.resizers.disk = autoresize(state.charts.disk, el);
}


async function fetchSeries(metric, extra = '') {
  const r = await fetch(`/api/series?metric=${metric}&range=${state.range}${extra}`);
  if (!r.ok) throw new Error(r.statusText);
  return r.json();
}

async function loadCPU()  { const d = await fetchSeries('cpu');  buildCPUChart(d.data); }
async function loadLoad() { const d = await fetchSeries('load'); buildLoadChart(d.data); }
async function loadMem()  { const d = await fetchSeries('mem');  buildMemChart(d.data); }

async function loadNet(iface = '') {
  const extra = iface ? `&iface=${iface}` : '';
  const d = await fetchSeries('net', extra);
  buildNetChart(d.data);
}

async function loadDisk(dev = '') {
  const extra = dev ? `&dev=${dev}` : '';
  const d = await fetchSeries('disk', extra);
  buildDiskChart(d.data);
}

async function loadAll() {
  await Promise.allSettled([loadCPU(), loadLoad(), loadMem(),
    loadNet(state.netIface), loadDisk(state.diskDev)]);
}


function appendToChart(chart, ts, values) {
  if (!chart) return;
  const data = chart.data;
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
  state.latestSnap = snap;
  const ts = new Date(snap.ts).getTime() / 1000;

  // CPU summary
  const cpuPct = snap.cpu?.total ?? 0;
  const el = document.getElementById('cpu-pct');
  if (el) {
    el.textContent = cpuPct.toFixed(1) + '%';
    el.style.color = cpuPct >= 90 ? COLORS.red : cpuPct >= 70 ? COLORS.orange : COLORS.green;
  }
  appendToChart(state.charts.cpu, ts,
    [snap.cpu?.user, snap.cpu?.sys, snap.cpu?.idle, snap.cpu?.total]);

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
    mp.style.color = memPct >= 90 ? COLORS.red : memPct >= 70 ? COLORS.orange : COLORS.text;
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
  let rd = 0, wr = 0;
  for (const d of (snap.disk || [])) {
    if (!state.diskDev || d.dev === state.diskDev) {
      rd += d.read_bps ?? 0;
      wr += d.write_bps ?? 0;
    }
  }
  appendToChart(state.charts.disk, ts, [rd, wr]);

  // Uptime
  const uEl = document.getElementById('uptime-line');
  if (uEl && snap.uptime) uEl.textContent = 'up ' + humanDuration(snap.uptime);

  // Temperatures
  renderTemps(snap.temps || []);

  // Filesystems
  renderFS(snap.fs || []);

  // Top processes
  renderProcs(snap.procs || []);

  // Build iface / dev tabs if not yet built.
  buildIfaceTabs(snap.net || [], snap.disk || []);
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
      <span class="temp-name">${t.name}</span>
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
        <span class="fs-mount">${f.mount}</span>
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
function buildIfaceTabs(nets, disks) {
  if (tabsBuilt) return;
  tabsBuilt = true;

  const netTabsEl = document.getElementById('iface-tabs');
  const devTabsEl = document.getElementById('dev-tabs');

  // Net tabs
  const ifaces = [...new Set(nets.map(n => n.iface))].sort();
  if (ifaces.length > 1) {
    netTabsEl.innerHTML = ['(all)', ...ifaces].map((name, i) => {
      const val = i === 0 ? '' : name;
      const active = val === (state.netIface ?? '') ? 'active' : '';
      return `<button class="tab-btn ${active}" data-val="${val}">${name}</button>`;
    }).join('');
    netTabsEl.addEventListener('click', e => {
      if (!e.target.matches('.tab-btn')) return;
      state.netIface = e.target.dataset.val || null;
      netTabsEl.querySelectorAll('.tab-btn').forEach(b => b.classList.toggle('active', b === e.target));
      loadNet(state.netIface);
    });
  }

  // Disk tabs
  const devs = [...new Set(disks.map(d => d.dev))].sort();
  if (devs.length > 1) {
    devTabsEl.innerHTML = ['(all)', ...devs].map((name, i) => {
      const val = i === 0 ? '' : name;
      const active = val === (state.diskDev ?? '') ? 'active' : '';
      return `<button class="tab-btn ${active}" data-val="${val}">${name}</button>`;
    }).join('');
    devTabsEl.addEventListener('click', e => {
      if (!e.target.matches('.tab-btn')) return;
      state.diskDev = e.target.dataset.val || null;
      devTabsEl.querySelectorAll('.tab-btn').forEach(b => b.classList.toggle('active', b === e.target));
      loadDisk(state.diskDev);
    });
  }
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


async function init() {
  // Host info
  try {
    const h = await fetch('/api/host').then(r => r.json());
    const hl = document.getElementById('host-line');
    if (hl) hl.textContent = `${h.hostname}  •  ${h.platform} ${h.platformVersion}  •  ${h.kernelVersion}`;
  } catch {}

  // Seed charts from history
  await loadAll();

  // Connect live stream
  connectSSE();

  // Range selector
  document.getElementById('range-select').addEventListener('change', e => {
    state.range = e.target.value;
    tabsBuilt = false;
    loadAll();
  });
}

document.addEventListener('DOMContentLoaded', init);
