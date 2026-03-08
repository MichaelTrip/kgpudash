'use strict';

// ── State ──────────────────────────────────────────────────────────────────
const state = {
  nodes: {},          // nodeName → { name, gpus: [] }
  historyNode: null,
  historyGPU: null,
  historyRange: 3600000, // 1h in ms
  charts: {},
};

// ── WebSocket ──────────────────────────────────────────────────────────────
let ws = null;
let wsReconnectTimer = null;

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${proto}://${location.host}/ws`);

  ws.onopen = () => {
    setWSStatus(true);
    document.getElementById('cluster-label').textContent = location.hostname;
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

// ── Snapshot handling ──────────────────────────────────────────────────────
function handleSnapshot(msg) {
  // Merge nodes into state.
  const incoming = {};
  for (const node of (msg.nodes || [])) {
    incoming[node.name] = node;
    state.nodes[node.name] = node;
  }

  // Remove nodes that are no longer present.
  for (const name of Object.keys(state.nodes)) {
    if (!incoming[name]) delete state.nodes[name];
  }

  renderGrid();
  updateNodeFilter();
  updateGPUCount();
}

// ── Rendering ──────────────────────────────────────────────────────────────
function renderGrid() {
  const grid = document.getElementById('main-grid');
  const empty = document.getElementById('empty-state');
  const filterVendor = document.getElementById('filter-vendor').value;
  const filterNode   = document.getElementById('filter-node').value;
  const filterPod    = document.getElementById('filter-pod').value.toLowerCase();

  const nodes = Object.values(state.nodes)
    .filter(n => !filterNode || n.name === filterNode)
    .sort((a, b) => a.name.localeCompare(b.name));

  // Remove stale node sections.
  const existingSections = new Set(
    [...grid.querySelectorAll('.node-section')].map(el => el.dataset.node)
  );
  for (const name of existingSections) {
    if (!nodes.find(n => n.name === name)) {
      grid.querySelector(`.node-section[data-node="${CSS.escape(name)}"]`)?.remove();
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

    let section = grid.querySelector(`.node-section[data-node="${CSS.escape(node.name)}"]`);
    if (!section) {
      section = document.createElement('div');
      section.className = 'node-section';
      section.dataset.node = node.name;
      section.innerHTML = `
        <div class="node-title">
          <span>⬡ ${escHtml(node.name)}</span>
        </div>
        <div class="gpu-row"></div>`;
      grid.appendChild(section);
    }

    const row = section.querySelector('.gpu-row');
    renderGPURow(row, node.name, gpus);
  }

  empty.style.display = totalVisible === 0 ? 'flex' : 'none';
}

function renderGPURow(row, nodeName, gpus) {
  // Build a map of existing cards.
  const existing = {};
  for (const card of row.querySelectorAll('.gpu-card')) {
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

  // Remove cards for GPUs no longer present.
  for (const [uuid, card] of Object.entries(existing)) {
    if (!seen.has(uuid)) card.remove();
  }
}

function createGPUCard(nodeName, gpu) {
  const card = document.createElement('div');
  card.className = 'gpu-card';
  card.dataset.uuid = gpu.uuid;
  card.dataset.vendor = gpu.vendor;
  card.dataset.node = nodeName;
  card.dataset.index = gpu.index;
  card.innerHTML = gpuCardHTML(gpu);
  card.addEventListener('click', () => openHistory(nodeName, gpu));
  return card;
}

function updateGPUCard(card, gpu) {
  card.dataset.vendor = gpu.vendor;
  card.innerHTML = gpuCardHTML(gpu);
  // Re-attach click handler (innerHTML wipes it).
  const nodeName = card.dataset.node;
  card.onclick = () => openHistory(nodeName, gpu);
}

function gpuCardHTML(gpu) {
  const vramPct = gpu.vram_total_mb > 0
    ? Math.round((gpu.vram_used_mb / gpu.vram_total_mb) * 100)
    : 0;
  const tempPct  = Math.min(100, Math.round((gpu.temp_celsius / 100) * 100));
  const powerPct = Math.min(100, Math.round((gpu.power_watts / 400) * 100));

  const podSection = gpu.pod ? `
    <div class="pod-info">
      <span class="pod-icon">⬡</span>
      <span class="pod-name">${escHtml(gpu.pod)}</span>
      <span class="pod-ns">${escHtml(gpu.namespace || '')}</span>
    </div>` : `
    <div class="pod-info" style="color:var(--text-muted)">
      <span class="pod-icon">○</span> idle
    </div>`;

  return `
    <div class="card-header">
      <div>
        <div class="card-gpu-id">GPU ${gpu.index}</div>
        <div class="card-gpu-name" title="${escHtml(gpu.name)}">${escHtml(gpu.name)}</div>
      </div>
      <span class="vendor-badge">${escHtml(gpu.vendor)}</span>
    </div>
    <div class="metric-list">
      <div class="metric-row">
        <div class="metric-label-row">
          <span>Utilization</span>
          <span class="metric-value">${gpu.util_percent.toFixed(1)}%</span>
        </div>
        <div class="progress-bar"><div class="progress-fill util" style="width:${gpu.util_percent}%"></div></div>
      </div>
      <div class="metric-row">
        <div class="metric-label-row">
          <span>VRAM</span>
          <span class="metric-value">${fmtMB(gpu.vram_used_mb)} / ${fmtMB(gpu.vram_total_mb)}</span>
        </div>
        <div class="progress-bar"><div class="progress-fill vram" style="width:${vramPct}%"></div></div>
      </div>
      <div class="metric-row">
        <div class="metric-label-row">
          <span>Temperature</span>
          <span class="metric-value">${gpu.temp_celsius.toFixed(1)} °C</span>
        </div>
        <div class="progress-bar"><div class="progress-fill temp" style="width:${tempPct}%"></div></div>
      </div>
      <div class="metric-row">
        <div class="metric-label-row">
          <span>Power</span>
          <span class="metric-value">${gpu.power_watts.toFixed(1)} W</span>
        </div>
        <div class="progress-bar"><div class="progress-fill power" style="width:${powerPct}%"></div></div>
      </div>
    </div>
    ${podSection}`;
}

// ── History panel ──────────────────────────────────────────────────────────
function openHistory(nodeName, gpu) {
  state.historyNode = nodeName;
  state.historyGPU  = gpu;

  const panel = document.getElementById('history-panel');
  panel.hidden = false;
  document.getElementById('history-title').textContent =
    `${gpu.name} · GPU ${gpu.index} · ${nodeName}`;

  loadHistory();
}

function closeHistory() {
  document.getElementById('history-panel').hidden = true;
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

function renderCharts(points) {
  destroyCharts();

  const labels = points.map(p => new Date(p.Time).toLocaleTimeString());

  const chartDefs = [
    { id: 'chart-util',  label: 'Utilization %', key: 'UtilPercent', color: '#6366f1' },
    { id: 'chart-vram',  label: 'VRAM Used (MB)', key: 'VRAMUsedMB',  color: '#06b6d4' },
    { id: 'chart-temp',  label: 'Temperature °C', key: 'TempCelsius', color: '#f59e0b' },
    { id: 'chart-power', label: 'Power (W)',       key: 'PowerWatts',  color: '#ef4444' },
  ];

  const chartOpts = {
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    plugins: { legend: { display: false } },
    scales: {
      x: {
        ticks: { color: '#8892a4', maxTicksLimit: 6, font: { size: 10 } },
        grid:  { color: '#2e3250' },
      },
      y: {
        ticks: { color: '#8892a4', font: { size: 10 } },
        grid:  { color: '#2e3250' },
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
      options: chartOpts,
    });
  }
}

function destroyCharts() {
  for (const chart of Object.values(state.charts)) chart.destroy();
  state.charts = {};
}

// ── Filters ────────────────────────────────────────────────────────────────
function updateNodeFilter() {
  const sel = document.getElementById('filter-node');
  const current = sel.value;
  const nodes = Object.keys(state.nodes).sort();

  // Rebuild options preserving selection.
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
  document.getElementById('gpu-count').textContent =
    `${total} GPU${total !== 1 ? 's' : ''}`;
}

// ── Utilities ──────────────────────────────────────────────────────────────
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

// ── Event listeners ────────────────────────────────────────────────────────
document.getElementById('filter-vendor').addEventListener('change', renderGrid);
document.getElementById('filter-node').addEventListener('change', renderGrid);
document.getElementById('filter-pod').addEventListener('input', renderGrid);
document.getElementById('history-close').addEventListener('click', closeHistory);

document.querySelectorAll('.range-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.range-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    state.historyRange = parseInt(btn.dataset.range, 10);
    loadHistory();
  });
});

// ── Boot ───────────────────────────────────────────────────────────────────
connectWS();
