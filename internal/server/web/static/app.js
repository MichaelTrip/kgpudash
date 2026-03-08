'use strict';

// ── Theme ───────────────────────────────────────────────────────────────────
const THEME_KEY = 'kgpudash-theme';

function getTheme() {
  return localStorage.getItem(THEME_KEY) ||
    (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
}

function applyTheme(theme) {
  document.documentElement.setAttribute('data-bs-theme', theme);
  const icon = document.getElementById('theme-icon');
  if (icon) {
    icon.className = theme === 'dark' ? 'bi bi-moon-stars-fill' : 'bi bi-sun-fill';
  }
  localStorage.setItem(THEME_KEY, theme);
  // Re-render charts with correct grid colours if the history panel is open.
  // Guard against being called before state is initialised.
  if (typeof state !== 'undefined' && state.historyNode) loadHistory();
}

// ── State ───────────────────────────────────────────────────────────────────
const state = {
  nodes: {},          // nodeName → { name, gpus: [] }
  historyNode: null,
  historyGPU: null,
  historyRange: 3600000, // 1h in ms
  charts: {},
};

// ── WebSocket ───────────────────────────────────────────────────────────────
let ws = null;
let wsReconnectTimer = null;

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);

  ws.onopen = () => {
    setWSStatus(true);
    document.getElementById('cluster-label').innerHTML =
      `<i class="bi bi-hdd-network me-1"></i>${escHtml(location.hostname)}`;
    clearTimeout(wsReconnectTimer);
  };

  ws.onmessage = (evt) => {
    try {
      const msg = JSON.parse(evt.data);
      if (msg.type === 'snapshot') handleSnapshot(msg);
    } catch (e) {
      console.error('WS parse error', e);
    }
  };

  ws.onclose = () => {
    setWSStatus(false);
    wsReconnectTimer = setTimeout(connectWS, 3000);
  };

  ws.onerror = () => ws.close();
}

function setWSStatus(connected) {
  const el = document.getElementById('ws-status');
  el.classList.toggle('connected', connected);
  el.title = connected ? 'Connected' : 'Disconnected';
}

// ── Snapshot handling ────────────────────────────────────────────────────────
function handleSnapshot(msg) {
  const incoming = {};
  for (const node of (msg.nodes || [])) {
    incoming[node.name] = node;
    state.nodes[node.name] = node;
  }
  for (const name of Object.keys(state.nodes)) {
    if (!incoming[name]) delete state.nodes[name];
  }
  renderGrid();
  updateNodeFilter();
  updateGPUCount();
}

// ── Rendering ────────────────────────────────────────────────────────────────
function renderGrid() {
  const grid      = document.getElementById('main-grid');
  const empty     = document.getElementById('empty-state');
  const filterVendor = document.getElementById('filter-vendor').value;
  const filterNode   = document.getElementById('filter-node').value;
  const filterPod    = document.getElementById('filter-pod').value.toLowerCase();

  const nodes = Object.values(state.nodes)
    .filter(n => !filterNode || n.name === filterNode)
    .sort((a, b) => a.name.localeCompare(b.name));

  // Remove stale node sections
  const existingSections = new Set(
    [...grid.querySelectorAll('.kgpu-node-section')].map(el => el.dataset.node)
  );
  for (const name of existingSections) {
    if (!nodes.find(n => n.name === name)) {
      grid.querySelector(`.kgpu-node-section[data-node="${CSS.escape(name)}"]`)?.remove();
    }
  }

  let totalVisible = 0;

  for (const node of nodes) {
    const gpus = (node.gpus || []).filter(g => {
      if (filterVendor && g.vendor !== filterVendor) return false;
      if (filterPod && !(g.pod || '').toLowerCase().includes(filterPod) &&
                       !(g.namespace || '').toLowerCase().includes(filterPod)) return false;
      return true;
    });

    if (gpus.length === 0) continue;
    totalVisible += gpus.length;

    let section = grid.querySelector(`.kgpu-node-section[data-node="${CSS.escape(node.name)}"]`);
    if (!section) {
      section = document.createElement('div');
      section.className = 'kgpu-node-section';
      section.dataset.node = node.name;
      section.innerHTML = `
        <div class="kgpu-node-heading">
          <i class="bi bi-server"></i>
          <span>${escHtml(node.name)}</span>
        </div>
        <div class="kgpu-gpu-row"></div>`;
      grid.appendChild(section);
    }

    const row = section.querySelector('.kgpu-gpu-row');
    renderGPURow(row, node.name, gpus);
  }

  empty.style.display = totalVisible === 0 ? 'flex' : 'none';
}

function renderGPURow(row, nodeName, gpus) {
  const existing = {};
  for (const card of row.querySelectorAll('.kgpu-card')) {
    existing[card.dataset.uuid] = card;
  }

  const seen = new Set();
  for (const gpu of gpus) {
    seen.add(gpu.uuid);
    let card = existing[gpu.uuid];
    if (!card) {
      card = createGPUCard(nodeName, gpu);
      row.appendChild(card);
    } else {
      updateGPUCard(card, gpu);
    }
  }

  for (const [uuid, card] of Object.entries(existing)) {
    if (!seen.has(uuid)) card.remove();
  }
}

function createGPUCard(nodeName, gpu) {
  const card = document.createElement('div');
  card.className = 'kgpu-card';
  card.dataset.uuid   = gpu.uuid;
  card.dataset.vendor = gpu.vendor;
  card.dataset.node   = nodeName;
  card.dataset.index  = gpu.index;
  card.innerHTML = gpuCardHTML(gpu);
  card.addEventListener('click', () => openHistory(nodeName, gpu));
  return card;
}

function updateGPUCard(card, gpu) {
  card.dataset.vendor = gpu.vendor;
  card.innerHTML = gpuCardHTML(gpu);
  const nodeName = card.dataset.node;
  card.onclick = () => openHistory(nodeName, gpu);
}

function gpuCardHTML(gpu) {
  const vramPct  = gpu.vram_total_mb > 0
    ? Math.round((gpu.vram_used_mb / gpu.vram_total_mb) * 100) : 0;
  const tempPct  = Math.min(100, Math.round((gpu.temp_celsius / 100) * 100));
  const powerPct = Math.min(100, Math.round((gpu.power_watts / 400) * 100));

  // Show VRAM row only when we have data (total > 0)
  const vramValue = gpu.vram_total_mb > 0
    ? `${fmtMB(gpu.vram_used_mb)} / ${fmtMB(gpu.vram_total_mb)}`
    : '—';

  // Show power only when we have data
  const powerValue = gpu.power_watts > 0
    ? `${gpu.power_watts.toFixed(1)} W`
    : '—';

  const podSection = gpu.pod
    ? `<div class="kgpu-pod">
         <i class="bi bi-box text-util"></i>
         <span class="kgpu-pod-name">${escHtml(gpu.pod)}</span>
         <span class="kgpu-pod-ns">${escHtml(gpu.namespace || '')}</span>
       </div>`
    : `<div class="kgpu-pod" style="color:var(--kgpu-muted)">
         <i class="bi bi-circle"></i> idle
       </div>`;

  return `
    <div class="kgpu-card-header">
      <div>
        <div class="kgpu-gpu-id">GPU ${gpu.index}</div>
        <div class="kgpu-gpu-name" title="${escHtml(gpu.name)}">${escHtml(gpu.name)}</div>
      </div>
      <span class="kgpu-vendor-badge">${escHtml(gpu.vendor)}</span>
    </div>

    <div class="kgpu-metrics">
      <div class="kgpu-metric">
        <div class="kgpu-metric-row">
          <span class="kgpu-metric-label">
            <i class="bi bi-activity text-util"></i> Utilization
          </span>
          <span class="kgpu-metric-value">${gpu.util_percent.toFixed(1)}%</span>
        </div>
        <div class="kgpu-bar">
          <div class="kgpu-bar-fill util" style="width:${gpu.util_percent}%"></div>
        </div>
      </div>

      <div class="kgpu-metric">
        <div class="kgpu-metric-row">
          <span class="kgpu-metric-label">
            <i class="bi bi-memory text-vram"></i> VRAM
          </span>
          <span class="kgpu-metric-value">${vramValue}</span>
        </div>
        <div class="kgpu-bar">
          <div class="kgpu-bar-fill vram" style="width:${vramPct}%"></div>
        </div>
      </div>

      <div class="kgpu-metric">
        <div class="kgpu-metric-row">
          <span class="kgpu-metric-label">
            <i class="bi bi-thermometer-half text-temp"></i> Temperature
          </span>
          <span class="kgpu-metric-value">${gpu.temp_celsius.toFixed(1)} °C</span>
        </div>
        <div class="kgpu-bar">
          <div class="kgpu-bar-fill temp" style="width:${tempPct}%"></div>
        </div>
      </div>

      <div class="kgpu-metric">
        <div class="kgpu-metric-row">
          <span class="kgpu-metric-label">
            <i class="bi bi-lightning-charge text-power"></i> Power
          </span>
          <span class="kgpu-metric-value">${powerValue}</span>
        </div>
        <div class="kgpu-bar">
          <div class="kgpu-bar-fill power" style="width:${powerPct}%"></div>
        </div>
      </div>
    </div>

    ${podSection}`;
}

// ── History panel ────────────────────────────────────────────────────────────
function openHistory(nodeName, gpu) {
  state.historyNode = nodeName;
  state.historyGPU  = gpu;

  const panel = document.getElementById('history-panel');
  panel.classList.add('visible');
  document.getElementById('history-title').textContent =
    `${gpu.name} · GPU ${gpu.index} · ${nodeName}`;

  loadHistory();
}

function closeHistory() {
  document.getElementById('history-panel').classList.remove('visible');
  state.historyNode = null;
  state.historyGPU  = null;
  destroyCharts();
}

async function loadHistory() {
  if (!state.historyNode || !state.historyGPU) return;

  const to   = Date.now();
  const from = to - state.historyRange;
  const url  = `/api/history?node=${encodeURIComponent(state.historyNode)}&gpu=${state.historyGPU.index}&from=${from}&to=${to}`;

  try {
    const res = await fetch(url);
    if (!res.ok) throw new Error(res.statusText);
    const data = await res.json();
    renderCharts(data.points || []);
  } catch (e) {
    console.error('history fetch failed', e);
  }
}

function chartColors() {
  const dark = document.documentElement.getAttribute('data-bs-theme') === 'dark';
  return {
    grid:   dark ? '#1e2235' : '#e9ecef',
    tick:   dark ? '#7986cb' : '#6c757d',
    border: dark ? '#252a40' : '#dee2e6',
  };
}

function renderCharts(points) {
  destroyCharts();

  const labels = points.map(p => new Date(p.Time).toLocaleTimeString());
  const c = chartColors();

  const chartDefs = [
    { id: 'chart-util',  label: 'Utilization %',  key: 'UtilPercent', color: '#6366f1' },
    { id: 'chart-vram',  label: 'VRAM Used (MB)',  key: 'VRAMUsedMB',  color: '#06b6d4' },
    { id: 'chart-temp',  label: 'Temperature °C',  key: 'TempCelsius', color: '#f59e0b' },
    { id: 'chart-power', label: 'Power (W)',        key: 'PowerWatts',  color: '#ef4444' },
  ];

  const baseOpts = {
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    plugins: { legend: { display: false } },
    scales: {
      x: {
        ticks: { color: c.tick, maxTicksLimit: 6, font: { size: 10 } },
        grid:  { color: c.grid },
        border: { color: c.border },
      },
      y: {
        ticks: { color: c.tick, font: { size: 10 } },
        grid:  { color: c.grid },
        border: { color: c.border },
      },
    },
  };

  for (const def of chartDefs) {
    const canvas = document.getElementById(def.id);
    state.charts[def.id] = new Chart(canvas, {
      type: 'line',
      data: {
        labels,
        datasets: [{
          data: points.map(p => p[def.key]),
          borderColor: def.color,
          backgroundColor: def.color + '22',
          borderWidth: 1.5,
          pointRadius: 0,
          fill: true,
          tension: 0.3,
        }],
      },
      options: baseOpts,
    });
  }
}

function destroyCharts() {
  for (const chart of Object.values(state.charts)) chart.destroy();
  state.charts = {};
}

// ── Filters ──────────────────────────────────────────────────────────────────
function updateNodeFilter() {
  const sel     = document.getElementById('filter-node');
  const current = sel.value;
  const nodes   = Object.keys(state.nodes).sort();

  sel.innerHTML = '<option value="">All Nodes</option>';
  for (const name of nodes) {
    const opt = document.createElement('option');
    opt.value = name;
    opt.textContent = name;
    if (name === current) opt.selected = true;
    sel.appendChild(opt);
  }
}

function updateGPUCount() {
  let total = 0;
  for (const node of Object.values(state.nodes)) {
    total += (node.gpus || []).length;
  }
  document.getElementById('gpu-count').innerHTML =
    `<i class="bi bi-gpu-card me-1"></i>${total} GPU${total !== 1 ? 's' : ''}`;
}

// ── Utilities ────────────────────────────────────────────────────────────────
function escHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function fmtMB(mb) {
  if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB';
  return mb + ' MB';
}

// ── Event listeners ──────────────────────────────────────────────────────────
document.getElementById('filter-vendor').addEventListener('change', renderGrid);
document.getElementById('filter-node').addEventListener('change', renderGrid);
document.getElementById('filter-pod').addEventListener('input', renderGrid);
document.getElementById('history-close').addEventListener('click', closeHistory);

document.getElementById('theme-toggle').addEventListener('click', () => {
  const current = document.documentElement.getAttribute('data-bs-theme') || 'dark';
  applyTheme(current === 'dark' ? 'light' : 'dark');
});

document.querySelectorAll('.kgpu-range-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.kgpu-range-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    state.historyRange = parseInt(btn.dataset.range, 10);
    loadHistory();
  });
});

// ── Boot ─────────────────────────────────────────────────────────────────────
// Apply saved / system theme (state is now initialised so applyTheme is safe).
applyTheme(getTheme());
connectWS();
