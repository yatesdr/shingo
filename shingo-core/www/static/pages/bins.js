import { debounce, delegateActions, el, escapeHtml, h, switchTab } from '/static/shared/dom.js';
import { confirm as uiConfirm, hideModal, prompt as uiPrompt, showModal } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';
import { abovePackNote } from '/static/shared/count-words.js';
import { linePieces, uopRatio } from '/static/shared/count.js';
import { apiGet, apiPost, openFromQuery, timeAgoHTML } from '/static/app.js';
import { onSSE } from '/static/shared/bus.js';
import { nodePicker, nodeSource, nodeWhy } from '/static/shared/node-picker.js';
import { partPicker, partSource } from '/static/shared/part-picker.js';
import { mountKeyStrip } from '/static/pages/bins-keystrip.js';
import { binRowView, uopText } from '/static/pages/bins-row.js';
import { echoesFor, newEchoLedger } from '/static/pages/bins-echo.js';
import { newRowsFeed } from '/static/pages/bins-live.js';

// ===== STATE =====
var currentBinId = null;
var currentBinData = null;
// journalFresh: the Journal tab shows the open bin's history as of its last
// draw. The pop-up reads the bin without history (LC12); the history is read
// when the Journal is opened, and again on an update while it is the open tab.
var journalFresh = false;
// ===== READS =====
// readBin is the one way this page reads a bin. withHistory=false is the
// history-free read (?history=0): what the pop-up, the row and the cycle count
// draw. withHistory=true is the default answer, the same plus the whole audit,
// read only for the Journal.
function readBin(id, withHistory) {
  return apiGet('/api/bins/detail?id=' + id + (withHistory ? '' : '&history=0'));
}

// errText is the door's sentence when the hub's api refused (an ApiError,
// app.js), else 'unknown'.
function errText(e) {
  return (e && e.name === 'ApiError' && e.message) || 'unknown';
}

// ===== OWN-ACTION ECHOES =====
// Every action this page posts makes Core broadcast bin-update events for its
// bin (ids only, www/sse.go). The action's answer already carries the bin, so
// those echoes are not a reason to read it again. They are counted, not timed
// (bins-echo.js): each own action expects exactly the number of events its
// verb emits, for 5 s after its answer; every other event is a real update and
// goes to the live feed (see SSE below).
var ECHO_EXPIRY_MS = 5000;
var echoes = newEchoLedger(ECHO_EXPIRY_MS);

// postBin posts an action for the bins in `ids` (verb `action`) and settles
// each bin's expected echoes against the answer: a bin the server refused (or
// a post that failed) emitted nothing, so any event already taken for it was a
// real update, and the open bin is read.
function postBin(url, body, ids, action) {
  var expected = {};
  ids.forEach(function(id) { expected[id] = echoes.expect(id, echoesFor(action)); });
  function settle(okFor) {
    var now = Date.now();
    var reread = false;
    ids.forEach(function(id) {
      if (echoes.settle(expected[id], okFor(id), now) > 0 && id === currentBinId) reread = true;
    });
    if (reread) refreshOpenBin();
  }
  return apiPost(url, body).then(
    function(resp) {
      if (resp && Array.isArray(resp.results)) {
        var ok = {};
        resp.results.forEach(function(r) { ok[r.id] = !!r.ok; });
        settle(function(id) { return !!ok[id]; });
      } else {
        settle(function() { return true; });
      }
      return resp;
    },
    function(e) { settle(function() { return false; }); throw e; });
}

// postBinAction posts one verb for one bin; the answer is the bin's
// history-free detail plus "status":"ok".
function postBinAction(id, action, params) {
  return postBin('/api/bins/action', { id: id, action: action, params: params || {} }, [id], action);
}

// ===== ALERT ESCALATION (P20) =====
// A card/row is as loud as the loudest status it contains (extends the Signal
// rule "only alerts are loud" up a level). These are the only "alert" statuses;
// a card whose bin status OR current-order status is one gets an alert
// affordance keyed to the loudest. Edge colors reuse existing semantic tokens
// (faulted→amber, failed/blocked→red) — no invented colors. Per-field pills are
// never overwritten; only card/row chrome is added.
var ALERT_STATUSES = ['faulted', 'failed', 'blocked'];
var ALERT_EDGE = { faulted: 'var(--warning)', failed: 'var(--danger)', blocked: 'var(--danger)' };
// loudestAlert: most-severe alert present among the given statuses (red beats
// amber — failed/blocked outrank faulted), or null when none are alerts.
function loudestAlert(statuses) {
  var present = statuses.filter(function(s) { return ALERT_STATUSES.indexOf(s) !== -1; });
  if (!present.length) return null;
  if (present.indexOf('failed') !== -1) return 'failed';
  if (present.indexOf('blocked') !== -1) return 'blocked';
  return 'faulted';
}

// ===== FILTERING =====
function filterBins() {
  var q = document.getElementById('bin-search').value.toLowerCase();
  var binType = document.getElementById('bin-type-filter').value;
  var status = document.getElementById('bin-status-filter').value;
  var contents = document.getElementById('bin-contents-filter').value;
  var locked = document.getElementById('bin-locked-filter').value;
  var rows = document.querySelectorAll('#bin-table tbody tr');
  var shown = 0;
  rows.forEach(function(row) {
    var d = row.dataset;
    var matchQ = !q || (d.label && d.label.toLowerCase().indexOf(q) >= 0) ||
                 (d.node && d.node.toLowerCase().indexOf(q) >= 0) ||
                 (d.payload && d.payload.toLowerCase().indexOf(q) >= 0);
    var matchType = !binType || d.type === binType;
    var matchStatus = !status || d.status === status;
    var matchContents = !contents || d.contents === contents;
    var matchLocked = !locked || d.locked === locked;
    var vis = matchQ && matchType && matchStatus && matchContents && matchLocked;
    row.style.display = vis ? '' : 'none';
    if (vis) shown++;
  });
  document.getElementById('bin-count').textContent = shown + ' bins';
  // A ticked row a filter hides is out of the selection (getSelectedIds):
  // keep the bar's count to what a bulk action would act on.
  updateBulkBar();
}

// ===== DETAIL MODAL =====

// openBinDetailRow is what a bin row binds to. The delegated dispatcher calls
// handlers as (...verb args, el, evt), so a zero-arg verb hands the row element
// to openBinDetail's FIRST parameter — which is a bin id. This used to be
// absorbed by a typeof check inside openBinDetail; reading the id out here keeps
// the element on the element side of the seam and leaves the id function taking
// an id, which is what every other caller passes it.
function openBinDetailRow(el) {
  openBinDetail(parseInt(el.dataset.binId, 10));
}

// openBinDetail opens the pop-up on a bin: one history-free read, Overview.
function openBinDetail(id) {
  currentBinId = id;
  journalFresh = false;
  readBin(id, false)
    .then(function(resp) {
      if (currentBinId !== id) return;
      document.getElementById('bd-journal').innerHTML = '';
      drawBin(resp, null);
      showBinTab('overview');
      showModal('bin-detail-modal', { onClose: binDetailClosed });
    })
    .catch(function(e) {
      console.error('openBinDetail', id, e);
      toast('Error loading bin: ' + errText(e), 'error');
    });
}

function closeBinDetail() {
  hideModal('bin-detail-modal');
  binDetailClosed();
}

// binDetailClosed is the pop-up's close however it closed (×, Escape, the
// backdrop, Discard): the bin it showed is no longer the open one.
function binDetailClosed() {
  currentBinId = null;
  currentBinData = null;
  journalFresh = false;
}

function binDetailOpen() {
  var m = document.getElementById('bin-detail-modal');
  return !!(currentBinId && m && m.classList.contains('active'));
}

function openTab() {
  var btn = document.querySelector('#bin-detail-modal .tab.active');
  var m = btn && /^switchTab:(\w+)$/.exec(btn.getAttribute('data-action') || '');
  return m ? m[1] : 'overview';
}

// drawBin draws the pop-up from a detail answer. `reset` is null for a fresh
// open; on an update it is the list of field ids to clear (the form an action
// just submitted), and every other field the viewer has typed in keeps its
// value and focus. The open tab is never changed here. The Journal is drawn
// only when the answer carries the history.
var drawGen = 0;
function drawBin(resp, reset) {
  var saved = reset ? captureTyped(reset) : [];
  drawGen++;
  currentBinData = resp;
  document.getElementById('bd-title').textContent = resp.bin.label;
  document.getElementById('bd-subtitle').textContent =
    resp.bin.bin_type_code + (resp.bin.node_name ? ' \u2022 ' + resp.bin.node_name : ' \u2022 unassigned');
  renderOverview(resp);
  renderContents(resp);
  renderActions(resp);
  if ('audit' in resp) {
    renderJournal(resp);
    journalFresh = true;
  } else {
    journalFresh = false;
  }
  restoreTyped(saved);
}

// updateOpenBin redraws the open pop-up after the bin changed. With the
// Journal open the history is read too (one default read in place of the
// history-free one); otherwise the Journal is left to be read when opened.
function updateOpenBin(resp, reset) {
  if (!binDetailOpen() || !resp || !resp.bin || resp.bin.id !== currentBinId) return;
  drawBin(resp, reset || []);
  if (!journalFresh && openTab() === 'journal') loadJournal();
}

// loadJournal reads the open bin with its history and draws the Journal (and,
// from the same answer, the rest), keeping what has been typed.
var journalLoading = null;
function loadJournal() {
  var id = currentBinId;
  if (!id || journalLoading === id) return;
  journalLoading = id;
  var gen = drawGen;
  readBin(id, true)
    .then(function(resp) {
      journalLoading = null;
      if (currentBinId !== id) return;
      // The bin was drawn from a newer answer while this read was out: this
      // one may predate it, so read again rather than draw an older state.
      if (gen !== drawGen) {
        if (!journalFresh && openTab() === 'journal') loadJournal();
        return;
      }
      drawBin(resp, []);
    })
    .catch(function(e) {
      journalLoading = null;
      console.error('loadJournal', id, e);
      toast('Error loading history: ' + errText(e), 'error');
    });
}

// captureTyped records every field in the pop-up whose value differs from
// what was drawn (a typed value, a changed choice), and which one has focus,
// except the ids in `reset`.
function captureTyped(reset) {
  var saved = [];
  var active = document.activeElement;
  Object.keys(BD_PICKERS).forEach(function(id) {
    var v = BD_PICKERS[id].value();
    if (reset.indexOf(id) === -1 && v) saved.push({ id: id, picker: true, dirty: true, value: v });
  });
  document.querySelectorAll('#bin-detail-modal input[id], #bin-detail-modal select[id], #bin-detail-modal textarea[id]')
    .forEach(function(f) {
      if (reset.indexOf(f.id) !== -1) return;
      var dirty;
      if (f.tagName === 'SELECT') {
        dirty = f.selectedIndex !== drawnIndex(f);
      } else {
        dirty = f.value !== f.defaultValue;
      }
      var focused = f === active;
      if (!dirty && !focused) return;
      var rec = { id: f.id, tag: f.tagName, dirty: dirty, value: f.value, focused: focused, start: null, end: null };
      try { rec.start = f.selectionStart; rec.end = f.selectionEnd; } catch (e) { /* not a text field */ }
      saved.push(rec);
    });
  return saved;
}

// drawnIndex is the option a select was drawn with: the one marked selected,
// else the first.
function drawnIndex(f) {
  for (var i = 0; i < f.options.length; i++) {
    if (f.options[i].defaultSelected) return i;
  }
  return f.options.length ? 0 : -1;
}

// clearFields puts the named fields back to what was drawn, so a submitted
// form is not carried over as typing by a later redraw.
function clearFields(ids) {
  ids.forEach(function(id) {
    if (BD_PICKERS[id]) { BD_PICKERS[id].set(''); return; }
    var f = document.getElementById(id);
    if (!f) return;
    if (f.tagName === 'SELECT') {
      f.selectedIndex = drawnIndex(f);
    } else {
      f.value = f.defaultValue;
    }
  });
}

function restoreTyped(saved) {
  saved.forEach(function(rec) {
    if (rec.picker) { if (BD_PICKERS[rec.id]) BD_PICKERS[rec.id].set(rec.value); return; }
    var f = document.getElementById(rec.id);
    if (!f || f.tagName !== rec.tag) return;
    if (rec.dirty) {
      if (f.tagName === 'SELECT') {
        if (Array.prototype.some.call(f.options, function(o) { return o.value === rec.value; })) f.value = rec.value;
      } else {
        f.value = rec.value;
      }
    }
    if (rec.focused && f.offsetParent !== null) {
      f.focus({ preventScroll: true });
      if (rec.start !== null) {
        try { f.setSelectionRange(rec.start, rec.end); } catch (e) { /* not a text field */ }
      }
    }
  });
}

// showBinTab is the bin pop-up's own tab handler, bound to its
// data-action="switchTab:<name>" tabs. Not dom.js's switchTab (§22): these
// panels are #bd-<name>, not .tab-panel siblings of the bar, and the page
// calls it with no tab element when it opens the pop-up on Overview.
function showBinTab(name) {
  if (name === 'journal' && !journalFresh) loadJournal();
  var tabs = ['overview', 'contents', 'actions', 'journal'];
  tabs.forEach(function(t) {
    var panel = document.getElementById('bd-' + t);
    var btn = document.querySelector('#bin-detail-modal .tab[data-action="switchTab:' + t + '"]');
    // A .tab-panel shows only with .active (shared/components.css, §22).
    if (panel) panel.classList.toggle('active', t === name);
    if (btn) btn.classList.toggle('active', t === name);
  });
}

function renderOverview(data) {
  var b = data.bin;
  // In transit, the bin sits at the synthetic _TRANSIT node and its own
  // payload can read blank — show the carrying order's cargo + route instead
  // so the modal reads like a tracking line, not a blank carrier.
  var inTransit = b.node_name === '_TRANSIT';
  var ord = data.current_order;
  var html = '<div class="bd-fields">';
  if (inTransit && ord && ord.source_node) {
    html += bdField('Location', escapeHtml(ord.source_node) + ' → ' + escapeHtml(ord.delivery_node) +
      ' <span class="badge badge-in_transit">in transit</span>'); // P21: in_transit is a Signal status — show its cyan pill, not muted text
  } else {
    html += bdField('Location', b.node_name ? escapeHtml(b.node_name) : '<span class="text-muted">unassigned</span>');
  }
  html += bdField('Status', '<span class="badge badge-' + escapeHtml(b.status) + '">' + escapeHtml(b.status) + '</span>');
  var payloadDisplay = b.payload_code
    ? '<code>' + escapeHtml(b.payload_code) + '</code>'
    : (inTransit && ord && ord.payload_code
        ? '<code>' + escapeHtml(ord.payload_code) + '</code> <span class="text-muted" style="font-size:0.85em">(in transit)</span>'
        : '<span class="text-muted">empty</span>');
  html += bdField('Payload', payloadDisplay);
  html += bdField('UoP Remaining', uopText(b, uopRatio(data.template_manifest)) + (b.payload_code ? uopBar(b.uop_remaining, data.template) : ''));
  html += bdField('Manifest', b.manifest_confirmed ? '<span style="color:var(--success)">Confirmed</span>' :
    (b.payload_code ? '<span style="color:var(--warning)">Unconfirmed</span>' : '<span class="text-muted">-</span>'));
  html += bdField('Locked', b.locked ? '<span style="color:var(--danger)">' + escapeHtml(b.locked_by) + '</span>' : 'No');
  html += bdField('Description', b.description ? escapeHtml(b.description) : '<span class="text-muted">-</span>');
  html += bdField('Bin Type', escapeHtml(b.bin_type_code));
  if (b.claimed_by) {
    html += bdField('Claimed By', 'Order #' + b.claimed_by);
  }
  if (b.last_counted_at) {
    html += bdField('Last Counted', timeAgoHTML(b.last_counted_at) + ' by ' + escapeHtml(b.last_counted_by));
  }
  html += bdField('Created', timeAgoHTML(b.created_at));
  html += bdField('Updated', timeAgoHTML(b.updated_at));
  html += '</div>';

  if (data.current_order) {
    var o = data.current_order;
    html += '<div class="action-group"><h4>Current Order</h4>';
    html += '<p>Order #' + o.id + ' &mdash; <span class="badge badge-' + escapeHtml(o.status) + '">' + escapeHtml(o.status) + '</span></p>';
    html += '</div>';
  }

  // P20 — card-level escalation. The two per-field pills above (bin Status,
  // Current Order status) stay exactly as built; here we only add chrome so the
  // card is as loud as its loudest status. When the bin or its current order is
  // an alert, prepend a banner (warning glyph + the loudest alert echoed as its
  // Signal badge) and key the card's left-edge to that alert. No alert → no
  // banner, edge cleared (looks exactly as before).
  var alert = loudestAlert([b.status, ord && ord.status]);
  if (alert) {
    html = '<div class="bd-alert" style="display:flex;align-items:center;gap:0.5rem;margin-bottom:0.75rem">' +
      '<span aria-hidden="true" style="color:' + ALERT_EDGE[alert] + ';font-size:1.15em">&#9888;</span>' +
      '<span class="badge badge-' + alert + '">' + alert + '</span>' +
      '<span class="text-muted">needs attention</span></div>' + html;
  }
  var ov = document.getElementById('bd-overview');
  ov.innerHTML = html;
  ov.style.borderLeft = alert ? ('4px solid ' + ALERT_EDGE[alert]) : '';
  ov.style.paddingLeft = alert ? '0.85rem' : '';
}

function renderContents(data) {
  var b = data.bin;
  var html = '';

  if (data.manifest && data.manifest.items && data.manifest.items.length > 0) {
    // The count is derived, not stored, from uop_remaining and the template
    // line, by the count rule (count.js linePieces: a template of one line
    // shows the bin's count as it stands). A part the template no longer
    // lists shows "—" rather than 0, because "we cannot count this" and "there
    // are none" are different things to tell someone standing at the bin.
    var template = data.template_manifest || [];
    var lineOf = {};
    template.forEach(function(t, i) { lineOf[t.part_number] = i; });
    var uop = (b && b.uop_remaining) || 0;
    // Either key: lines are written as part_number, and bins loaded before that
    // change carry catid. Same fallback the Go reader and the inventory query
    // make — a bin does not get its jsonb rewritten under a running plant.
    html += h`<table class="table-compact"><thead><tr><th>Part number</th><th>Qty</th><th>Notes</th></tr></thead><tbody>${
      data.manifest.items.map(function(item) {
        var part = item.part_number || item.catid;
        var line = lineOf[part];
        var qty = line == null ? '—' : String(linePieces(template, line, uop));
        return h`<tr><td><code>${part}</code></td><td>${qty}</td><td>${item.notes || ''}</td></tr>`;
      })
    }</tbody></table>`;
  } else {
    html += '<p class="text-muted mb-1">No manifest items.</p>';
  }

  if (PAGE_AUTH) {
    html += '<hr style="margin:1rem 0;border:none;border-top:1px solid var(--border)">';

    // Load Payload — filter to payloads whose bin-type allow-list is empty
    // (unrestricted) or includes this bin's type. Mirrors backend advisory
    // enforcement in BinService.LoadPayload.
    //
    // Withheld while an order holds the bin before delivery: Core would refuse
    // the load (refuseHeldCarrierTx), and data.load_refusal is its sentence,
    // naming the order. Absent from delivered on, when the load lands.
    var held = data.load_refusal || '';
    var off = held ? ' disabled' : '';
    html += '<div class="action-group"><h4>Load Payload</h4>';
    if (held) html += '<p class="text-muted mb-1" id="bd-load-held">' + escapeHtml(held) + '</p>';
    html += '<div class="inline-form">';
    html += '<div class="form-group"><label>Payload</label><div id="bd-load-payload" class="bd-picker"></div></div>';
    html += '<div class="form-group"><label>Count (blank = full)</label><input type="number" id="bd-load-uop" min="0" style="width:110px" placeholder="full pack"' + off + '></div>';
    html += held ? '<button class="btn btn-primary btn-sm" disabled>Load</button>'
      : '<button class="btn btn-primary btn-sm" data-action="loadPayload">Load</button>';
    html += '</div></div>';

    // Clear / Confirm
    html += '<div style="display:flex;gap:0.5rem">';
    if (b.payload_code) {
      html += '<button class="btn btn-sm btn-danger" data-action="clearBin">Clear Bin</button>';
      if (b.manifest_confirmed) {
        html += '<button class="btn btn-sm" data-action="toggleManifest">Unconfirm</button>';
      } else {
        html += '<button class="btn btn-sm" data-action="toggleManifest">Confirm Manifest</button>';
      }
    }
    html += '</div>';
  }

  document.getElementById('bd-contents').innerHTML = html;
  if (PAGE_AUTH) mountLoadPicker(b, data.load_refusal || '');
}

// THE PAGE'S ONE NODE LIST AND ONE PART LIST (shared/node-picker.js,
// part-picker.js), as handleBins rendered them. Each field says what it may
// not take, and the picker shows it greyed with the reason; _TRANSIT is never
// offered.
var binNodes = nodeSource(function() { return PAGE_NODES || []; }, PAGE_NODES || []);
var binParts = partSource(function() { return PAGE_PAYLOADS || []; }, PAGE_PAYLOADS || []);
// The pop-up's pickers, by their host's id: drawn afresh with the pop-up, and
// kept across a redraw as a typed field is (captureTyped).
var BD_PICKERS = {};

// mountLoadPicker is Load Payload's part: a part whose bin-type allow-list is
// set and leaves this bin's type out is shown greyed (BinService.LoadPayload
// refuses it too); a load the server would refuse while an order holds the
// bin greys every part with its sentence.
function mountLoadPicker(b, held) {
  var host = document.getElementById('bd-load-payload');
  BD_PICKERS['bd-load-payload'] = partPicker(host, binParts, {
    rule: function(p) {
      if (held) return held;
      var allowed = (PAGE_PAYLOAD_BIN_TYPES || {})[p.code];
      return allowed && allowed.length > 0 && allowed.indexOf(b.bin_type_id) === -1 ? 'not on a ' + b.bin_type_code : '';
    },
  }, { placeholder: 'Pick a payload' });
}

// mountMovePicker is Move's destination: any node but the one the bin is at,
// and a robot deck (BinService.MoveByHand refuses one; a bin reaches it only
// by a robot lifting it).
function mountMovePicker(b) {
  var host = document.getElementById('bd-move-node');
  BD_PICKERS['bd-move-node'] = nodePicker(host, binNodes, {
    value: 'id',
    rule: nodeWhy.any(nodeWhy.robot, function(n) { return b.node_id && n.id === b.node_id ? 'the bin is here' : ''; }),
  }, { placeholder: 'Pick a node' });
}

// mountCreateNodePicker is Create bin's Node: optional, any node but a robot
// deck and a disabled one; the form posts it as node_id.
var createNodePicker = null;
function mountCreateNodePicker() {
  var host = document.getElementById('bin-create-node');
  if (!host) return;
  createNodePicker = nodePicker(host, binNodes, { value: 'id', rule: nodeWhy.any(nodeWhy.robot, nodeWhy.disabled) },
    { name: 'node_id', placeholder: 'No node' });
}
mountCreateNodePicker();

async function renderActions(data) {
  var b = data.bin;
  if (!PAGE_AUTH) {
    document.getElementById('bd-actions').innerHTML = '<p class="text-muted">Login required for actions.</p>';
    return;
  }
  var html = '';

  // Status transitions
  html += '<div class="action-group"><h4>Status</h4>';
  if (b.status !== 'available') html += '<button class="btn btn-sm" data-action="doBinAction:activate" >Activate</button> ';
  if (b.status !== 'flagged') html += '<button class="btn btn-sm" data-action="doBinAction:flag" >Flag</button> ';
  if (b.status !== 'maintenance') html += '<button class="btn btn-sm" data-action="doBinAction:maintenance" >Maintenance</button> ';
  // Staged toggle: blue when active, default otherwise. Available ↔ staged only.
  if (b.status === 'available' || b.status === 'staged') {
    var stagedActive = (b.status === 'staged');
    html += '<button class="btn btn-sm' + (stagedActive ? ' btn-primary' : '') +
      '" data-action="doBinAction:' + (stagedActive ? 'release' : 'stage') + '">Staged</button> ';
  }
  if (b.status !== 'retired') html += '<button class="btn btn-sm btn-danger" data-action="doBinAction:retire" >Retire</button> ';
  html += '</div>';

  // Lock/Unlock
  html += '<div class="action-group"><h4>Lock</h4>';
  if (b.locked) {
    html += '<p class="text-muted mb-1">Locked by <strong>' + escapeHtml(b.locked_by) + '</strong></p>';
    html += '<button class="btn btn-sm" data-action="doBinAction:unlock" >Unlock</button>';
  } else {
    html += '<div class="inline-form">';
    html += '<div class="form-group"><label>Locked By</label><input type="text" id="bd-lock-actor" placeholder="Name" style="width:150px"></div>';
    html += '<button class="btn btn-sm" data-action="lockBin">Lock</button>';
    html += '</div>';
  }
  html += '</div>';

  // Move
  html += '<div class="action-group"><h4>Move</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Destination</label><div id="bd-move-node" class="bd-picker"></div></div>';
  html += '<button class="btn btn-sm" data-action="moveBin">Move</button>';
  html += '<button class="btn btn-sm" data-action="requestMove">Request Move</button>';
  html += '</div></div>';

  // Record Count
  html += '<div class="action-group"><h4>Record Count</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Actual UoP</label><input type="number" id="bd-count-uop" min="0" style="width:80px" value="' + b.uop_remaining + '"' +
    ' data-pack="' + (b.uop_capacity || 0) + '" data-action-input="showCountNote">' +
    '<small id="bd-count-note" class="text-muted" style="display:block" hidden></small></div>';
  html += '<div class="form-group"><label>Counter</label><input type="text" id="bd-count-actor" placeholder="Name" style="width:150px"></div>';
  html += '<button class="btn btn-sm" data-action="recordCount">Record</button>';
  html += '</div></div>';

  // Edit Properties
  html += '<div class="action-group"><h4>Properties</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Label</label><input type="text" id="bd-edit-label" value="' + escapeHtml(b.label) + '" style="width:150px"></div>';
  html += '<div class="form-group"><label>Description</label><input type="text" id="bd-edit-desc" value="' + escapeHtml(b.description || '') + '" style="width:200px"></div>';
  html += '<div class="form-group"><label>Bin Type</label><select id="bd-edit-bin-type" style="width:160px">';
  (PAGE_BIN_TYPES || []).forEach(function(bt) {
    var sel = (bt.id === b.bin_type_id) ? ' selected' : '';
    html += '<option value="' + bt.id + '"' + sel + '>' + escapeHtml(bt.code) + '</option>';
  });
  html += '</select></div>';
  // The count of totes standing on the cart, filled or empty: blank is
  // unknown, 0 is tops & bottoms. A change of bin type blanks it on the server.
  var totes = (b.totes_on_cart === undefined || b.totes_on_cart === null) ? '' : String(b.totes_on_cart);
  html += '<div class="form-group"><label title="Totes standing on the cart, filled or empty. Blank is unknown; 0 is tops & bottoms (no totes on the cart). Changing the bin type blanks it.">Totes on cart</label>' +
    '<input type="number" id="bd-edit-totes" min="0" step="1" placeholder="unknown" value="' + escapeHtml(totes) + '" style="width:90px"></div>';
  html += '<button class="btn btn-sm" data-action="updateBinProps">Save</button>';
  html += '</div></div>';

  // Retire
  html += '<div class="action-group">';
  var confirmMsg = retireConfirmMsg(b);
  html += '<form method="POST" action="/bins/retire" data-action-submit="confirmDeleteForm" data-confirm-msg="' + escapeHtml(confirmMsg) + '">';
  html += '<input type="hidden" name="id" value="' + b.id + '">';
  html += '<button type="submit" class="btn btn-sm btn-danger">Retire Bin</button>';
  html += '</form></div>';

  document.getElementById('bd-actions').innerHTML = html;
  mountMovePicker(b);
}

function renderJournal(data) {
  var html = '';

  // Add Note form
  if (PAGE_AUTH) {
    html += '<div class="action-group"><h4>Add Note</h4>';
    html += '<div class="inline-form mb-1">';
    html += '<div class="form-group"><label>Type</label><select id="bd-note-type" style="width:120px">';
    html += '<option value="general">General</option><option value="issue">Issue</option>';
    html += '<option value="quality">Quality</option><option value="resolution">Resolution</option>';
    html += '</select></div>';
    html += '<div class="form-group"><label>Actor</label><input type="text" id="bd-note-actor" placeholder="Name" style="width:120px"></div>';
    html += '</div>';
    html += '<div class="form-group"><textarea id="bd-note-msg" placeholder="Note text..." rows="2" style="font-size:0.85rem"></textarea></div>';
    html += '<button class="btn btn-sm btn-primary" data-action="addNote">Add Note</button>';
    html += '</div>';
    html += '<hr style="margin:1rem 0;border:none;border-top:1px solid var(--border)">';
  }

  // Audit entries
  if (data.audit && data.audit.length > 0) {
    html += h`<div class="timeline">${
      data.audit.map(function(e) {
        return h`<div class="timeline-item">
          <div class="time">${{__html:true, value: timeAgoHTML(e.created_at)}} &middot; ${e.actor}</div>
          <div>${e.action}${(e.old_value || e.new_value) ? {__html:true, value: h`: ${e.old_value} &rarr; ${e.new_value}`} : ''}${e.detail ? {__html:true, value: h` &mdash; ${e.detail}`} : ''}</div>
        </div>`;
      })
    }</div>`;
  } else {
    html += '<p class="text-muted">No audit entries.</p>';
  }

  // Recent orders
  if (data.recent_orders && data.recent_orders.length > 0) {
    html += '<h4 style="margin-top:1rem;font-size:0.8rem;text-transform:uppercase;color:var(--text-muted)">Recent Orders</h4>';
    html += h`<table class="table-compact"><thead><tr><th>ID</th><th>Type</th><th>Status</th><th>Created</th></tr></thead><tbody>${
      data.recent_orders.map(function(o) {
        return h`<tr><td>${o.id}</td><td>${o.order_type || ''}</td>
          <td><span class="badge badge-${o.status}">${o.status}</span></td>
          <td>${{__html:true, value: timeAgoHTML(o.created_at)}}</td></tr>`;
      })
    }</tbody></table>`;
  }

  document.getElementById('bd-journal').innerHTML = html;
}

// ===== BIN ACTIONS =====
// doBinAction is the Actions tab's status and unlock buttons
// (data-action="doBinAction:<verb>"). Retire asks first, with the words the
// Retire Bin button below it asks with.
async function doBinAction(action) {
  if (action === 'retire' && currentBinData) {
    if (!await uiConfirm(retireConfirmMsg(currentBinData.bin))) return;
  }
  runBinAction(action);
}

function retireConfirmMsg(b) {
  return 'Mark ' + (b.label || ('bin ' + b.id)) + ' as retired? It will be removed from all production nodes and no longer assigned to orders. Bin history will be preserved.';
}

// runBinAction posts one verb for the open bin. The answer carries the bin
// (LC12), so the pop-up and its table row are drawn from it: one POST, no
// read. `reset` names the fields of the form that was submitted; they are
// drawn fresh, every other typed field is kept, and the open tab stays.
function runBinAction(action, params, reset) {
  var id = currentBinId;
  postBinAction(id, action, params)
    .then(function(resp) {
      // A done action can carry a warning (a move onto a line whose parts
      // take another carrier type): shown, never a refusal.
      if (resp && resp.warning) toast(resp.warning, 'warning');
      // A typed count above the pack is taken as typed, and said (owner,
      // 2026-11-19): the same sentence as every other typed box.
      var over = (action === 'load_payload' || action === 'record_count') && resp && resp.bin
        ? abovePackNote(resp.bin.uop_remaining, resp.bin.uop_capacity) : null;
      if (over) toast(over.text, 'warning');
      if (!resp || !resp.bin) { refreshBinRow(id); return; }
      clearFields(reset || []);
      paintBinRow(resp);
      updateOpenBin(resp, reset);
    })
    .catch(function(e) { toast('Error: ' + (e.error || e), 'error'); });
}

function loadPayload() {
  var code = BD_PICKERS['bd-load-payload'] ? BD_PICKERS['bd-load-payload'].value() : '';
  if (!code) { toast('Select a payload', 'info'); return; }
  // Blank = template capacity (full pack). An explicit value — 0 included —
  // is the operator's declared count and must survive as-is; the old
  // `parseInt(...) || 0` collapsed a typed "0" into "use capacity".
  var raw = document.getElementById('bd-load-uop').value.trim();
  var params = { payload_code: code };
  if (raw !== '') {
    var uop = parseInt(raw, 10);
    if (isNaN(uop) || uop < 0) { toast('Count must be 0 or more', 'info'); return; }
    params.uop_override = uop;
  }
  runBinAction('load_payload', params, ['bd-load-payload', 'bd-load-uop']);
}

async function clearBin() {
  if (!await uiConfirm('Clear this bin\'s payload and manifest?')) return;
  runBinAction('clear');
}

function toggleManifest() {
  var b = currentBinData.bin;
  runBinAction(b.manifest_confirmed ? 'unconfirm_manifest' : 'confirm_manifest');
}

function lockBin() {
  var actor = document.getElementById('bd-lock-actor').value.trim();
  if (!actor) { toast('Enter who is locking this bin', 'info'); return; }
  runBinAction('lock', { actor: actor }, ['bd-lock-actor']);
}

function moveBin() {
  var nodeId = parseInt(BD_PICKERS['bd-move-node'] ? BD_PICKERS['bd-move-node'].value() : '');
  if (!nodeId) { toast('Select a destination', 'info'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    toast('Bin is already at this location', 'info'); return;
  }
  runBinAction('move', { node_id: nodeId }, ['bd-move-node']);
}

// requestMove is "Request Move": a robot carries this bin to the node picked in
// Destination, as a plain move order filed through the manual-move door the
// Orders page uses for a named bin. "Move" beside it records a move made by
// hand and sends no robot. A refusal shows the door's sentence unchanged.
function requestMove() {
  var nodeId = parseInt(BD_PICKERS['bd-move-node'] ? BD_PICKERS['bd-move-node'].value() : '');
  if (!nodeId) { toast('Select a destination', 'info'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    toast('Bin is already at this location', 'info'); return;
  }
  apiPost('/api/bins/request-move', { bin_id: currentBinId, destination_node_id: nodeId })
    .then(function(data) {
      var msg = 'Move #' + data.order_id + ' to ' + (data.to || 'the destination') + ' requested (' + data.status + ')';
      if (data.queue_reason) msg += ': ' + data.queue_reason;
      toast(msg, 'info');
      openBinDetail(currentBinId);
      refreshBinRow(currentBinId);
    })
    .catch(function(e) { toast('Error: ' + (e.error || e), 'error'); });
}

// askRobotToSetDown is the bins page's Return button: the recover_carried_bin
// recovery action, which sends the bin on a robot back to where a claim
// declares it is used from — the same chooser the cancel-return watch uses.
//
// ONE OF THE TWO ACTIONS ON THIS PAGE THAT PUT A ROBOT IN MOTION (Request Move
// is the other), which is why the answer is shown rather than swallowed. On success the detail names the
// destination and which declaration chose it; on refusal the sentence the
// server returns is shown UNCHANGED — "AMR-09 is not dispatchable, the plant
// has taken it out of the pool" is already written for a person, and
// flattening it to "could not recover" would throw away the only useful part.
//
// It POSTs the existing /api/recovery/repair action rather than a new route:
// the diagnostics Recovery tab is the receipt for these, and there is one
// handler behind both.
async function askRobotToSetDown(binId, robot) {
  var id = parseInt(binId, 10) || 0;
  if (!id) return;
  if (!await uiConfirm('Return this bin to where it is used from? ' + (robot && typeof robot === 'string' ? robot : 'The robot') + ' will drive there and unload. If nothing declares a place for it, nothing moves.')) return;
  apiPost('/api/recovery/repair', { action: 'recover_carried_bin', order_id: 0, bin_id: id })
    .then(function(data) { toast(data.detail || 'Recovery order created', 'info'); refreshBinRow(id); })
    .catch(function(e) { toast(e.error || e, 'error'); });
}

// showCountNote is Record Count's live line under the box: the above-pack
// note while the typed count is over the bin's pack.
function showCountNote(input) {
  var note = document.getElementById('bd-count-note');
  if (!note || !input) return;
  var over = abovePackNote(parseInt(input.value, 10), Number(input.dataset.pack) || 0);
  note.textContent = over ? over.text : '';
  note.hidden = !over;
}

function recordCount() {
  // A blank box is not a count: it used to record 0 (parseInt || 0), an
  // empty bin nobody counted. A typed 0 is an empty bin; a negative is
  // refused here and at the door (owner, 2026-11-19).
  var raw = document.getElementById('bd-count-uop').value.trim();
  var uop = parseInt(raw, 10);
  if (raw === '' || isNaN(uop)) { toast('Type the count: 0 for an empty bin', 'info'); return; }
  if (uop < 0) { toast('Count must be 0 or more', 'info'); return; }
  var actor = document.getElementById('bd-count-actor').value.trim();
  runBinAction('record_count', { actual_uop: uop, actor: actor }, ['bd-count-uop', 'bd-count-actor']);
}

function updateBinProps() {
  var label = document.getElementById('bd-edit-label').value.trim();
  var desc = document.getElementById('bd-edit-desc').value.trim();
  var binTypeID = parseInt(document.getElementById('bd-edit-bin-type').value, 10);
  // Sent every time, as the box stands: blank is unknown (null). The box opens
  // on the stored count, so a save of the other fields sends it back as it was
  // and the server writes nothing for it.
  var totesText = document.getElementById('bd-edit-totes').value.trim();
  var params = { label: label, description: desc, totes_on_cart: totesText === '' ? null : parseInt(totesText, 10) };
  if (binTypeID) params.bin_type_id = binTypeID;
  runBinAction('update', params, ['bd-edit-label', 'bd-edit-desc', 'bd-edit-bin-type', 'bd-edit-totes']);
}

function addNote() {
  var noteType = document.getElementById('bd-note-type').value;
  var msg = document.getElementById('bd-note-msg').value.trim();
  var actor = document.getElementById('bd-note-actor').value.trim();
  if (!msg) { toast('Enter a note message', 'info'); return; }
  runBinAction('add_note', { note_type: noteType, message: msg, actor: actor }, ['bd-note-type', 'bd-note-actor', 'bd-note-msg']);
}

// ===== BULK OPERATIONS =====
function toggleAllBins(cb) {
  var boxes = document.querySelectorAll('.bin-cb');
  boxes.forEach(function(box) {
    var row = box.closest('tr');
    if (row && row.style.display !== 'none') {
      box.checked = cb.checked;
    }
  });
  updateBulkBar();
}

function updateBulkBar() {
  var ids = getSelectedIds();
  var bar = document.getElementById('bulk-bar');
  if (!bar) return;
  if (ids.length > 0) {
    bar.style.display = 'flex';
    document.getElementById('bulk-count').textContent = ids.length + ' selected';
  } else {
    bar.style.display = 'none';
  }
}

// getSelectedIds is the ticked rows the viewer can see. A row ticked and then
// hidden by a filter is not acted on: a bulk action does what the table shows.
function getSelectedIds() {
  var ids = [];
  document.querySelectorAll('.bin-cb:checked').forEach(function(cb) {
    var row = cb.closest('tr');
    if (row && row.style.display === 'none') return;
    ids.push(parseInt(cb.value));
  });
  return ids;
}

// BULK_MAX is the server's limit on one bulk action (apiBulkBinAction refuses
// more than 100 ids); the page says so before asking.
var BULK_MAX = 100;

async function bulkAction(action) {
  var ids = getSelectedIds();
  if (ids.length === 0) return;
  if (ids.length > BULK_MAX) {
    toast(ids.length + ' bins selected: a bulk action takes at most ' + BULK_MAX + '. Narrow the filter or untick some.', 'error');
    return;
  }
  var params = {};
  if (action === 'lock') {
    var actor = await uiPrompt('Lock by (name):');
    if (!actor) return;
    params = { actor: actor };
  }
  if (!await uiConfirm(action + ' ' + ids.length + ' bin(s)?')) return;
  postBin('/api/bins/bulk-action', { ids: ids, action: action, params: params }, ids, action)
    .then(function(data) {
      var results = data.results || [];
      var failed = results.filter(function(r) { return !r.ok; });
      if (failed.length > 0) {
        toast(failed.length + ' failed: ' + failed.map(function(f) { return '#' + f.id + ': ' + f.error; }).join(', '), 'error');
      }
      // Each result carries its bin (LC12): repaint from the answer.
      results.forEach(function(r) {
        if (!r.bin) return;
        paintBinRow(r);
        updateOpenBin(r);
      });
      clearSelection();
    })
    .catch(function(e) { toast('Error: ' + (e.error || e), 'error'); });
}

// refreshBinRow asks the live feed for one bin's row. Only the callers whose
// answer does not carry the bin use it (Return, Request Move, an action whose
// re-read failed); an action's answer is painted directly.
function refreshBinRow(id) {
  rowsFeed.bin(id);
}

// paintBinRow draws one table row from a detail answer as bins.html draws it
// (bins-row.js binRowView): Location's display name, the Return button, and
// the sort and search values with the cells. False when the table has no row
// for the bin.
function paintBinRow(detail) {
  var b = detail && detail.bin;
  if (!b) return false;
  var row = document.querySelector('#bin-table tbody tr[data-id="' + b.id + '"]');
  if (!row) return false;
  var v = binRowView(detail, PAGE_AUTH, typeof PAGE_UOP_RATIOS === 'object' ? PAGE_UOP_RATIOS : null);
  row.dataset.label = v.data.label;
  row.dataset.type = v.data.type;
  row.dataset.status = v.data.status;
  row.dataset.node = v.data.node;
  row.dataset.payload = v.data.payload;
  row.dataset.uop = v.data.uop;
  row.dataset.uopCapacity = v.data.uopCapacity;
  row.dataset.locked = v.data.locked;
  row.dataset.claimed = v.data.claimed;
  row.dataset.confirmed = v.data.confirmed;
  row.dataset.contents = v.data.contents;

  var tds = row.querySelectorAll('td');
  var off = row.querySelector('.bin-cb') ? 1 : 0;
  function cell(i, html, sort) {
    var td = tds[off + i];
    if (!td) return;
    td.innerHTML = html;
    if (sort !== undefined) td.setAttribute('data-sort-value', sort);
  }
  // Label, Type, Location, Process, Payload, UoP, Status, Flags.
  cell(0, v.label.html, v.label.sort);
  if (tds[off + 1]) tds[off + 1].textContent = v.type.text;
  cell(2, v.location.html, v.location.sort);
  if (tds[off + 3]) tds[off + 3].textContent = v.process.text;
  cell(4, v.payload.html, v.payload.sort);
  cell(5, v.uop.html, v.uop.sort);
  cell(6, v.status.html, v.status.sort);
  cell(7, v.flags.html + v.flags.returnHTML);
  return true;
}

// insertBinRow adds a row for a bin the table does not have (made since the
// page was drawn) at the top, where the server's newest-first order puts it,
// built as bins.html builds one and painted by paintBinRow.
function insertBinRow(tbody, detail) {
  var id = detail.bin.id;
  var tr = document.createElement('tr');
  tr.dataset.id = id;
  tr.dataset.action = 'openBinDetailRow';
  tr.dataset.binId = id;
  tr.dataset.skipOnCheckbox = '1';
  tr.innerHTML = (PAGE_AUTH ? '<td data-action="stopPropagation"><input type="checkbox" class="bin-cb" value="' + id +
    '" data-action-change="updateBulkBar"></td>' : '') +
    '<td></td><td></td><td></td><td></td><td></td><td></td><td></td><td class="bin-flags"></td>';
  tbody.insertBefore(tr, tbody.firstChild);
  paintBinRow(detail);
}

// applyRows paints a GET /api/bins/rows answer: each row repainted where the
// table has it and added where it does not, each gone bin's row dropped, and,
// for an answer of every row, any row it does not hold dropped too. Then the
// filters run again, so a row that now matches the search shows and one that
// no longer does hides. A repainted row keeps its place: sorting again orders
// by the new values. The open pop-up is read again when its bin was in the
// answer.
//
// A page drawn with no bins has no table to add to; a reload draws one.
function applyRows(answer, every) {
  var tbody = document.querySelector('#bin-table tbody');
  if (!tbody || !answer) return;
  var held = {};
  (answer.rows || []).forEach(function(detail) {
    held[detail.bin.id] = true;
    if (!paintBinRow(detail)) insertBinRow(tbody, detail);
  });
  var drop = (answer.gone || []).slice();
  if (every) {
    tbody.querySelectorAll('tr[data-id]').forEach(function(tr) {
      if (!held[tr.dataset.id]) drop.push(tr.dataset.id);
    });
  }
  drop.forEach(function(id) {
    var tr = tbody.querySelector('tr[data-id="' + id + '"]');
    if (tr) tr.remove();
  });
  filterBins();
  if (currentBinId && held[currentBinId]) refreshOpenBin();
}

function clearSelection() {
  document.querySelectorAll('.bin-cb').forEach(function(cb) { cb.checked = false; });
  var hdr = document.querySelector('#bin-table thead input[type=checkbox]');
  if (hdr) hdr.checked = false;
  updateBulkBar();
}

// ===== SSE =====
// Subscribed on the shared onSSE bus (shared/bus.js); the handler receives
// the parsed payload. Replaces the retired app.js IIFE window.onBinUpdate
// dispatch (Q-002).
//
// The events carry ids only. Each names a bin, or an order whose carts' rows
// changed, to the live feed (bins-live.js), which reads those rows in one
// request and repaints them (applyRows); the open pop-up is read again when
// its bin was among them: history-free, or with history when the Journal is
// the open tab. The redraw keeps the open tab and anything typed.
//
// The counted echoes of this page's own actions are dropped: the action's
// answer already drew the bin. A bin-row that follows an own action is not
// counted, so it costs one row read: the price of never missing a count tick.
var refreshOpenBin = debounce(function() {
  if (!binDetailOpen()) return;
  var id = currentBinId;
  readBin(id, openTab() === 'journal')
    .then(function(resp) { updateOpenBin(resp); })
    .catch(function(e) { console.error('bin-update refresh', id, e); });
}, 500);

var rowsFeed = newRowsFeed({
  read: function(query) { return apiGet('/api/bins/rows' + (query ? '?' + query : '')); },
  apply: applyRows,
  hidden: function() { return document.hidden; },
  later: function(fn, ms) { return setTimeout(fn, ms); },
  cancel: function(handle) { clearTimeout(handle); },
  quietMs: 400,
  maxWaitMs: 2000
});
document.addEventListener('visibilitychange', function() {
  if (!document.hidden) rowsFeed.shown();
});

onSSE('bin-update', function(data) {
  if (!data) return;
  // A bin made elsewhere names only its node, so every row is read.
  if (!data.bin_id) { rowsFeed.every(); return; }
  // Taken for every bin, open or not, so a counted echo is consumed where it
  // lands and cannot be mistaken for a later real update.
  if (echoes.take(data.bin_id, Date.now())) return;
  rowsFeed.bin(data.bin_id);
});

// A count tick, or a station door's write: no bin-update says so.
onSSE('bin-row', function(data) {
  if (data) rowsFeed.bin(data.bin_id);
});

// An order's dispatch claims its carts and its end releases them, which
// changes their rows (the order chip, the route) with no bin event.
var CLAIM_MOVES = { dispatched: true, completed: true, failed: true, cancelled: true, skipped: true };
onSSE('order-update', function(data) {
  if (data && CLAIM_MOVES[data.type]) rowsFeed.order(data.order_id);
});

// RECONNECT ONLY, never on the first connect: the first connect of a fresh
// page has missed nothing, the server just drew it (sourcing.js has the same
// rule, and why). After a drop, events were missed and nothing says which.
var everConnected = false;
var droppedSinceConnect = false;
onSSE('disconnected', function() {
  if (everConnected) droppedSinceConnect = true;
});
onSSE('connected', function() {
  if (everConnected && droppedSinceConnect) {
    droppedSinceConnect = false;
    rowsFeed.every();
  }
  everConnected = true;
});

// ===== HELPERS =====

function bdField(label, value) {
  return '<div class="bd-field"><label>' + label + '</label><span>' + value + '</span></div>';
}

function uopBar(remaining, template) {
  var capacity = (template && template.uop_capacity) ? template.uop_capacity : remaining;
  if (capacity <= 0) return '';
  var pct = Math.min(100, Math.round(remaining / capacity * 100));
  var cls = pct > 25 ? 'uop-ok' : (pct > 5 ? 'uop-low' : 'uop-empty');
  return ' <span class="uop-bar"><span class="uop-bar-fill ' + cls + '" style="width:' + pct + '%"></span></span>';
}

// ===== BIN TYPE MODALS =====
// "+ Bin Type" opens with the open family tab's family filled in.
function openCreateBTModal() {
  var family = document.getElementById('bt-create-family');
  if (family) family.value = openFamilyCode();
  renderRobotGroupChoices(document.getElementById('bt-create-robot-group'), '');
  showModal('bt-create-modal');
}

// renderRobotGroupChoices draws the empty-carrier robot picker: one radio per
// fleet robot group plus "Any robot", every option visible with its state on
// it, the same shape as the node Allowed Bin Types list. ONE choice, not
// checkboxes, because a fleet order carries exactly one group
// (fleet.CreateOrderRequest.RobotGroup) and "every group" is already "Any robot".
//
// The saved value is drawn FIRST and stays drawn whatever the fetch does. A
// carrier restriction must survive the fleet manager being down - it is the
// reason the restriction exists - so on an RDS outage the list still submits
// what is saved instead of silently falling back to "Any robot". A saved group
// the fleet no longer reports is kept and marked, never dropped.
function renderRobotGroupChoices(box, current) {
  if (!box) return;
  current = current || '';
  var name = 'required_robot_group';
  var drawn = {};
  box.innerHTML = '';
  function addChoice(value, text, note) {
    if (drawn[value]) return;
    drawn[value] = true;
    var row = document.createElement('label');
    row.className = 'tag-check';
    var radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = name;
    radio.value = value;
    radio.checked = value === current;
    row.appendChild(radio);
    row.appendChild(document.createTextNode(' ' + text));
    if (note) {
      var n = document.createElement('span');
      n.className = 'text-muted';
      n.textContent = ' ' + note;
      row.appendChild(n);
    }
    box.appendChild(row);
  }
  addChoice('', 'Any robot');
  if (current) addChoice(current, current);
  fetch('/api/fleet/robot-groups')
    .then(function(r) { return r.json(); })
    .then(function(resp) {
      var data = (resp && resp.data) || resp || {};
      var groups = data.groups || [];
      var known = {};
      groups.forEach(function(g) { known[g.name] = true; });
      // Redraw in fleet order, keeping the saved value however the fleet answers.
      drawn = {};
      box.innerHTML = '';
      addChoice('', 'Any robot');
      groups.forEach(function(g) { addChoice(g.name, g.name, g.desc ? '- ' + g.desc : ''); });
      if (current && !known[current]) addChoice(current, current, '(not reported by the fleet)');
    })
    .catch(function() { /* keep what is drawn: Any robot + the saved value */ });
}
function closeBTCreateModal() { hideModal('bt-create-modal'); }

function openEditBTModal(btn) {
  var d = btn.dataset;
  document.getElementById('bt-edit-id').value = d.id;
  // The type's id is in the window's heading: the tabs draw no ID column.
  document.getElementById('bt-edit-heading-id').textContent = d.id ? '#' + d.id : '';
  document.getElementById('bt-edit-code').value = d.code;
  document.getElementById('bt-edit-desc').value = d.desc || '';
  document.getElementById('bt-edit-family').value = d.family || '';
  document.getElementById('bt-edit-w').value = d.width && d.width !== '0' ? d.width : '';
  document.getElementById('bt-edit-h').value = d.height && d.height !== '0' ? d.height : '';
  document.getElementById('bt-edit-l').value = d.length && d.length !== '0' ? d.length : '';
  // Blank is unknown, and a save of a blank box writes unknown: the box must
  // open on the stored value or a save of anything else on the form erases it.
  document.getElementById('bt-edit-totes-per-layer').value = d.totesPerLayer || '';
  renderRobotGroupChoices(document.getElementById('bt-edit-robot-group'), d.robotGroup || '');
  showModal('bt-edit-modal');
}
function closeBTEditModal() { hideModal('bt-edit-modal'); }

// ===== CREATE BIN MODAL =====
function openCreateBinModal() { if (createNodePicker) createNodePicker.set(''); showModal('bin-create-modal'); previewBinLabels(); }
function closeCreateBinModal() { hideModal('bin-create-modal'); }

// previewBinLabels mirrors the server's batchLabels rule (bin_service.go
// batchLabels): quantity 1 uses the label as typed; a trailing number is
// incremented across the batch; no trailing number appends 0001, 0002, ….
// Showing the generated range before save is the one place the operator can
// still catch a mis-typed prefix (the ALN_017 × 94 batch at Hopkinsville
// looked exactly like a carrier fleet on the form and like node names
// everywhere else).
function previewBinLabels() {
  var out = document.getElementById('bin-label-preview');
  if (!out) return;
  var form = document.getElementById('bin-create-modal');
  var label = form.querySelector('input[name=label_prefix]').value.trim();
  var qtyEl = form.querySelector('input[name=quantity]');
  var qty = parseInt(qtyEl.value, 10) || 1;
  if (!label) { out.textContent = ''; return; }
  var labels = batchLabelsPreview(label, qty);
  if (labels.length <= 2) {
    out.textContent = 'Creates carrier: ' + labels.join(', ');
  } else {
    out.textContent = 'Creates ' + labels.length + ' carriers: ' + labels[0] + ' … ' + labels[labels.length - 1];
  }
}

// batchLabelsPreview is the client twin of the server's label generation
// (bin_service.go batchLabels): one carrier keeps the label as typed; a batch
// increments a trailing number at its own width, else appends a 4-digit suffix.
function batchLabelsPreview(label, count) {
  count = Math.max(1, Math.min(count, 100));
  if (count === 1) return [label];
  var m = label.match(/^(.*?)(\d+)$/);
  var out = [];
  if (m) {
    var start = parseInt(m[2], 10);
    var width = m[2].length;
    for (var i = 0; i < count; i++) out.push(m[1] + String(start + i).padStart(width, '0'));
    return out;
  }
  for (var i = 0; i < count; i++) out.push(label + String(i + 1).padStart(4, '0'));
  return out;
}

// confirmBinCreate is the submit gate on the create form. preventDefault is
// first — delegateActions ignores what a handler returns, and a preventDefault
// reached after the first await arrives once the browser is already navigating
// (see handleNodeSave in nodes-detail.js for the same rule). form.submit() at
// the end does not fire a submit event, so this cannot re-enter.
//
// A multi-carrier batch is confirmed with its full label range; a single
// carrier goes through unasked. A generated label that matches an existing
// NODE name is refused outright — that collision is the fingerprint of a
// prefix typed from the wrong list (the Hopkinsville ALN_017 × 94 batch),
// and no quantity of confirmations makes it right.
async function confirmBinCreate(form, evt) {
  if (evt) evt.preventDefault();
  var label = form.querySelector('input[name=label_prefix]').value.trim();
  var qty = parseInt(form.querySelector('input[name=quantity]').value, 10) || 1;
  var labels = batchLabelsPreview(label, qty);
  var nodeNames = {};
  (PAGE_NODES || []).forEach(function(n) { nodeNames[n.name] = true; });
  var collisions = labels.filter(function(l) { return nodeNames[l]; });
  if (collisions.length > 0) {
    toast('Label ' + collisions[0] + ' is already a NODE name — carriers and nodes cannot share labels. Fix the label prefix.', 'error');
    return;
  }
  if (labels.length > 1) {
    var msg = 'Create ' + labels.length + ' carriers labeled ' + labels[0] + ' … ' + labels[labels.length - 1] + '?';
    if (!await uiConfirm(msg)) return;
  }
  form.submit();
}


// Action handler called via data-action="toggleBinTypesAccordion" —
// formerly inline document.getElementById(...).classList.toggle.
function toggleBinTypesAccordion() {
  var el = document.getElementById('bt-accordion');
  if (el) el.classList.toggle('open');
}

// A family keys Save answers with /bins#bt-accordion
// (handleBinTypeFamilyKeys): open the table the Save was made from, so the
// saved keys are in view.
if (location.hash === '#bt-accordion') toggleBinTypesAccordion();

// ─── THE BIN TYPES SECTION: A TAB PER CARRIER FAMILY ─────────────────
// The tabs are the shared ones (style guide 22): dom.js's switchTab, wrapped to
// remember the open family. Every form of the section posts and the page comes
// back on /bins, so the family a form was for is kept in sessionStorage and its
// tab reopened: the keys' Save comes back on its own family, a type made or
// edited on its family's tab (a type moved to another family on that family's),
// and a type form also reopens the section. The server is unchanged.
var BT_FAMILY = 'bins.btFamily';
var BT_REOPEN = 'bins.btReopen';

function showFamilyTab(key, btn) {
  switchTab(key, btn);
  rememberFamily(btn && btn.dataset ? btn.dataset.family : '');
}

function rememberFamily(code) {
  try { if (code) sessionStorage.setItem(BT_FAMILY, code); } catch (e) { /* no storage: the first tab opens */ }
}

// openFamilyCode is the open tab's family, '' for none.
function openFamilyCode() {
  var tab = document.querySelector('#bt-accordion .tabs .tab.active');
  return tab ? (tab.dataset.family || '') : '';
}

function openFamilyTab(code) {
  if (!code) return;
  var tabs = document.querySelectorAll('#bt-accordion .tabs .tab');
  for (var i = 0; i < tabs.length; i++) {
    if (tabs[i].dataset.family === code) { switchTab(tabs[i].dataset.tab, tabs[i]); return; }
  }
}

// rememberFormFamily is a section form's submit: the family it was for. A type
// made with no family has a family of its own, under its code.
function rememberFormFamily(evt) {
  var form = evt.target;
  if (!form || !form.getAttribute) return;
  var action = form.getAttribute('action');
  if (action === '/bin-types/family-keys') { rememberFamily(form.dataset.family); return; }
  if (action !== '/bin-types/create' && action !== '/bin-types/update') return;
  var family = form.elements.family ? form.elements.family.value.trim() : '';
  var code = form.elements.code ? form.elements.code.value.trim() : '';
  rememberFamily(family || (action === '/bin-types/create' ? code : openFamilyCode()));
  try { sessionStorage.setItem(BT_REOPEN, '1'); } catch (e) { /* the section stays as it was */ }
}
document.addEventListener('submit', rememberFormFamily);

try {
  openFamilyTab(sessionStorage.getItem(BT_FAMILY));
  if (sessionStorage.getItem(BT_REOPEN) === '1') {
    sessionStorage.removeItem(BT_REOPEN);
    if (location.hash !== '#bt-accordion') toggleBinTypesAccordion();
  }
} catch (e) { /* no storage: the first tab, the section as the page drew it */ }

// Each family tab's key strip (bins-keystrip.js): one line at rest, a Change
// per key that opens its shared picker in place, Save keys and Cancel only
// while a picked key is not saved. The form posts as it always did.
var keyStrips = new WeakMap();
document.querySelectorAll('#bt-accordion form.bt-keys').forEach(function(form) {
  keyStrips.set(form, mountKeyStrip(form));
});

function keyStripOf(el) {
  var form = el && el.closest ? el.closest('form.bt-keys') : null;
  return form ? keyStrips.get(form) : null;
}
function changeFamilyKey(key, el) { var s = keyStripOf(el); if (s) s.change(key); }
function cancelFamilyKeys(el) { var s = keyStripOf(el); if (s) s.cancel(); }

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addNote,
    askRobotToSetDown,
    bulkAction,
    clearBin,
    confirmBinCreate,
    clearSelection,
    closeBTCreateModal,
    closeBTEditModal,
    closeBinDetail,
    closeCreateBinModal,
    doBinAction,
    filterBins,
    loadPayload,
    lockBin,
    moveBin,
    openBinDetailRow,
    openCreateBTModal,
    openCreateBinModal,
    openEditBTModal,
    previewBinLabels,
    recordCount,
    showCountNote,
    requestMove,
    switchTab: showBinTab,
    showFamilyTab,
    changeFamilyKey,
    cancelFamilyKeys,
    toggleAllBins,
    toggleBinTypesAccordion,
    toggleManifest,
    updateBinProps,
    updateBulkBar
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });

// /bins?open=17 opens that bin's detail — the order pop-up links its bin here
// (app.js openFromQuery).
openFromQuery(function(id) {
  var n = parseInt(id, 10);
  if (n > 0) openBinDetail(n);
});
