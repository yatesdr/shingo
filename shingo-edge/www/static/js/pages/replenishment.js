// replenishment.js — UOP-threshold replenishment admin page.
//
// Three inline edit verbs:
//   replenishmentApplyLoader    — PUT loader_payload_threshold
//   replenishmentDeleteLoader   — DELETE loader_payload_threshold
//   replenishmentApplyCell      — PUT style_node_claim reorder_point
//
// Page reloads after a successful write so the source badge and any
// derived state (e.g. another row affected by SendClaimSync) reflect.

// CALC_FIELDS lists the seven calculator inputs in display order.
// Each entry: snake_case key (matches both the data-field attribute
// on the modal <input> and the canonical override token persisted to
// the threshold row), human label for the "Overrides: ..." line on
// the main table, and the result-payload field name returned by
// /api/replenishment/calculate.
const CALC_FIELDS = [
  { key: 'cycle_seconds',          label: 'Cycle time',     result: null /* engineer-supplied */ },
  { key: 'l1_queue_seconds',       label: 'L1 queue',       result: 'L1QueueSeconds' },
  { key: 'l1_transit_seconds',     label: 'L1 transit',     result: 'L1TransitSeconds' },
  { key: 'l2_load_seconds',        label: 'L2 fill time',   result: 'L2LoadSeconds' },
  { key: 'l2_transit_seconds',     label: 'L2 transit',     result: 'L2TransitSeconds' },
  { key: 'market_to_cell_seconds', label: 'Market→cell',    result: 'MarketToCellSeconds' },
  { key: 'safety_factor',          label: 'Safety factor',  result: null /* engineer-supplied */ },
];

// Render the "Overrides: <list>" hint under any source/confidence cell
// that carries a non-empty data-overrides attribute. Server stores the
// list as comma-separated snake_case tokens; UI maps to human labels.
function renderOverridesHints() {
  const labelByKey = {};
  for (const f of CALC_FIELDS) labelByKey[f.key] = f.label;
  for (const el of document.querySelectorAll('.overrides-line')) {
    const raw = (el.dataset.overrides || '').trim();
    if (!raw) { el.textContent = ''; continue; }
    const labels = raw.split(',').map(t => labelByKey[t.trim()] || t.trim()).filter(Boolean);
    el.textContent = 'Overrides: ' + labels.join(', ');
  }
}

// Render the "≈ N bins" annotation next to each loader threshold value
// on the main table. Reads bin capacity from the row's data-bin-capacity
// (populated server-side from FindAnyLoaderClaimForPayload). cap or
// threshold = 0 ⇒ empty span; the muted color and small font are set in
// the template so nothing here needs to manipulate style.
function renderThresholdImpliedBins() {
  for (const el of document.querySelectorAll('.threshold-implied-bins')) {
    const row = el.closest('tr');
    const cap = parseInt(row.dataset.binCapacity, 10) || 0;
    const threshold = parseInt(row.dataset.currentThreshold, 10) || 0;
    el.textContent = formatImpliedBins(threshold, cap);
  }
}

function onPageReady() {
  renderOverridesHints();
  renderThresholdImpliedBins();
}
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', onPageReady);
} else {
  onPageReady();
}

async function replenishmentApplyLoader(btn) {
  const row = btn.closest('tr');
  const coreNodeName = row.dataset.coreNodeName;
  const payload      = row.dataset.payload;
  const value        = parseInt(row.querySelector('.loader-threshold-input').value, 10) || 0;
  const r = await fetch('/api/replenishment/loader-threshold', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      core_node_name: coreNodeName,
      payload_code: payload,
      replenish_uop_threshold: value,
      source: 'manual',
    }),
  });
  if (!r.ok) {
    alert('Failed: ' + await r.text());
    return;
  }
  window.location.reload();
}

async function replenishmentDeleteLoader(btn) {
  const row = btn.closest('tr');
  const coreNodeName = row.dataset.coreNodeName;
  const payload      = row.dataset.payload;
  if (!confirm('Remove threshold configuration for ' + payload + '? Legacy bin-count will resume for this pair.')) {
    return;
  }
  const r = await fetch('/api/replenishment/loader-threshold', {
    method: 'DELETE',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ core_node_name: coreNodeName, payload_code: payload }),
  });
  if (!r.ok) {
    alert('Failed: ' + await r.text());
    return;
  }
  window.location.reload();
}

// ── Calculate modal ───────────────────────────────────────────────
//
// Every calculator input is editable. On Run calculation the server
// returns the observed inputs over the date range; we pre-fill each
// input and remember the observed value so we can compute which
// fields the engineer overrode when they click Apply / Override.
// Outputs recompute locally on every input change — the formula is
// trivial enough to mirror in JS and we save a server round-trip per
// edit.

let calcContext = null;

function replenishmentOpenCalculate(btn) {
  const row = btn.closest('tr');
  calcContext = {
    coreNodeName: row.dataset.coreNodeName,
    payload: row.dataset.payload,
    currentThreshold: parseInt(row.dataset.currentThreshold, 10) || 0,
    safety: parseFloat(row.dataset.safety) || 1.5,
    observed: {},   // snake_case key → numeric observed value
    confidence: '',
    computedAt: '',
    thresholdCalculated: 0,
    capUOP: 0,   // loader's per-bin UOP capacity; populated from result.inputs.BinCapacityUOP on Run. Used only for the "≈ N bins" annotation.
  };
  document.getElementById('calc-title').textContent =
    'Calculate threshold for ' + calcContext.payload + ' (' + calcContext.coreNodeName + ')';
  document.getElementById('calc-result').style.display = 'none';
  document.getElementById('calc-status').textContent = '';
  // Seed engineer-supplied defaults so the inputs aren't blank pre-run.
  document.getElementById('calc-input-cycle_seconds').value = '22.5';
  document.getElementById('calc-input-safety_factor').value = calcContext.safety.toString();
  for (const f of CALC_FIELDS) {
    if (f.result) document.getElementById('calc-input-' + f.key).value = '';
  }
  document.getElementById('calculate-modal').style.display = 'block';
}

function replenishmentCloseCalculate() {
  document.getElementById('calculate-modal').style.display = 'none';
  calcContext = null;
}

function isoDateRange(days) {
  const end = new Date();
  const start = new Date(end);
  start.setDate(start.getDate() - days);
  return { start: start.toISOString(), end: end.toISOString() };
}

async function replenishmentRunCalculate() {
  if (!calcContext) return;
  const days = parseInt(document.getElementById('calc-range').value, 10);
  const cycle = parseFloat(document.getElementById('calc-input-cycle_seconds').value) || 0;
  const safety = parseFloat(document.getElementById('calc-input-safety_factor').value) || 1.5;
  document.getElementById('calc-status').textContent = 'Running…';

  const range = isoDateRange(days);
  const r = await fetch('/api/replenishment/calculate', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      core_node_name: calcContext.coreNodeName,
      payload_code: calcContext.payload,
      date_range_start: range.start,
      date_range_end: range.end,
      cycle_seconds: cycle,
      safety_factor: safety,
    }),
  });
  if (!r.ok) {
    document.getElementById('calc-status').textContent = 'Failed: ' + await r.text();
    return;
  }
  const result = await r.json();
  calcContext.confidence = result.confidence;
  calcContext.computedAt = result.computed_at || '';
  calcContext.thresholdCalculated = (result.outputs && result.outputs.L1Threshold) || 0;
  calcContext.capUOP = (result.inputs && result.inputs.BinCapacityUOP) || 0;

  // Pre-fill each editable input and remember the observed value.
  // Engineer-supplied inputs (cycle, safety) keep whatever the
  // engineer typed pre-Run.
  for (const f of CALC_FIELDS) {
    const inputEl = document.getElementById('calc-input-' + f.key);
    if (f.result) {
      const v = (result.inputs[f.result] || 0);
      inputEl.value = v.toFixed(1);
      calcContext.observed[f.key] = parseFloat(v.toFixed(1));
    } else {
      // Engineer-supplied: the observed "default" is whatever the
      // engineer had typed in the input at Run time.
      calcContext.observed[f.key] = parseFloat(inputEl.value) || 0;
    }
  }

  // Annotate each data-derived input with the sample count.
  const samples = {
    l1_queue_seconds:       result.samples_l1,
    l1_transit_seconds:     result.samples_l1,
    l2_load_seconds:        result.samples_l1,
    l2_transit_seconds:     result.samples_l2,
    market_to_cell_seconds: result.samples_retrieve,
  };
  for (const key of Object.keys(samples)) {
    const annotEl = document.getElementById('calc-source-' + key);
    if (!annotEl) continue;
    const n = samples[key] || 0;
    annotEl.textContent = result.date_range_days + 'd, n=' + n;
  }

  document.getElementById('calc-current').textContent = calcContext.currentThreshold;
  document.getElementById('calc-result').style.display = '';
  document.getElementById('calc-status').textContent = '';

  // Wire the input listener (idempotent — adding the same listener
  // again is a no-op for named functions).
  for (const f of CALC_FIELDS) {
    document.getElementById('calc-input-' + f.key).addEventListener('input', recomputeOutputsLocally);
  }
  recomputeOutputsLocally();
}

// Local mirror of service/threshold_calculator.go CalculateThresholds —
// keep paired with the Go formula. Pure formula output, no clamps.
function recomputeOutputsLocally() {
  if (!calcContext) return;
  const get = key => parseFloat(document.getElementById('calc-input-' + key).value) || 0;
  const cycle  = get('cycle_seconds');
  const safety = get('safety_factor') || 1.5;
  const l1Lead = get('l1_queue_seconds') + get('l1_transit_seconds') +
                 get('l2_load_seconds') + get('l2_transit_seconds');

  let l1 = 0, cell = 0;
  if (cycle > 0) {
    l1   = Math.ceil((l1Lead / cycle) * safety);
    cell = Math.ceil((get('market_to_cell_seconds') / cycle) * safety);
  }
  document.getElementById('calc-threshold').textContent  = l1;
  document.getElementById('calc-cell').textContent       = cell;
  document.getElementById('calc-confidence').textContent = calcContext.confidence;
  document.getElementById('calc-implied-bins').textContent = formatImpliedBins(l1, calcContext.capUOP);

  // Confidence LOW gates Apply — engineer Overrides to commit anyway.
  document.getElementById('calc-apply').disabled = (calcContext.confidence === 'LOW');
}

// formatImpliedBins returns the "≈ N bins" annotation for a threshold
// against a per-bin UOP capacity. Empty string when cap <= 0 (no claim
// resolvable) or threshold <= 0; the UI suppresses the line by writing
// the empty string to its text node.
function formatImpliedBins(threshold, capUOP) {
  if (capUOP <= 0 || threshold <= 0) return '';
  const n = Math.ceil(threshold / capUOP);
  return '≈ ' + n + (n === 1 ? ' bin' : ' bins');
}

// collectOverrides returns the list of input field keys whose current
// modal value differs from the value observed (or engineer-supplied
// at Run time). The comparison tolerates trivial float drift —
// matches must agree to one decimal place since that's what we
// pre-filled with.
function collectOverrides() {
  if (!calcContext) return [];
  const overrides = [];
  for (const f of CALC_FIELDS) {
    const cur = parseFloat(document.getElementById('calc-input-' + f.key).value) || 0;
    const obs = calcContext.observed[f.key] || 0;
    if (Math.abs(cur - obs) > 0.05) overrides.push(f.key);
  }
  return overrides;
}

async function replenishmentApplyFromModal() {
  if (!calcContext || !calcContext.computedAt) return;
  const value = parseInt(document.getElementById('calc-threshold').textContent, 10) || 0;
  const safety = parseFloat(document.getElementById('calc-input-safety_factor').value) || 1.5;
  const r = await fetch('/api/replenishment/calculate-and-apply', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      core_node_name: calcContext.coreNodeName,
      payload_code: calcContext.payload,
      value: value,
      safety_factor: safety,
      confidence: calcContext.confidence,
      threshold_calculated: calcContext.thresholdCalculated,
      computed_at: calcContext.computedAt,
      overridden_inputs: collectOverrides(),
    }),
  });
  if (!r.ok) {
    alert('Apply failed: ' + await r.text());
    return;
  }
  window.location.reload();
}

async function replenishmentOverrideFromModal() {
  if (!calcContext || !calcContext.computedAt) return;
  const suggested = parseInt(document.getElementById('calc-threshold').textContent, 10) || 0;
  const v = prompt('Override threshold value (integer):', suggested);
  if (v == null) return;
  const override = parseInt(v, 10);
  if (isNaN(override) || override < 0) {
    alert('Invalid value');
    return;
  }
  const r = await fetch('/api/replenishment/override', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      core_node_name: calcContext.coreNodeName,
      payload_code: calcContext.payload,
      override_value: override,
      confidence: calcContext.confidence,
      threshold_calculated: calcContext.thresholdCalculated,
      computed_at: calcContext.computedAt,
      overridden_inputs: collectOverrides(),
    }),
  });
  if (!r.ok) {
    alert('Override failed: ' + await r.text());
    return;
  }
  window.location.reload();
}

// ── Recalculate-all sweep ─────────────────────────────────────────

function replenishmentRecalculateAll() {
  document.getElementById('recalc-results').innerHTML = '';
  document.getElementById('recalc-all-modal').style.display = 'block';
}

function replenishmentCloseRecalcAll() {
  document.getElementById('recalc-all-modal').style.display = 'none';
}

async function replenishmentRunRecalculateAll() {
  const days = parseInt(document.getElementById('recalc-range').value, 10);
  const cycle = parseFloat(document.getElementById('recalc-cycle').value) || 0;
  const safety = parseFloat(document.getElementById('recalc-safety').value) || 1.5;
  const range = isoDateRange(days);
  const results = document.getElementById('recalc-results');
  results.innerHTML = '<p class="text-muted">Running…</p>';

  const r = await fetch('/api/replenishment/recalculate-all', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      date_range_start: range.start,
      date_range_end: range.end,
      cycle_seconds: cycle,
      safety_factor: safety,
    }),
  });
  if (!r.ok) {
    results.innerHTML = '<p>Failed: ' + await r.text() + '</p>';
    return;
  }
  const rows = await r.json();
  if (!rows || rows.length === 0) {
    results.innerHTML = '<p class="text-muted">No loader bindings on active processes.</p>';
    return;
  }
  let html = '<table class="table"><thead><tr><th>Loader</th><th>Payload</th><th>Threshold</th><th>Cell</th><th>Confidence</th><th></th></tr></thead><tbody>';
  for (const row of rows) {
    const implied = formatImpliedBins(row.threshold || 0, row.bin_capacity_uop || 0);
    const trailing = row.error || implied;
    html += '<tr><td>' + row.core_node_name + '</td>' +
            '<td class="mono">' + row.payload_code + '</td>' +
            '<td class="mono" style="text-align:right;">' + row.threshold + '</td>' +
            '<td class="mono" style="text-align:right;">' + row.cell_reorder + '</td>' +
            '<td>' + (row.confidence || '') + '</td>' +
            '<td class="text-muted">' + trailing + '</td></tr>';
  }
  html += '</tbody></table>';
  html += '<p class="text-muted" style="font-size:0.85rem;">Per-row Apply lives on the main page after closing this dialog. Review then click Apply on each row that looks right.</p>';
  results.innerHTML = html;
}

async function replenishmentApplyCell(btn) {
  const row = btn.closest('tr');
  const claimID    = parseInt(row.dataset.claimId, 10);
  const value      = parseInt(row.querySelector('.reorder-point-input').value, 10) || 0;
  const autoOn     = row.querySelector('.auto-reorder-input').checked;
  const r = await fetch('/api/replenishment/cell-reorder', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      claim_id: claimID,
      reorder_point: value,
      source: 'manual',
      auto_reorder: autoOn,
    }),
  });
  if (!r.ok) {
    alert('Failed: ' + await r.text());
    return;
  }
  window.location.reload();
}
