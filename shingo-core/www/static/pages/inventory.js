// Inventory v2 — exception-first. Leads with what needs attention: a
// Replenishment Health table (on-hand vs threshold per payload, worst first),
// a KPI strip, a conditional alerts banner, and lineside buckets with staleness
// colouring. Bin-level detail lives on /bins now. Data sources:
//   /api/inventory/monitor-totals — per-payload on-hand (bins+lineside split),
//     which payloads are monitored, and their thresholds.
//   /api/loader/list — editable per-loader threshold config (Save + Calc).
//   /api/inventory — bin rows, for the payload's holding bins + bin lifecycle.
//   /api/buckets — lineside buckets, now carrying updated_at for staleness.
//   /api/nodes — node id -> name, for home-loader node labels.
//   /api/parts/consumption?since&until — window consumption for the drill.

import {
  apiGet, apiPost, escapeHtml, delegateActions, toast, uiConfirm, timeAgo, debounce,
} from '/static/app.js';
import { onSSE } from '/static/shared/utils.js';

// ── state ──────────────────────────────────────────────────────────────
let health = [];        // /api/inventory/monitor-totals rows
let ledgerExceptions = {}; // /api/inventory/ledger-exceptions
let anomalySummary = {}; // /api/inventory/anomaly-summary (rejected/stale counts)
let loaders = [];       // /api/loader/list loaders (editable config)
let bins = [];          // /api/inventory rows
let buckets = [];       // /api/buckets rows (with updated_at)
let nodesById = {};     // node id -> name
let binsByPayload = {}; // payload_code -> [bin rows]
let onHandSeries = [];  // small rolling on-hand samples for the KPI sparkline

let searchTerm = '';
let zoneFilter = '';
let groupFilter = '';
let expanded = null;    // payload_code of the currently-open RH row
let drillPayload = null;
let drillDays = 14;

const page = document.querySelector('.inv-page');
const isAuth = !!page && page.dataset.authenticated === 'true';

const STALE_WARN_MS = 7 * 24 * 3600 * 1000;
const STALE_BAD_MS = 30 * 24 * 3600 * 1000;

// ── data loading ─────────────────────────────────────────────────────────
async function loadAll(quiet) {
  try {
    const [h, ld, inv, bk, nd, an, lx] = await Promise.all([
      apiGet('/api/inventory/monitor-totals').catch(() => []),
      apiGet('/api/loader/list').catch(() => ({ loaders: [] })),
      apiGet('/api/inventory').catch(() => []),
      apiGet('/api/buckets').catch(() => []),
      apiGet('/api/nodes').catch(() => ({})),
      apiGet('/api/inventory/anomaly-summary').catch(() => ({})),
      apiGet('/api/inventory/ledger-exceptions').catch(() => ({})),
    ]);
    ledgerExceptions = lx && typeof lx === 'object' ? lx : {};
    health = Array.isArray(h) ? h : [];
    anomalySummary = an && typeof an === 'object' ? an : {};
    loaders = (ld && ld.loaders) || [];
    bins = Array.isArray(inv) ? inv : (inv && inv.rows) || [];
    buckets = Array.isArray(bk) ? bk : (bk && bk.rows) || [];
    const nodes = (nd && (nd.nodes || nd.data || nd)) || [];
    nodesById = {};
    (Array.isArray(nodes) ? nodes : []).forEach((n) => {
      const id = n.id != null ? n.id : n.ID;
      if (id != null) nodesById[id] = n.name != null ? n.name : n.Name;
    });
    indexBins();
    sampleOnHand();
    renderAll();
  } catch (e) {
    if (!quiet) toast('Failed to load inventory: ' + (e.message || e), 'error', { sticky: true });
    const body = document.getElementById('rh-body');
    if (body && !health.length) {
      body.innerHTML = '<tr><td colspan="9" class="dash-empty">Could not load — '
        + escapeHtml(String(e.message || e))
        + ' <button class="btn btn-sm" data-action="refresh">Retry</button></td></tr>';
    }
  }
}

function indexBins() {
  binsByPayload = {};
  bins.forEach((b) => {
    const pc = b.payload_code || '';
    if (!pc) return;
    (binsByPayload[pc] = binsByPayload[pc] || []).push(b);
  });
}

function sampleOnHand() {
  let total = 0;
  health.forEach((r) => { if (r.monitored) total += r.on_hand; });
  onHandSeries.push(total);
  if (onHandSeries.length > 40) onHandSeries.shift();
}

// ── health classification ──────────────────────────────────────────────
// Worst -> best: ledger error, below, near, ok, unset. "Near" is within 20% of
// the threshold; a negative in-loop total is a broken ledger (can't be trusted).
function healthState(r) {
  if (r.on_hand < 0) return 'err';
  if (!r.monitored || !(r.threshold > 0)) return 'unset';
  if (r.on_hand < r.threshold) return 'below';
  if (r.on_hand < r.threshold * 1.2) return 'near';
  return 'ok';
}
const STATE_RANK = { err: 0, below: 1, near: 2, ok: 3, unset: 4 };

// ── search + filter helpers ───────────────────────────────────────────────
function zonesForPayload(pc) {
  const set = {};
  (binsByPayload[pc] || []).forEach((b) => { if (b.zone) set[b.zone] = 1; });
  return set;
}
function groupsForPayload(pc) {
  const set = {};
  (binsByPayload[pc] || []).forEach((b) => { if (b.group_name) set[b.group_name] = 1; });
  return set;
}
function matchesSearch(r) {
  if (!searchTerm) return true;
  const t = searchTerm.toLowerCase();
  if ((r.payload_code || '').toLowerCase().includes(t)) return true;
  if ((r.description || '').toLowerCase().includes(t)) return true;
  return (binsByPayload[r.payload_code] || []).some((b) =>
    (b.node_name || '').toLowerCase().includes(t) || (b.bin_label || '').toLowerCase().includes(t));
}
function passesFilters(r) {
  if (!matchesSearch(r)) return false;
  if (zoneFilter && !zonesForPayload(r.payload_code)[zoneFilter]) return false;
  if (groupFilter && !groupsForPayload(r.payload_code)[groupFilter]) return false;
  return true;
}
// Escape, then wrap the current search term in a highlight mark.
function hl(text) {
  const s = escapeHtml(text == null ? '' : String(text));
  if (!searchTerm) return s;
  const t = searchTerm.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  try {
    return s.replace(new RegExp('(' + t + ')', 'ig'), '<mark class="inv-hit">$1</mark>');
  } catch (e) { return s; }
}

// ── rendering ─────────────────────────────────────────────────────────────
function renderAll() {
  populateFilters();
  renderKpis();
  renderAlerts();
  renderLedgerExceptions();
  renderDeltaIntegrity();
  renderHealth();
  renderBuckets();
  const asof = document.getElementById('inv-asof');
  if (asof) asof.textContent = 'as of ' + new Date().toLocaleTimeString();
}

function populateFilters() {
  const zones = {}, groups = {};
  bins.forEach((b) => { if (b.zone) zones[b.zone] = 1; if (b.group_name) groups[b.group_name] = 1; });
  buckets.forEach((b) => { if (b.zone) zones[b.zone] = 1; if (b.group_name) groups[b.group_name] = 1; });
  fillSelect('inv-zone', Object.keys(zones).sort(), zoneFilter, 'All zones');
  fillSelect('inv-group', Object.keys(groups).sort(), groupFilter, 'All groups');
}
function fillSelect(id, values, current, allLabel) {
  const sel = document.getElementById(id);
  if (!sel || sel.dataset.filled === values.join('|')) return;
  sel.dataset.filled = values.join('|');
  sel.innerHTML = '<option value="">' + allLabel + '</option>'
    + values.map((v) => '<option value="' + escapeHtml(v) + '"' + (v === current ? ' selected' : '') + '>' + escapeHtml(v) + '</option>').join('');
}

function sparklineSvg(series) {
  if (series.length < 2) return '';
  const max = Math.max.apply(null, series), min = Math.min.apply(null, series);
  const span = max - min || 1;
  const pts = series.map((v, i) => {
    const x = (i / (series.length - 1)) * 120;
    const y = 18 - ((v - min) / span) * 16 - 1;
    return x.toFixed(1) + ',' + y.toFixed(1);
  }).join(' ');
  return '<svg viewBox="0 0 120 20" preserveAspectRatio="none" width="100%" height="18">'
    + '<polyline fill="none" stroke="var(--viz-teal)" stroke-width="1.6" points="' + pts + '"/></svg>';
}

function renderKpis() {
  const el = document.getElementById('inv-kpi');
  if (!el) return;
  let binU = 0, lineU = 0, monitored = 0, belowN = 0;
  health.forEach((r) => {
    if (r.monitored) { binU += r.bin_uop; lineU += r.bucket_uop; monitored++; }
    const st = healthState(r);
    if (st === 'below' || st === 'err') belowN++;
  });
  const onHand = binU + lineU;
  // Bin lifecycle: stocked (loaded), in-production-empty (claimed but drained), idle.
  let stocked = 0, prodEmpty = 0, idle = 0;
  bins.forEach((b) => {
    if (b.payload_code && b.confirmed && b.uop_remaining > 0) stocked++;
    else if (b.payload_code) prodEmpty++;
    else idle++;
  });
  const binTot = stocked + prodEmpty + idle || 1;
  let staleBk = 0;
  buckets.forEach((b) => { if (bucketAgeMs(b) > STALE_BAD_MS) staleBk++; });

  el.innerHTML = [
    tile('On-hand (monitored)', num(onHand), 'UoP · bins ' + num(binU) + ' + lineside ' + num(lineU),
      '', sparklineSvg(onHandSeries)),
    tile('Below threshold', String(belowN), 'of ' + monitored + ' monitored payloads',
      belowN ? 'kpi-tile--bad kpi-tile--clickable' : 'kpi-tile--clickable', '', 'scrollTo:rh'),
    lifecycleTile(stocked, prodEmpty, idle, binTot),
    tile('Stale buckets', String(staleBk), 'untouched &gt; 30 d — ghost risk',
      (staleBk ? 'kpi-tile--warn ' : '') + 'kpi-tile--clickable', '', 'scrollTo:buckets'),
  ].join('');
}
function tile(label, value, sub, cls, spark, action) {
  const act = action ? ' data-action="' + action + '"' : '';
  return '<div class="card kpi-tile ' + (cls || '') + '"' + act + '>'
    + '<div class="kpi-label">' + label + '</div>'
    + '<div class="kpi-value">' + value + '</div>'
    + '<div class="kpi-sub">' + sub + '</div>'
    + (spark ? '<div class="kpi-spark">' + spark + '</div>' : '')
    + '</div>';
}
function lifecycleTile(stocked, prodEmpty, idle, tot) {
  const w = (n) => ((n / tot) * 100).toFixed(1) + '%';
  return '<div class="card kpi-tile">'
    + '<div class="kpi-label">Bin lifecycle</div>'
    + '<div class="kpi-value">' + (stocked + prodEmpty + idle) + '</div>'
    + '<div class="kpi-sub">bins tracked</div>'
    + '<div class="lifebar"><span class="seg-stocked" style="width:' + w(stocked) + '"></span>'
    + '<span class="seg-prodempty" style="width:' + w(prodEmpty) + '"></span>'
    + '<span class="seg-idle" style="width:' + w(idle) + '"></span></div>'
    + '<div class="lifekey"><span><i class="k-stocked"></i>' + stocked + ' stocked</span>'
    + '<span><i class="k-prodempty"></i>' + prodEmpty + ' in-prod empty</span>'
    + '<span><i class="k-idle"></i>' + idle + ' idle</span></div>'
    + '</div>';
}

function renderAlerts() {
  const el = document.getElementById('inv-alerts');
  if (!el) return;
  let below = 0, err = 0, stale = 0;
  health.forEach((r) => {
    const st = healthState(r);
    if (st === 'below') below++;
    if (st === 'err') err++;
  });
  buckets.forEach((b) => { if (bucketAgeMs(b) > STALE_BAD_MS) stale++; });
  // Rejected-delta bins (deltas being refused: payload mismatch or stale epoch)
  // and bins parked staged past their own TTL — the SNF3 stranding signals.
  const rejected = anomalySummary.rejected_delta_bins || 0;
  const staleStaged = anomalySummary.stale_staged_bins || 0;
  const parts = [];
  if (below) parts.push('<b>' + below + '</b> payload' + (below > 1 ? 's' : '') + ' below threshold');
  if (err) parts.push('<b>' + err + '</b> ledger error' + (err > 1 ? 's' : ''));
  // The rejected-delta count is its own click target — it opens the drill listing
  // WHICH carriers are flagged (part, node, reason), not just a scroll.
  if (rejected) parts.push('<span data-action="showRejectedDeltas" style="text-decoration:underline;cursor:pointer"><b>' + rejected + '</b> rejected delta' + (rejected > 1 ? 's' : '') + '</span>');
  if (staleStaged) parts.push('<b>' + staleStaged + '</b> stale staged bin' + (staleStaged > 1 ? 's' : ''));
  if (stale) parts.push('<b>' + stale + '</b> stale bucket' + (stale > 1 ? 's' : ''));
  if (!parts.length) { el.innerHTML = ''; return; }
  el.innerHTML = '<div class="alerts-banner" data-action="scrollTo:' + (err || below ? 'rh' : 'buckets') + '">'
    + '<svg class="icon icon-16" aria-hidden="true"><use href="#icon-alert-triangle"></use></svg>'
    + '<span>' + parts.join(' · ') + ' — click to review</span></div>';
}

function renderHealth() {
  const body = document.getElementById('rh-body');
  if (!body) return;
  const rows = health.filter(passesFilters).slice();
  rows.sort((a, b) => {
    const ra = STATE_RANK[healthState(a)], rb = STATE_RANK[healthState(b)];
    if (ra !== rb) return ra - rb;
    return headroom(a) - headroom(b); // within a band, least headroom first
  });
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="9" class="dash-empty">'
      + (health.length ? 'No payloads match the filter.' : 'No monitored or stocked payloads.') + '</td></tr>';
    return;
  }
  body.innerHTML = rows.map(rhRowHtml).join('');
}
function headroom(r) { return r.on_hand - (r.threshold || 0); }

function meterHtml(r) {
  const st = healthState(r);
  if (st === 'err') {
    return '<div class="thr-meter thr-meter--err" title="In-loop total is negative — reconcile the bins ledger before trusting the threshold"></div>';
  }
  const scale = Math.max((r.threshold || 0) * 1.6, r.on_hand * 1.1, 1);
  const fill = Math.max(0, Math.min(100, (r.on_hand / scale) * 100));
  const cls = st === 'below' ? ' thr-meter--below' : st === 'near' ? ' thr-meter--near' : st === 'unset' ? ' thr-meter--unset' : '';
  let html = '<div class="thr-meter' + cls + '"><div class="thr-meter__fill" style="width:' + fill.toFixed(1) + '%"></div>';
  if (r.threshold > 0) {
    const tick = Math.max(0, Math.min(100, (r.threshold / scale) * 100));
    html += '<div class="thr-meter__tick" style="left:' + tick.toFixed(1) + '%"></div>';
  }
  return html + '</div>';
}
// ── ledger exceptions ─────────────────────────────────────────────────────
//
// Bins whose UoP ledger is negative RIGHT NOW. Blank on a good day, and that
// is the whole design — a list that stays empty until something is wrong is a
// better instrument than a panel that always shows something.
//
// This is upstream of the failure class the rest of the branch chases. A
// negative in-loop total means material moved off the books, so every number
// derived from it is computed from a reading nobody can trust: the total the
// threshold monitor decides on, and the gap the Edge sizes an order from.
//
// It does NOT stop replenishment. That behaviour existed and was removed —
// material having gone missing is exactly when the loop still needs
// restocking, and refusing to order let the payload genuinely run dry and a
// changeover arm on a dry source. What a broken count loses is the right to
// SIZE the order: the Edge clamps the gap at 0 rather than reading -443 as
// demand for 26 bins. Springfield's negative appeared on no surface at all,
// which is why nobody could act on it.
//
// Deliberately NOT clamped anywhere: negative is loudly wrong, and a clamp
// would make it silently wrong.
function renderLedgerExceptions() {
  const host = document.getElementById('rh-ledger-exceptions');
  if (!host) return;
  const openBins = (ledgerExceptions && ledgerExceptions.open_bins) || [];
  if (!openBins.length) { host.innerHTML = ''; return; }

  const rows = openBins.map((b) => {
    const since = b.negative_since
      ? ' since ' + new Date(b.negative_since).toLocaleString()
      // The crossing predates the audit trail — say so rather than implying it
      // just happened.
      : ' (start unknown)';
    const where = b.node_name ? ' at ' + escapeHtml(b.node_name) : '';
    return '<li><code>' + escapeHtml(b.label || ('bin ' + b.bin_id)) + '</code>'
      + (b.payload_code ? ' <span class="text-muted-xs">' + escapeHtml(b.payload_code) + '</span>' : '')
      + where + ' counts <b>' + b.uop_remaining + '</b>' + escapeHtml(since) + '</li>';
  }).join('');

  // Short. An earlier version explained overpacks and fork trucks in a
  // paragraph — that belongs in this comment, not on the page. It also
  // overstated: it claimed replenishment was suppressed, which was true of one
  // part rather than all three, and is no longer true of any.
  host.innerHTML = '<div class="ledger-exceptions">'
    + '<div class="ledger-exceptions__head">'
    + '<span class="chip chip-err">Count below zero</span> '
    + openBins.length + ' bin' + (openBins.length === 1 ? '' : 's')
    + ' reporting a negative count'
    + '</div>'
    + '<div class="ledger-exceptions__why">Ordering is unaffected. Recount to correct.</div>'
    + '<ul class="ledger-exceptions__list">' + rows + '</ul></div>';
}

// ── delta integrity ───────────────────────────────────────────────────────
//
// The mechanism panel. Its neighbour above says "this payload is negative";
// this says "and here is what happened to the counts". Read-side only: every
// dropped delta is already an observation row in bin_uop_audit (before == after,
// because the count did not move) with the dropped quantity in its metadata.
//
// TWO FIGURES, AND THEY MUST NOT BE MIXED.
//
//   UoP lost      — stale_epoch_dropped + payload_mismatch_dropped ONLY.
//   Mixed contents — payload_rebound_with_inventory, as a COUNT with no UoP
//                    total. A rebind is not a drop: the applier rebinds the
//                    payload and APPLIES the delta, so the tote's unit total
//                    stays correct and the bin is flagged for a cycle count of
//                    what is physically in it. Summing it into UoP lost
//                    inflates the figure and corrupts the one comparison this
//                    panel exists to make.
//
// "UoP lost" is a NET, signed toward the ledger: dropped credits minus dropped
// consumes, so a positive number means the count reads BELOW reality by that
// much — directly comparable to the negative total printed beside it. If a
// payload reads -443 and shows +443 lost, the answer is on the screen.
function renderDeltaIntegrity() {
  const host = document.getElementById('rh-delta-integrity');
  if (!host) return;
  const rows = (ledgerExceptions && ledgerExceptions.delta_integrity) || [];
  // A payload with nothing dropped and nothing rebound is not worth a row.
  const shown = rows.filter((r) => r.drop_rows > 0 || r.mixed_contents > 0);
  if (!shown.length) { host.innerHTML = ''; return; }

  // Worst first: the biggest ledger effect, then the biggest count of drops.
  shown.sort((a, b) => Math.abs(b.uop_lost) - Math.abs(a.uop_lost) || b.drop_rows - a.drop_rows);

  // ONE ROW IS FOUR ALIGNED CELLS, NOT A SENTENCE.
  //
  // This used to render each payload as a line of prose — "2199 UoP of count
  // never landed — reads above reality (2171 dropped deltas: 2170 stale epoch,
  // 1 payload mismatch)" — repeated seventeen times. Every row restated the
  // same two clauses around two numbers, so the numbers, which are the only
  // thing that varies, were the hardest part to find and could not be compared
  // down the column because nothing lined up.
  //
  // The prose moves to the tooltip, where the detail is still one hover away
  // for the row you care about, and the grid carries what you scan: which
  // part, how much, how many drops, and whether its ledger is negative too.
  // Direction moves onto an arrow beside the figure, legended once in the head
  // rather than spelled out per row.
  const items = shown.map((r) => {
    // Signed toward the ledger: >= 0 reads BELOW reality (credit never
    // landed), < 0 reads ABOVE (consumption never landed). Only `below` can
    // explain a negative ledger printed beside it — see the head note.
    const below = r.uop_lost >= 0;
    const arrow = below ? '▼' : '▲';
    const causes = [];
    if (r.stale_epoch_rows) causes.push(r.stale_epoch_rows + ' stale epoch');
    if (r.payload_mismatch_rows) causes.push(r.payload_mismatch_rows + ' payload mismatch');

    let uopCell = '';
    let dropCell = '';
    if (r.drop_rows > 0) {
      const tip = 'Reads ' + num(Math.abs(r.uop_lost)) + ' UoP '
        + (below ? 'BELOW' : 'ABOVE') + ' reality. '
        + 'Dropped credits ' + num(r.credits_dropped)
        + ', dropped consumes ' + num(r.consumes_dropped)
        + ', across ' + r.bins + ' bin' + (r.bins === 1 ? '' : 's') + '.'
        + (causes.length ? ' Cause: ' + causes.join(', ') + '.' : '');
      uopCell = '<span class="di-uop" title="' + escapeHtml(tip) + '">'
        + num(Math.abs(r.uop_lost))
        + ' <span class="di-dir' + (below ? ' di-dir--below' : '') + '">' + arrow + '</span></span>';
      dropCell = '<span class="di-drops">' + num(r.drop_rows) + '</span>';
    }

    // Flags column. Both entries live here so the numeric columns stay pure
    // numbers and keep aligning.
    let flags = '';
    // The comparison. Printing the ledger total beside the drop total is the
    // whole design: the two numbers matching IS the finding.
    if (r.ledger_total < 0) {
      flags += '<span class="di-ledger" title="This payload\'s ledger total is negative.">'
        + num(r.ledger_total) + '</span>';
    }
    if (r.mixed_contents > 0) {
      // Deliberately no UoP figure here. Nothing was lost — the delta was
      // applied — so a number would read as a loss and would be added to the
      // one above by anyone skimming.
      const tip = 'A bin\'s payload was rebound while it still held units under the old label. '
        + 'Counting CONTINUED and the unit total is correct; the bin needs a cycle count of what '
        + 'is physically in it. This is NOT lost count and has no UoP figure.';
      flags += '<span class="chip chip-warn" title="' + escapeHtml(tip) + '">'
        + r.mixed_contents + ' mixed</span>';
    }

    return '<div class="di-row">'
      + '<code class="di-pay">' + escapeHtml(r.payload_code) + '</code>'
      + uopCell + dropCell
      + '<span class="di-flags">' + flags + '</span>'
      + '</div>';
  }).join('');

  const totalDrops = shown.reduce((n, r) => n + r.drop_rows, 0);
  host.innerHTML = '<div class="delta-integrity">'
    + '<div class="delta-integrity__head">'
    + '<span class="chip chip-warn">Counts not landing</span> '
    + totalDrops + ' delta' + (totalDrops === 1 ? '' : 's')
    + ' dropped across ' + shown.length + ' payload' + (shown.length === 1 ? '' : 's')
    + '</div>'
    // THE LEGEND CARRIES WHAT THE PROSE USED TO, AND IT IS NOT OPTIONAL.
    //
    // Two paragraphs used to sit here. They are gone from the page because they
    // were re-read on every visit to learn nothing new — but the CLAIM they
    // guard against is still live, so it moves to the ⓘ rather than being
    // deleted:
    //
    //   Dropped consumes leave the count reading HIGH; only dropped credits
    //   leave it reading LOW. Springfield has 12,514 dropped consumes and has
    //   never recorded a single dropped credit, so every row here reads ABOVE
    //   while the ledgers beside them read negative — OPPOSITE directions. An
    //   earlier version invited closing one finding with the other's evidence.
    //
    // What stays visible is the arrow legend, because the arrows are unreadable
    // without it, and the one sentence a reader must not miss: ▲ and a negative
    // ledger are two problems, not one.
    + '<div class="delta-integrity__legend">'
    + '<b>▲</b> reads high · <b class="di-dir--below">▼</b> reads low — only <b>▼</b> explains a negative ledger'
    + ' <span class="di-info" title="'
    + escapeHtml('Direction is the reading. ABOVE (▲) means consumption never landed, so the count is too high. '
      + 'BELOW (▼) means credit never landed. Only BELOW can explain a negative ledger printed beside it — a payload '
      + 'that reads above while its ledger reads negative has two separate problems, not one.\n\n'
      + 'A large negative is an INDICATOR, NOT A DIAGNOSIS: it says ShinGo has been out of the loop on that part. '
      + 'Manual moves, robots down and sourcing gaps all read the same from here. It points at a cycle count, not at a cause.')
    + '">ⓘ</span></div>'
    + renderDeltaDaily()
    + '<div class="di-rows">'
    + '<div class="di-row di-row--head">'
    + '<span>Part</span><span class="di-uop">UoP</span>'
    + '<span class="di-drops">Drops</span><span class="di-flags">Ledger</span>'
    + '</div>'
    + items + '</div></div>';
}

// renderDeltaDaily draws the drop trend as one bar per plant-local day.
//
// A DAILY SERIES, NOT A WINDOW TOTAL, because the population is episodic and a
// total cannot say so. Bars are scaled against the largest ABSOLUTE net in the
// window, so a single incident day does not flatten every ordinary day to
// nothing while still being visibly the outlier.
//
// Gaps are left as gaps. Days with no drops are not in the data and are not
// drawn as zero-height bars: at Springfield those days are weekends and the
// plant being down, and a zero bar would assert a measured quiet day where
// there was no production at all. Absence is not zero — the number doctrine's
// rule 4, at the one place on this panel it can bite.
function renderDeltaDaily() {
  const days = (ledgerExceptions && ledgerExceptions.delta_daily) || [];
  if (!days.length) return '';

  const peak = days.reduce((m, d) => Math.max(m, Math.abs(d.net_delta)), 0);
  if (!peak) return '';

  const bars = days.map((d) => {
    const pct = Math.max(2, Math.round((Math.abs(d.net_delta) / peak) * 100));
    // Sign drives the class, not the magnitude: the two directions are
    // different findings and must not share a colour.
    const dir = d.net_delta < 0 ? 'dd-bar--above' : 'dd-bar--below';
    const causes = [];
    if (d.stale_epoch_rows) causes.push(d.stale_epoch_rows + ' stale epoch');
    if (d.payload_mismatch_rows) causes.push(d.payload_mismatch_rows + ' payload mismatch');
    const tip = d.day.slice(0, 10) + ': net ' + d.net_delta + ' UoP across '
      + d.drop_rows + ' dropped delta' + (d.drop_rows === 1 ? '' : 's')
      + (causes.length ? ' (' + causes.join(', ') + ')' : '')
      + ' · ' + d.payloads + ' payload' + (d.payloads === 1 ? '' : 's')
      + ', ' + d.bins + ' bin' + (d.bins === 1 ? '' : 's');
    return '<span class="dd-bar ' + dir + '" style="height:' + pct + '%" '
      + 'title="' + escapeHtml(tip) + '"></span>';
  }).join('');

  const net = days.reduce((n, d) => n + d.net_delta, 0);
  const first = days[0].day.slice(0, 10);
  const last = days[days.length - 1].day.slice(0, 10);
  return '<div class="delta-daily">'
    + '<div class="delta-daily__head">'
    + '<span class="cs-lbl">Daily net</span> '
    + '<span class="dd-net">' + net + ' UoP</span> '
    + '<span class="text-muted-xs">' + days.length + ' day'
    + (days.length === 1 ? '' : 's') + ' with drops · ' + first + ' → ' + last + '</span>'
    + '</div>'
    + '<div class="delta-daily__chart">' + bars + '</div></div>';
}

function chipsHtml(r) {
  const st = healthState(r);
  let chips = '';
  if (st === 'err') {
    // on_hand < 0. Ordering CONTINUES on this reading — a wrong count must not
    // starve a line — but the number is not real and needs a recount.
    const tip = 'On-hand is ' + r.on_hand + '. Ordering is unaffected; recount to correct.';
    chips += '<span class="chip chip-err" title="' + escapeHtml(tip) + '">Count below zero</span>';
  }
  else if (st === 'below') chips += '<span class="chip chip-below">Below — order due</span>';
  else if (st === 'near') chips += '<span class="chip chip-near">Near threshold</span>';
  else if (st === 'ok') chips += '<span class="chip chip-ok">OK</span>';
  else chips += '<span class="chip chip-muted">No threshold set</span>';
  if (r.ledger_negative && st !== 'err') {
    // on_hand (bins + lineside) can read healthy while the BINS side alone is
    // negative — bucket stock masking a broken bin ledger. The existing
    // on_hand < 0 test cannot see that, so it gets its own chip.
    const tip = 'Bins for this payload sum to ' + r.ledger_total + ', but lineside stock is '
      + 'masking it in the on-hand total. The bin ledger is still wrong — check this '
      + 'payload in the ledger exceptions above.';
    chips += ' <span class="chip chip-warn" title="' + escapeHtml(tip) + '">Bin ledger negative</span>';
  }
  if (r.swap_contradiction) {
    // P2-C9: an operator requested a manual swap for this payload while the
    // ledger reads it as fully stocked — the SNF3 phantom-on-hand shape. The
    // line may be starving behind a stale staged bin; check this payload's bins.
    const tip = 'A manual swap was requested for this payload while on-hand ('
      + r.on_hand + ') reads at or above threshold. The line may be starving '
      + 'behind a phantom on-hand — check this payload\'s bins for a stale staged bin.';
    chips += ' <span class="chip chip-warn" title="' + escapeHtml(tip) + '">Swap requested vs stocked ledger</span>';
  }
  return chips;
}
function rhRowHtml(r) {
  const hr = headroom(r);
  const open = expanded === r.payload_code;
  const nameCell = '<code>' + hl(r.payload_code) + '</code>'
    + (r.description ? ' <span class="text-muted-xs">' + hl(r.description) + '</span>' : '');
  let out = '<tr class="rh-row' + (open ? ' open' : '') + '" data-action="toggleRow:' + escapeHtml(r.payload_code) + '">'
    + '<td><span class="chev"><svg class="icon" aria-hidden="true"><use href="#icon-chevron-right"></use></svg></span></td>'
    + '<td>' + nameCell + '</td>'
    + '<td><code>' + hl(catIdFor(r.payload_code)) + '</code></td>'
    + '<td>' + meterHtml(r) + '</td>'
    + '<td class="rh-num">' + (r.on_hand < 0 ? '<span class="rh-neg">' + num(r.on_hand) + '</span>' : num(r.on_hand))
    + '<div class="rh-split">bins ' + num(r.bin_uop) + ' + line ' + num(r.bucket_uop) + '</div></td>'
    + '<td class="rh-num">' + (r.threshold > 0 ? num(r.threshold) : '<span class="text-muted">—</span>') + '</td>'
    + '<td class="rh-num' + (hr < 0 ? ' rh-neg' : '') + '">' + (r.threshold > 0 ? (hr >= 0 ? '+' : '') + num(hr) : '—') + '</td>'
    + '<td>' + chipsHtml(r) + '</td>'
    + '<td><button class="view-detail" data-action="openDrill:' + escapeHtml(r.payload_code) + '">detail <svg class="icon" aria-hidden="true"><use href="#icon-chevron-right"></use></svg></button></td>'
    + '</tr>';
  if (open) out += editorRowHtml(r);
  return out;
}
function catIdFor(pc) {
  const b = (binsByPayload[pc] || []).find((x) => x.cat_id);
  return b ? b.cat_id : '';
}

// Expanded editor: per-loader threshold rows (explicit Save/Discard + Calc) plus
// the payload's holding bins.
function editorRowHtml(r) {
  const cfg = loaderRowsFor(r.payload_code);
  let editor = '<div class="text-muted-sm mb-2">Thresholds for <code>' + escapeHtml(r.payload_code)
    + '</code> — per Core-owned loader. Saving re-derives demand and pushes to the edge on the next sync.</div>';
  if (!cfg.length) {
    editor += '<div class="text-muted-sm">No Core-owned loader carries this payload. Add it on the '
      + '<a href="/nodes">Nodes page</a> to set a threshold.</div>';
  } else {
    editor += '<table class="thr-edit-table"><thead><tr><th></th><th>Loader</th><th>Node</th><th>Kind</th>'
      + '<th>Cycle (s)</th><th></th><th>UoP threshold</th><th>Min stock</th><th></th></tr></thead><tbody>'
      + cfg.map(thrRowHtml).join('') + '</tbody></table>';
  }
  editor += holdingBinsHtml(r.payload_code);
  return '<tr class="rh-editor"><td colspan="9">' + editor + '</td></tr>';
}
function loaderRowsFor(pc) {
  const rows = [];
  loaders.forEach((item) => {
    const l = item.loader;
    (item.payloads || []).forEach((p) => {
      if (p.payload_code === pc) rows.push({ l, node: l.core_node_name, kind: 'payload', thr: p.uop_threshold || 0, ms: p.min_stock || 0 });
    });
    (item.homes || []).forEach((hm) => {
      if (hm.payload_code === pc) {
        rows.push({ l, node: nodesById[hm.position_node_id] || ('node#' + hm.position_node_id), kind: 'home', thr: hm.uop_threshold || 0, ms: 0, positionNodeId: hm.position_node_id });
      }
    });
  });
  return rows;
}
function thrRowHtml(c) {
  const key = c.l.id + '|' + c.kind + '|' + c.node;
  const data = 'data-lid="' + c.l.id + '" data-kind="' + c.kind + '" data-node="' + escapeHtml(c.node) + '"'
    + ' data-pnid="' + (c.positionNodeId || '') + '" data-anchor="' + escapeHtml(c.l.core_node_name || '') + '"'
    + ' data-ms="' + (c.ms || 0) + '"';
  return '<tr data-thr-key="' + escapeHtml(key) + '">'
    + '<td><span class="dirty-dot" style="visibility:hidden"></span></td>'
    + '<td>' + escapeHtml(c.l.name || '') + '</td>'
    + '<td><code>' + escapeHtml(c.node) + '</code></td>'
    + '<td><span class="kind-pill">' + c.kind + '</span></td>'
    + '<td><input type="number" class="form-input thr-cycle" placeholder="cycle" ' + data + ' style="width:70px"></td>'
    + '<td><button class="btn btn-sm" data-action="calcThr" ' + data + '>Calc</button></td>'
    + '<td><input type="number" class="form-input thr-value" value="' + c.thr + '" data-orig="' + c.thr + '" ' + data
    + ' data-action-input="onThrInput"></td>'
    + '<td><input type="number" class="form-input thr-ms" value="' + (c.ms || 0) + '"' + (c.kind === 'home' ? ' disabled title="Min stock applies to payload rows only"' : '') + ' style="width:70px"></td>'
    + '<td class="nowrap"><button class="btn btn-sm btn-primary thr-save" data-action="saveThr" ' + data + ' style="visibility:hidden">Save</button> '
    + '<button class="btn btn-sm thr-discard" data-action="discardThr" style="visibility:hidden">Discard</button></td>'
    + '</tr>';
}
function holdingBinsHtml(pc) {
  const list = binsByPayload[pc] || [];
  if (!list.length) return '<div class="holding-title">Holding bins</div><div class="text-muted-sm">None on hand.</div>';
  const rows = list.map((b) =>
    '<tr><td><a href="/bins">' + escapeHtml(b.bin_label || '') + '</a></td>'
    + '<td><code>' + escapeHtml(b.node_name || '') + '</code></td>'
    + '<td>' + escapeHtml(b.zone || '') + '</td>'
    + '<td class="rh-num">' + num(b.uop_remaining) + '</td>'
    + '<td><span class="badge badge-' + escapeHtml(b.status || '') + '">' + escapeHtml(b.status || '') + '</span>'
    + (b.in_transit ? ' <span class="chip chip-near">in transit</span>' : '') + '</td></tr>').join('');
  return '<div class="holding-title">Holding bins (' + list.length + ')</div>'
    + '<table class="holding-table"><thead><tr><th>Bin</th><th>Node</th><th>Zone</th><th class="rh-num">UoP</th><th>Status</th></tr></thead>'
    + '<tbody>' + rows + '</tbody></table>';
}

function bucketAgeMs(b) {
  const t = b.updated_at ? new Date(b.updated_at).getTime() : NaN;
  return isFinite(t) ? (Date.now() - t) : 0;
}
function renderBuckets() {
  const body = document.getElementById('buckets-body');
  if (!body) return;
  const rows = buckets.filter((b) => {
    if (zoneFilter && b.zone !== zoneFilter) return false;
    if (groupFilter && b.group_name !== groupFilter) return false;
    if (!searchTerm) return true;
    const t = searchTerm.toLowerCase();
    return (b.part_number || '').toLowerCase().includes(t)
      || (b.payload_code || '').toLowerCase().includes(t)
      || (b.node_name || '').toLowerCase().includes(t);
  });
  if (!rows.length) {
    body.innerHTML = '<tr><td colspan="8" class="dash-empty">'
      + (buckets.length ? 'No buckets match the filter.' : 'No lineside buckets.') + '</td></tr>';
    return;
  }
  body.innerHTML = rows.map((b) => {
    const age = bucketAgeMs(b);
    const stale = age > STALE_BAD_MS;
    const ageCls = stale ? 'stale-30' : age > STALE_WARN_MS ? 'stale-7' : '';
    const ageText = b.updated_at ? timeAgo(b.updated_at) : '—';
    return '<tr' + (stale ? ' class="row-stale"' : '') + '>'
      + '<td>' + hl(b.group_name || '—') + '</td>'
      + '<td>' + hl(b.station || '') + '</td>'
      + '<td><code>' + hl(b.node_name || '') + '</code></td>'
      + '<td><code>' + hl(b.part_number || b.payload_code || '') + '</code></td>'
      + '<td><span class="badge ' + (b.state === 'stranded' ? 'badge-flagged' : 'badge-available') + '">' + escapeHtml(b.state || 'active') + '</span></td>'
      + '<td class="rh-num">' + num(b.qty) + '</td>'
      + '<td class="' + ageCls + '"' + (b.updated_at ? ' title="' + escapeHtml(new Date(b.updated_at).toLocaleString()) + '"' : '')
      + '>' + escapeHtml(ageText) + (stale ? ' · <b>stale</b>' : '') + '</td>'
      + (isAuth ? '<td><button class="btn btn-sm btn-danger" data-action="deleteBucket:' + b.id + '">Delete</button></td>' : '<td></td>')
      + '</tr>';
  }).join('');
}

// ── formatting ─────────────────────────────────────────────────────────────
function num(n) { return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString(); }

// ── interactions ───────────────────────────────────────────────────────────
const onSearch = debounce((el) => { searchTerm = (el.value || '').trim(); renderHealth(); renderBuckets(); }, 150);
function onSearchKey() { /* the document-level '/' shortcut handles focus */ }
function onFilter() {
  zoneFilter = (document.getElementById('inv-zone') || {}).value || '';
  groupFilter = (document.getElementById('inv-group') || {}).value || '';
  renderHealth();
  renderBuckets();
}
function refresh() { loadAll(); }
function scrollTo(id) {
  const el = document.getElementById(id);
  if (el) el.scrollIntoView({ behavior: 'smooth', block: 'start' });
}
function exportInventory() { window.location = '/api/inventory/export'; }

function toggleRow(pc) {
  expanded = expanded === pc ? null : pc;
  renderHealth();
}

// Threshold editing: explicit save. Typing marks the row dirty (shows Save /
// Discard); nothing persists until Save.
function onThrInput(el) { setRowDirty(el.closest('tr'), true); }
function setRowDirty(tr, dirty) {
  if (!tr) return;
  const dot = tr.querySelector('.dirty-dot');
  const save = tr.querySelector('.thr-save');
  const disc = tr.querySelector('.thr-discard');
  tr.classList.toggle('row-dirty', dirty);
  if (dot) dot.style.visibility = dirty ? 'visible' : 'hidden';
  if (save) save.style.visibility = dirty ? 'visible' : 'hidden';
  if (disc) disc.style.visibility = dirty ? 'visible' : 'hidden';
}
async function saveThr(el) {
  const tr = el.closest('tr');
  const thr = Number(tr.querySelector('.thr-value').value) || 0;
  const d = el.dataset;
  try {
    if (d.kind === 'home') {
      await apiPost('/api/loader/set-home', { loader_id: Number(d.lid), position_node_id: Number(d.pnid), payload_code: expanded, uop_threshold: thr });
    } else {
      const ms = Number(tr.querySelector('.thr-ms').value) || 0;
      await apiPost('/api/loader/set-payload', { loader_id: Number(d.lid), payload_code: expanded, uop_threshold: thr, min_stock: ms });
    }
    toast('Threshold saved — demand re-derived, edge push on next sync', 'success');
    await loadAll(true);
  } catch (e) {
    toast('Save failed: ' + (e.message || e), 'error', { sticky: true });
  }
}
function discardThr(el) {
  const tr = el.closest('tr');
  const input = tr.querySelector('.thr-value');
  input.value = input.dataset.orig;
  setRowDirty(tr, false);
  closeCalcPop(tr);
}

// Calc: suggest a threshold from observed lead times; the operator applies it
// (which just fills the field + marks dirty), then Saves deliberately.
async function calcThr(el) {
  const tr = el.closest('tr');
  const cyc = Number(tr.querySelector('.thr-cycle').value);
  if (!cyc || cyc <= 0) { toast('Enter cycle seconds first (the box left of Calc)', 'warning'); return; }
  try {
    const d = await apiPost('/api/loader/calculate', { core_node_name: el.dataset.anchor, payload_code: expanded, cycle_seconds: cyc, days: 14 });
    if (d && d.error) { toast(d.error, 'error'); return; }
    const l1 = (d && d.outputs && d.outputs.L1Threshold) || 0;
    showCalcPop(tr, l1, d ? d.confidence : '');
  } catch (e) {
    toast('Calc failed: ' + (e.message || e), 'error');
  }
}
function showCalcPop(tr, value, confidence) {
  closeCalcPop(tr);
  const conf = String(confidence || '').toLowerCase();
  const pop = document.createElement('div');
  pop.className = 'calc-pop';
  pop.innerHTML = 'Suggested UoP threshold: <b>' + value + '</b> &nbsp;·&nbsp; confidence '
    + '<b class="' + (conf === 'high' ? 'conf-high' : 'conf-low') + '">' + escapeHtml(confidence || 'n/a') + '</b>'
    + '<div class="text-muted-sm mt-2">From the observed lead times over 14 days. Nothing is saved until you Apply, then Save.</div>'
    + '<button class="btn btn-sm btn-primary mt-2" data-action="applyCalc:' + value + '">Apply ' + value + '</button> '
    + '<button class="btn btn-sm mt-2" data-action="dismissCalc">Dismiss</button>';
  tr.querySelector('.thr-value').closest('td').appendChild(pop);
}
function closeCalcPop(scope) {
  (scope || document).querySelectorAll('.calc-pop').forEach((p) => p.remove());
}
function applyCalc(value, el) {
  const tr = el.closest('tr');
  tr.querySelector('.thr-value').value = value;
  setRowDirty(tr, true);
  closeCalcPop(tr);
}
function dismissCalc(el) { closeCalcPop(el.closest('tr')); }

async function deleteBucket(id) {
  if (!await uiConfirm('Delete this lineside bucket row? This clears a Core-only ghost record.')) return;
  try {
    await apiPost('/api/buckets/delete', { id: Number(id) });
    toast('Bucket deleted', 'success');
    await loadAll(true);
  } catch (e) {
    toast('Delete failed: ' + (e.message || e), 'error', { sticky: true });
  }
}

// ── consumption / cover drill ──────────────────────────────────────────────
function openDrill(pc) {
  drillPayload = pc;
  drillDays = 14;
  // The rejected-delta view hides the range toggle; restore it for the
  // consumption drill (the two views share the #inv-drill modal).
  const range = document.getElementById('drill-range');
  if (range) range.style.display = '';
  document.querySelectorAll('#drill-range button').forEach((b) => b.classList.toggle('is-active', b.dataset.action === 'drillRange:14'));
  const title = document.getElementById('drill-title');
  if (title) title.textContent = pc + ' — consumption & cover';
  document.getElementById('inv-drill').classList.add('active');
  renderDrill();
}
function drillRange(days, el) {
  drillDays = Number(days) || 14;
  document.querySelectorAll('#drill-range button').forEach((b) => b.classList.remove('is-active'));
  if (el) el.classList.add('is-active');
  renderDrill();
}
async function renderDrill() {
  const detail = document.getElementById('drill-detail');
  const narr = document.getElementById('drill-narrative');
  const r = health.find((x) => x.payload_code === drillPayload);
  if (!r || !detail) return;
  detail.innerHTML = '<div class="dash-empty">Loading…</div>';
  const since = new Date(Date.now() - drillDays * 24 * 3600 * 1000).toISOString();
  const until = new Date().toISOString();
  let consumed = null, perDay = null, cover = null;
  try {
    const resp = await apiGet('/api/parts/consumption?top=500&since=' + encodeURIComponent(since) + '&until=' + encodeURIComponent(until));
    const rows = (resp && resp.rows) || [];
    // payload_code and part_number are different keys; match by code where a
    // part happens to share it, else leave consumption unknown. A real trend
    // needs a per-payload daily series endpoint (see the TODO below).
    const hit = rows.find((x) => x.part_number === drillPayload);
    if (hit) {
      consumed = hit.uop;
      perDay = consumed / drillDays;
      cover = perDay > 0 ? (r.on_hand / perDay) : null;
    }
  } catch (e) { /* consumption is best-effort */ }

  detail.innerHTML =
    '<div class="ov-support">'
    + supp('On-hand', num(r.on_hand) + ' UoP') + supp('Threshold', r.threshold > 0 ? num(r.threshold) : '—')
    + supp('Headroom', r.threshold > 0 ? num(headroom(r)) : '—')
    + supp('Consumed (' + drillDays + 'd)', consumed == null ? 'n/a' : num(consumed) + ' UoP')
    + supp('Per day', perDay == null ? 'n/a' : perDay.toFixed(1))
    + supp('Days of cover', cover == null ? 'n/a' : cover.toFixed(1))
    + '</div>'
    + '<div class="mt-3">' + meterHtml(r) + '</div>'
    + '<button class="btn btn-sm mt-3" data-action="showOnMap:' + escapeHtml(drillPayload) + '">show on map <svg class="icon icon-16" aria-hidden="true"><use href="#icon-arrow-up-right"></use></svg></button>';

  if (narr) {
    narr.innerHTML = consumed == null
      ? 'No consumption match for this payload over the last ' + drillDays + ' days. '
        + 'Consumption is recorded per part number, which does not map 1:1 to a payload code — a per-payload daily '
        + 'series would give a real trend line here.'
      : 'Consuming about <b>' + perDay.toFixed(1) + ' UoP/day</b> over ' + drillDays + ' days. '
        + 'At that rate, on-hand (' + num(r.on_hand) + ') covers <b>' + cover.toFixed(1) + ' days</b>'
        + (r.threshold > 0 ? '; the threshold is ' + num(r.threshold) + ' UoP.' : '.');
  }
}
function supp(label, value) {
  return '<div class="ov-support__item"><div class="ov-support__value">' + value + '</div>'
    + '<div class="ov-support__label">' + label + '</div></div>';
}
// TODO (inventory v2): the drill wants a per-payload daily consumption series to
// draw a real trend line with the threshold overlaid. /api/parts/consumption
// returns only per-part window totals today, and part_number does not map 1:1 to
// payload_code, so this shows window totals + days-of-cover instead of a chart.
function showOnMap() {
  // Deep-link stub for the map material layer (a later map task). The map does
  // not consume a payload highlight yet, so this just opens the map hub.
  // "/" is the hub, and this opened /dashboards — a route that has redirected
  // there since refactor #3 AND sits behind auth, so the promised "opening the
  // map" landed a signed-out reader on the login page. Go to the hub directly.
  toast('Map material-layer highlight is not wired yet — opening the hub.', 'info');
  window.open('/', '_blank');
}
function closeDrill() { document.getElementById('inv-drill').classList.remove('active'); }

// ── rejected-delta drill ───────────────────────────────────────────────────
// The "N rejected deltas" banner count was a dead scroll — it never told the
// operator WHICH carrier was flagged. This lists them: carrier, part, node, why
// (stale epoch / payload mismatch), how many drops, and when — so they know
// exactly what to cycle-count. Reuses the #inv-drill modal shell.
function reasonLabel(op) {
  if (op === 'stale_epoch_dropped') return 'Stale epoch — bin was rebound; deltas from the old count are dropped';
  if (op === 'payload_mismatch_dropped') return 'Payload mismatch — the delta’s part differs from the bin’s';
  return 'Flagged (no recent drop logged)';
}
async function showRejectedDeltas() {
  const modal = document.getElementById('inv-drill');
  const detail = document.getElementById('drill-detail');
  const title = document.getElementById('drill-title');
  const range = document.getElementById('drill-range');
  const narr = document.getElementById('drill-narrative');
  if (!modal || !detail) return;
  if (title) title.textContent = 'Rejected deltas — carriers whose counts are being refused';
  if (range) range.style.display = 'none';
  if (narr) narr.innerHTML = 'These carriers have live counter deltas the system is dropping, so their on-hand '
    + 'can drift from what is physically in the bin. Cycle-count the carrier at its node to clear the flag and realign the count.';
  detail.innerHTML = '<div class="dash-empty">Loading…</div>';
  modal.classList.add('active');
  let rows;
  try {
    rows = await apiGet('/api/inventory/rejected-deltas');
  } catch (e) {
    detail.innerHTML = '<div class="dash-empty">Failed to load: ' + escapeHtml(e.message || String(e)) + '</div>';
    return;
  }
  if (!rows || !rows.length) {
    detail.innerHTML = '<div class="dash-empty">No carriers are currently flagged.</div>';
    return;
  }
  detail.innerHTML = '<div class="table-scroll-y"><table class="rh-table"><thead><tr>'
    + '<th>Carrier</th><th>Part</th><th>Node</th><th>Reason</th><th>Drops</th><th>Flagged</th><th>Last reject</th>'
    + '</tr></thead><tbody>'
    + rows.map((b) =>
      '<tr><td>' + escapeHtml(b.bin_label || ('#' + b.bin_id)) + '</td>'
      + '<td>' + escapeHtml(b.payload_code || '—') + '</td>'
      + '<td>' + escapeHtml(b.node_name || '—') + '</td>'
      + '<td>' + escapeHtml(reasonLabel(b.reason)) + '</td>'
      + '<td>' + (b.drop_count || 0) + '</td>'
      + '<td class="nowrap">' + timeAgo(b.anomaly_at) + '</td>'
      + '<td class="nowrap">' + (b.last_reject_at ? timeAgo(b.last_reject_at) : '—') + '</td></tr>').join('')
    + '</tbody></table></div>';
}

// ── init + live updates ────────────────────────────────────────────────────
delegateActions(document.body, {
  onSearch, onSearchKey, onFilter, refresh, exportInventory, scrollTo,
  toggleRow, onThrInput, saveThr, discardThr, calcThr, applyCalc, dismissCalc,
  deleteBucket, openDrill, drillRange, showOnMap, showRejectedDeltas,
  'close-modal': closeDrill,
}, { events: ['click', 'change', 'input', 'keydown'] });

// '/' focuses the search box from anywhere (unless already typing in a field).
document.addEventListener('keydown', (e) => {
  if (e.key === '/' && !/^(INPUT|TEXTAREA|SELECT)$/.test((e.target.tagName || ''))) {
    e.preventDefault();
    const s = document.getElementById('inv-search');
    if (s) s.focus();
  } else if (e.key === 'Escape') {
    closeDrill();
  }
});

// Live: refresh on order/inventory events; a light interval as a fallback so the
// "as of" stamp stays honest even without live traffic.
const live = document.getElementById('inv-live');
function markLive() { if (live) { live.textContent = 'live'; live.classList.add('is-live'); } }
onSSE('order-update', () => { markLive(); loadAll(true); });
onSSE('connected', () => { markLive(); loadAll(true); });
setInterval(() => loadAll(true), 20000);

loadAll();
