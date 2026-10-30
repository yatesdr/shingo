// Count history (/inventory/count-history): the count ledger lists that left
// /inventory on 2026-10-29, moved as they were. /inventory is in-loop UoP
// against the UoP threshold and nothing else; what the counts did — bins below
// zero, dropped deltas, report divergences, dropped holds, declared-full
// stamps — is read here, with the carriers flagged for a recount and the tick
// feed until the owner rules where those two go. (The lineside piles went to
// the Bins page and the keeper's levels to the Nodes page, owner 2026-10-29.)
//
// The rows name the node, never the Edge's station id (the station id is a
// plant's configuration, and at its default it says nothing a reader can use).
//
// Data sources:
//   /api/inventory/ledger-exceptions — the count ledger lists.
//   /api/inventory/anomaly-summary   — carriers flagged, stale staged, undeclared.

import { apiGet, escapeHtml, delegateActions, toast, timeAgoHTML } from '/static/app.js';
import { formatClock, formatDuration, formatTime, onSSE, serverNow } from '/static/shared/utils.js';
import { createLiveSchedule } from '/static/pages/live-schedule.js';

// ── state ──────────────────────────────────────────────────────────────
let ledgerExceptions = {}; // /api/inventory/ledger-exceptions
let anomalySummary = {};   // /api/inventory/anomaly-summary (flagged/stale counts)

let lastLoadAt = 0;

async function loadAll(quiet) {
  lastLoadAt = Date.now();
  try {
    const [lx, an] = await Promise.all([
      apiGet('/api/inventory/ledger-exceptions').catch(() => ({})),
      apiGet('/api/inventory/anomaly-summary').catch(() => ({})),
    ]);
    ledgerExceptions = lx && typeof lx === 'object' ? lx : {};
    anomalySummary = an && typeof an === 'object' ? an : {};
    renderAll();
  } catch (e) {
    if (!quiet) toast('Failed to load count history: ' + (e.message || e), 'error', { sticky: true });
  }
}

function renderAll() {
  renderAlerts();
  renderLedgerExceptions();
  renderDeltaIntegrity();
  renderReportDivergence();
  renderHoldDrops();
  renderDeclaredStamps();
  const asof = document.getElementById('ch-asof');
  if (asof) asof.textContent = 'as of ' + formatClock(serverNow());
}

// The carrier-level counts that used to ride /inventory's alerts banner.
// Carriers flagged anomaly_at (a refused delta — payload mismatch or stale
// epoch — or a rebind that left mixed contents) and bins parked staged past
// their own TTL — the SNF3 stranding signals.
//
// CARRIERS, NOT DELTAS. This figure is a live count of flagged bins with no
// window, beside the panel's "M deltas dropped" with its window; the two never
// agree because they are different populations (pinned by
// TestInventory_RejectedAndDroppedCountDifferentPopulations).
function renderAlerts() {
  const el = document.getElementById('ch-alerts');
  if (!el) return;
  const rejected = anomalySummary.rejected_delta_bins || 0;
  const staleStaged = anomalySummary.stale_staged_bins || 0;
  // Carriers holding a payload their bin type is not declared to carry: they
  // count nowhere and no sourcing reader will fetch them. /material-flags
  // lists which; the fix (move the parts, or declare the bin type on the
  // payload) is not something this page can do, so it is a link.
  const undeclared = anomalySummary.undeclared_carrier_bins || 0;
  const parts = [];
  // The flagged-carrier count opens the drill listing WHICH carriers.
  if (rejected) parts.push('<button type="button" class="link-btn" data-action="showRejectedDeltas"><b>' + rejected + '</b> carrier' + (rejected > 1 ? 's' : '') + ' flagged for a recount</button>');
  if (staleStaged) parts.push('<b>' + staleStaged + '</b> stale staged bin' + (staleStaged > 1 ? 's' : ''));
  if (undeclared) parts.push('<a href="/material-flags"><b>' + undeclared + '</b> bin' + (undeclared > 1 ? 's' : '') + ' in undeclared carriers</a>');
  if (!parts.length) { el.innerHTML = ''; return; }
  el.innerHTML = '<div class="alerts-banner">'
    + '<svg class="icon icon-16" aria-hidden="true"><use href="#icon-alert-triangle"></use></svg>'
    + '<span>' + parts.join(' · ') + '</span></div>';
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
      ? ' since ' + formatTime(b.negative_since)
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
//   UoP lost      — stale_epoch_dropped + payload_mismatch_dropped ONLY, less
//                    uop_recovered: mismatch-refused units the running net
//                    landed with the bin's next accepted count.
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

  // ONE ROW IS FOUR ALIGNED CELLS, NOT A SENTENCE: which part, how much, how
  // many drops, and whether its ledger is negative too. The prose is on the
  // row's tooltip. Direction is a word beside the figure ("high" / "low"):
  // the arrow glyphs and their legend went with /inventory's Daily net
  // (2026-10-29), and a word needs no key.
  const items = shown.map((r) => {
    // Signed toward the ledger: >= 0 reads BELOW reality (credit never
    // landed), < 0 reads ABOVE (consumption never landed). Only `below` can
    // explain a negative ledger printed beside it — see the head note.
    const below = r.uop_lost >= 0;
    const dir = below ? 'low' : 'high';
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
        + (r.uop_recovered ? ' Recovered ' + num(Math.abs(r.uop_recovered))
          + ' refused UoP that landed with a later count, already taken off the figure.' : '')
        + (causes.length ? ' Cause: ' + causes.join(', ') + '.' : '');
      uopCell = '<span class="di-uop" title="' + escapeHtml(tip) + '">'
        + num(Math.abs(r.uop_lost))
        + ' <span class="di-dir' + (below ? ' di-dir--below' : '') + '">' + dir + '</span></span>';
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

  // A COUNT IS EXACT OF A STATED WINDOW (guide, "The numbers themselves"): the
  // rows are drops since ledgerExceptions.since, so the head says since when.
  const totalDrops = shown.reduce((n, r) => n + r.drop_rows, 0);
  const since = ledgerExceptions && ledgerExceptions.since;
  host.innerHTML = '<div class="delta-integrity">'
    + '<div class="delta-integrity__head">'
    + '<span class="chip chip-warn">Counts not landing</span> '
    + totalDrops + ' delta' + (totalDrops === 1 ? '' : 's')
    + ' dropped across ' + shown.length + ' payload' + (shown.length === 1 ? '' : 's')
    + (since ? ' since ' + escapeHtml(formatTime(since)) : '')
    // Dropped consumes leave the count reading HIGH; only dropped credits leave
    // it reading LOW, and only LOW can explain a negative ledger printed beside
    // it — a payload that reads high while its ledger reads negative has two
    // problems, not one. Said once, on the head's ⓘ.
    + ' <span class="di-info" title="'
    + escapeHtml('Direction is the reading. HIGH means consumption never landed, so the count is too high. '
      + 'LOW means credit never landed. Only LOW can explain a negative ledger printed beside it — a payload '
      + 'that reads high while its ledger reads negative has two separate problems, not one.\n\n'
      + 'A large negative is an INDICATOR, NOT A DIAGNOSIS: it says ShinGo has been out of the loop on that part. '
      + 'Manual moves, robots down and sourcing gaps all read the same from here. It points at a cycle count, not at a cause.')
    + '">ⓘ</span>'
    + '</div>'
    + '<div class="di-rows">'
    + '<div class="di-row di-row--head">'
    + '<span>Part</span><span class="di-uop">UoP</span>'
    + '<span class="di-drops">Drops</span><span class="di-flags">Ledger</span>'
    + '</div>'
    + items + '</div></div>';
}

// ── report divergence ─────────────────────────────────────────────────────
//
// The lineside report is a checksum: each minute the Edge says which carrier
// it has bound at each consume seat and what it counts, and Core compares that
// against its own count. A disagreement is an episode, open until a later
// report agrees. Replenishment is decided off Core's count either way, so a
// row here is a count to go and check, not an order that did or did not go.
// Blank on a good day. Values are as first seen when the episode opened.
const DIVERGENCE_CLASS = {
  count: 'count differs',
  epoch: 'Edge counting an old load',
  not_at_seat: 'Edge carrier not here in Core',
  unbound_carrier: 'Core carrier not bound at the Edge',
  empty_seat: 'Core carrier where the Edge has nothing',
  bucket: 'lineside bucket differs',
};

// openFor is how long an episode has been open. opened_at is a SERVER stamp, so
// it is differenced against serverNow(), never the browser's Date.now(); the
// duration is the shared ladder's.
function openFor(iso) {
  return formatDuration(serverNow() - new Date(iso).getTime());
}

function renderReportDivergence() {
  const host = document.getElementById('rh-report-divergence');
  if (!host) return;
  const rows = (ledgerExceptions && ledgerExceptions.report_divergences) || [];
  if (!rows.length) { host.innerHTML = ''; return; }

  const items = rows.map((d) => {
    const bin = d.bin_id ? (d.bin_label || ('bin ' + d.bin_id)) : 'bucket';
    const epoch = d.edge_epoch !== null && d.edge_epoch !== undefined
      && d.core_epoch !== null && d.core_epoch !== undefined && d.edge_epoch !== d.core_epoch
      ? ' · epoch Edge ' + d.edge_epoch + ' / Core ' + d.core_epoch
      : (d.core_epoch !== null && d.core_epoch !== undefined ? ' · epoch ' + d.core_epoch : '');
    return '<li><code>' + escapeHtml(bin) + '</code>'
      + ' <span class="text-muted-xs">' + escapeHtml(d.payload_code || '') + '</span>'
      + (d.node ? ' at ' + escapeHtml(d.node) : '')
      + ' — ' + escapeHtml(DIVERGENCE_CLASS[d.class] || d.class)
      + ': Edge <b>' + num(d.edge_count) + '</b>, Core <b>' + num(d.core_count) + '</b>'
      + escapeHtml(epoch)
      + ' <span class="text-muted-xs">open ' + escapeHtml(openFor(d.opened_at))
    + ' (since ' + escapeHtml(formatTime(d.opened_at)) + ')</span>'
      + '</li>';
  }).join('');

  host.innerHTML = '<div class="ledger-exceptions">'
    + '<div class="ledger-exceptions__head">'
    + '<span class="chip chip-warn">Inventory count anomalies</span> '
    + rows.length + ' open'
    + '</div>'
    + '<div class="ledger-exceptions__why">Ordering reads the Core count. Recount the carrier to correct.</div>'
    + '<ul class="ledger-exceptions__list">' + items + '</ul></div>';
}

// ── strict pack: dropped holds and declared-full stamps ───────────────────
//
// Two routine lists, read together. A produce seat whose bin is at its pack
// holds the strokes past it and replays them onto the next bin; a hold that
// cannot be replayed is dropped, and the Edge says so (hold_dropped). A station
// that releases a bin FULL below its pack ships the count and the declaration,
// and Core stamps the bin up to its part's pack (release_declared_full); a part
// with no pack keeps its count, and the row says so. A dropped hold
// makes the next bin short and that bin is then stamped, so the two rows can
// describe the same parts: every row shows its node and its time together.
//
// NOT ALARMS. Both happen on a normal shift, so neither carries a red chip and
// neither is blank on a good day: an empty window reads as one quiet line, so
// "nothing happened" is visible as an answer rather than as a missing panel.
// Both lists cover the same window as the excursions (ledgerExceptions.since).
const HOLD_DROP_REASON = {
  no_carrier: 'no carrier coming',
  sweep: 'no carrier coming (sweep)',
  backstop: 'over a pack at replay',
  hold_cap: 'over a pack while the bin was full',
  cutover: 'cleared at changeover',
  foreign_hold: 'another part beside the bin',
  flip_carry_overflow: 'did not fit the partner bin',
  no_bin: 'made with no bin to count into',
};

// HOLD_DROP_RUNS_SHOWN is how many of a line's runs are spelled out; the rest
// are summed into "and N earlier".
const HOLD_DROP_RUNS_SHOWN = 6;

// renderHoldDrops draws the hold drops as Core groups them: runs, each a seat,
// part, reason and station with its drops a stroke or so apart (a line at its
// pack with a pack held drops a part a stroke until a bin comes). One line per
// seat, part and reason, with its total, then its runs, latest first. The
// totals count every drop in the window; Core's limit bounds the runs only.
function renderHoldDrops() {
  const host = document.getElementById('rh-hold-drops');
  if (!host) return;
  const runs = (ledgerExceptions && ledgerExceptions.hold_drop_runs) || [];
  const since = ledgerExceptions && ledgerExceptions.since;
  const span = since ? ' since ' + escapeHtml(formatTime(since)) : '';
  if (!runs.length) {
    host.innerHTML = '<div class="ledger-routine text-muted-sm">No hold drops' + (span || ' in the window') + '.</div>';
    return;
  }
  const lines = [];
  const byKey = {};
  runs.forEach((r) => {
    const key = [r.node, r.payload_code, r.reason, r.station].join('\u0000');
    if (!byKey[key]) {
      byKey[key] = { node: r.node, part: r.payload_code, reason: r.reason, station: r.station, qty: 0, runs: [] };
      lines.push(byKey[key]);
    }
    byKey[key].qty += r.qty || 0;
    byKey[key].runs.push(r);
  });
  const runText = (r) => {
    const mins = Math.round((new Date(r.last) - new Date(r.first)) / 60000);
    return escapeHtml(formatTime(r.first)) + (mins >= 1 ? ' for ' + mins + ' min' : '') + ' — <b>' + num(r.qty) + '</b>';
  };
  const items = lines.map((l) => {
    const shown = l.runs.slice(0, HOLD_DROP_RUNS_SHOWN).map(runText);
    const rest = l.runs.slice(HOLD_DROP_RUNS_SHOWN);
    if (rest.length) {
      const restQty = rest.reduce((n, r) => n + (r.qty || 0), 0);
      shown.push('and ' + rest.length + ' earlier — <b>' + num(restQty) + '</b>');
    }
    return '<li>'
      + escapeHtml(l.node || '(no node)')
      + ' — <b>' + num(l.qty) + '</b> of <span class="text-muted-xs">' + escapeHtml(l.part || '(no part)') + '</span>'
      + ' dropped: ' + escapeHtml(HOLD_DROP_REASON[l.reason] || l.reason || '')
      + '<div class="text-muted-xs">' + l.runs.length + ' run' + (l.runs.length === 1 ? '' : 's') + ': '
      + shown.join(' · ') + '</div>'
      + '</li>';
  }).join('');
  const total = lines.reduce((n, l) => n + l.qty, 0);
  host.innerHTML = '<div class="ledger-routine">'
    + '<div class="ledger-routine__head">' + num(total) + ' dropped in ' + runs.length + ' run' + (runs.length === 1 ? '' : 's')
    + span + '</div>'
    + '<ul class="ledger-routine__list">' + items + '</ul></div>';
}

function renderDeclaredStamps() {
  const host = document.getElementById('rh-declared-stamps');
  if (!host) return;
  const rows = (ledgerExceptions && ledgerExceptions.declared_full_stamps) || [];
  const since = ledgerExceptions && ledgerExceptions.since;
  const span = since ? ' since ' + escapeHtml(formatTime(since)) : '';
  if (!rows.length) {
    host.innerHTML = '<div class="ledger-routine text-muted-sm">No bins declared full' + (span || ' in the window') + '.</div>';
    return;
  }
  const items = rows.map((st) => '<li>'
    + escapeHtml(st.node || '(no node)') + ' · ' + escapeHtml(formatTime(st.applied_at))
    + ' — <code>' + escapeHtml(st.bin_label || ('bin ' + st.bin_id)) + '</code>'
    + ' <span class="text-muted-xs">' + escapeHtml(st.payload_code || '') + '</span>'
    + (st.reason === 'no_pack'
      ? ' counted <b>' + num(st.counted) + '</b>, kept: no pack for this part'
      : ' counted <b>' + num(st.counted) + '</b> → its pack <b>' + num(st.declared) + '</b>')
    + (st.actor ? ' by ' + escapeHtml(st.actor) : '')
    + '</li>').join('');
  host.innerHTML = '<div class="ledger-routine">'
    + '<div class="ledger-routine__head">' + rows.length + ' bin' + (rows.length === 1 ? '' : 's')
    + ' declared full' + span + '</div>'
    + '<ul class="ledger-routine__list">' + items + '</ul></div>';
}

function closeDrill() { document.getElementById('inv-drill').classList.remove('active'); }

// ── flagged-carrier drill ──────────────────────────────────────────────────
// The banner's flagged-carrier count was a dead scroll — it never told the
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
  const narr = document.getElementById('drill-narrative');
  if (!modal || !detail) return;
  if (title) title.textContent = 'Carriers flagged for a recount';
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
      + '<td class="nowrap">' + timeAgoHTML(b.anomaly_at) + '</td>'
      + '<td class="nowrap">' + (b.last_reject_at ? timeAgoHTML(b.last_reject_at) : '—') + '</td></tr>').join('')
    + '</tbody></table></div>';
}


// ── formatting ─────────────────────────────────────────────────────────────
function num(n) { return (n == null || isNaN(n)) ? '—' : Number(n).toLocaleString(); }

function refresh() { loadAll(); }

// ── init + live updates ────────────────────────────────────────────────────
delegateActions(document.body, {
  refresh, showRejectedDeltas,
  'close-modal': closeDrill,
}, { events: ['click'] });

document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeDrill(); });

// Live: refresh on order events, a burst being one reload after it settles; a
// hidden tab reloads nothing and catches up once when shown; a light interval
// keeps the "as of" stamp honest without live traffic.
const FALLBACK_MS = 20000;
const live = document.getElementById('ch-live');
function markLive() { if (live) { live.textContent = 'live'; live.classList.add('is-live'); } }
const reload = createLiveSchedule(() => loadAll(true), { debounceMs: 1500 });
onSSE('order-update', () => { markLive(); reload.kick(); });
let connectedOnce = false;
onSSE('connected', () => {
  markLive();
  if (connectedOnce) reload.kick();
  connectedOnce = true;
});
setInterval(() => {
  if (reload.hidden()) { reload.markStale(); return; }
  if (Date.now() - lastLoadAt >= FALLBACK_MS - 1000) loadAll(true);
}, FALLBACK_MS);

loadAll();
