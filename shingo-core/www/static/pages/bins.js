import { api, apiGet, apiPost, debounce, delegateActions, el, escapeHtml, h, hideModal, openFromQuery, showModal, timeAgoHTML, toast, uiConfirm, uiPrompt } from '/static/app.js';
import { onSSE } from '/static/shared/utils.js';
import { binRowView, uopText } from '/static/pages/bins-row.js';
import { echoesFor, newEchoLedger } from '/static/pages/bins-echo.js';

// ===== STATE =====
var currentBinId = null;
var currentBinData = null;
// journalFresh: the Journal tab shows the open bin's history as of its last
// draw. The pop-up reads the bin without history (LC12); the history is read
// when the Journal is opened, and again on an update while it is the open tab.
var journalFresh = false;
var ccState = newCCState();

function newCCState() {
  return { step: 0, bins: [], index: 0, results: [], ready: false, busy: false, seq: 0 };
}

// ===== READS =====
// readBin is the one way this page reads a bin. withHistory=false is the
// history-free read (?history=0): what the pop-up, the row and the cycle count
// draw. withHistory=true is the default answer, the same plus the whole audit,
// read only for the Journal.
function readBin(id, withHistory) {
  return apiGet('/api/bins/detail?id=' + id + (withHistory ? '' : '&history=0'));
}

function errText(e) {
  return (typeof e === 'string' && e) || (e && e.error) || 'unknown';
}

// ===== OWN-ACTION ECHOES =====
// Every action this page posts makes Core broadcast bin-update events for its
// bin (ids only, www/sse.go). The action's answer already carries the bin, so
// those echoes are not a reason to read it again. They are counted, not timed
// (bins-echo.js): each own action expects exactly the number of events its
// verb emits, for 5 s after its answer; every other event for the open bin is
// a real update and costs the one read.
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
      switchTab('overview');
      showModal('bin-detail-modal');
    })
    .catch(function(e) {
      console.error('openBinDetail', id, e);
      toast('Error loading bin: ' + errText(e), 'error');
    });
}

function closeBinDetail() {
  hideModal('bin-detail-modal');
  currentBinId = null;
  currentBinData = null;
  journalFresh = false;
}

function binDetailOpen() {
  var m = document.getElementById('bin-detail-modal');
  return !!(currentBinId && m && m.classList.contains('active'));
}

function openTab() {
  var btn = document.querySelector('#bin-detail-modal .tab-btn.active');
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

function switchTab(name) {
  if (name === 'journal' && !journalFresh) loadJournal();
  var tabs = ['overview', 'contents', 'actions', 'journal'];
  tabs.forEach(function(t) {
    var panel = document.getElementById('bd-' + t);
    var btn = document.querySelector('.tab-btn[data-action="switchTab:' + t + '"]');
    if (t === name) {
      if (panel) { panel.classList.remove('hide'); panel.style.display = ''; }
      if (btn) btn.classList.add('active');
    } else {
      if (panel) { panel.classList.add('hide'); panel.style.display = 'none'; }
      if (btn) btn.classList.remove('active');
    }
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
    html += bdField('Location', esc(ord.source_node) + ' → ' + esc(ord.delivery_node) +
      ' <span class="badge badge-in_transit">in transit</span>'); // P21: in_transit is a Signal status — show its cyan pill, not muted text
  } else {
    html += bdField('Location', b.node_name ? esc(b.node_name) : '<span class="text-muted">unassigned</span>');
  }
  html += bdField('Status', '<span class="badge badge-' + esc(b.status) + '">' + esc(b.status) + '</span>');
  var payloadDisplay = b.payload_code
    ? '<code>' + esc(b.payload_code) + '</code>'
    : (inTransit && ord && ord.payload_code
        ? '<code>' + esc(ord.payload_code) + '</code> <span class="text-muted" style="font-size:0.85em">(in transit)</span>'
        : '<span class="text-muted">empty</span>');
  html += bdField('Payload', payloadDisplay);
  html += bdField('UoP Remaining', uopText(b) + (b.payload_code ? uopBar(b.uop_remaining, data.template) : ''));
  html += bdField('Manifest', b.manifest_confirmed ? '<span style="color:var(--success)">Confirmed</span>' :
    (b.payload_code ? '<span style="color:var(--warning)">Unconfirmed</span>' : '<span class="text-muted">-</span>'));
  html += bdField('Locked', b.locked ? '<span style="color:var(--danger)">' + esc(b.locked_by) + '</span>' : 'No');
  html += bdField('Description', b.description ? esc(b.description) : '<span class="text-muted">-</span>');
  html += bdField('Bin Type', esc(b.bin_type_code));
  if (b.claimed_by) {
    html += bdField('Claimed By', 'Order #' + b.claimed_by);
  }
  if (b.last_counted_at) {
    html += bdField('Last Counted', timeAgoHTML(b.last_counted_at) + ' by ' + esc(b.last_counted_by));
  }
  html += bdField('Created', timeAgoHTML(b.created_at));
  html += bdField('Updated', timeAgoHTML(b.updated_at));
  html += '</div>';

  if (data.current_order) {
    var o = data.current_order;
    html += '<div class="action-group"><h4>Current Order</h4>';
    html += '<p>Order #' + o.id + ' &mdash; <span class="badge badge-' + esc(o.status) + '">' + esc(o.status) + '</span></p>';
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
    // The count is derived, not stored: uop_remaining x the template line's
    // parts_per_cycle. A part the template no longer lists shows "—" rather
    // than 0, because "we cannot count this" and "there are none" are
    // different things to tell someone standing at the bin.
    var perCycle = {};
    (data.template_manifest || []).forEach(function(t) {
      perCycle[t.part_number] = t.parts_per_cycle;
    });
    var uop = (b && b.uop_remaining) || 0;
    // Either key: lines are written as part_number, and bins loaded before that
    // change carry catid. Same fallback the Go reader and the inventory query
    // make — a bin does not get its jsonb rewritten under a running plant.
    html += h`<table class="table-compact"><thead><tr><th>Part number</th><th>Qty</th><th>Notes</th></tr></thead><tbody>${
      data.manifest.items.map(function(item) {
        var part = item.part_number || item.catid;
        var ppc = perCycle[part];
        var qty = ppc == null ? '—' : String(uop * ppc);
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
    html += '<div class="action-group"><h4>Load Payload</h4>';
    html += '<div class="inline-form">';
    html += '<div class="form-group"><label>Payload</label><select id="bd-load-payload" style="width:200px"><option value="">-- Select --</option>';
    (PAGE_PAYLOADS || []).forEach(function(p) {
      var allowed = (PAGE_PAYLOAD_BIN_TYPES || {})[p.code];
      if (allowed && allowed.length > 0 && allowed.indexOf(b.bin_type_id) === -1) return;
      html += '<option value="' + esc(p.code) + '">' + esc(p.code) + '</option>';
    });
    html += '</select></div>';
    html += '<div class="form-group"><label>Count (blank = full)</label><input type="number" id="bd-load-uop" min="0" style="width:110px" placeholder="full pack"></div>';
    html += '<button class="btn btn-primary btn-sm" data-action="loadPayload">Load</button>';
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
}

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
    html += '<p class="text-muted mb-1">Locked by <strong>' + esc(b.locked_by) + '</strong></p>';
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
  html += '<div class="form-group"><label>Destination</label><select id="bd-move-node" style="width:200px"><option value="">-- Select --</option>';
  (PAGE_NODES || []).forEach(function(n) {
    if (b.node_id && n.id === b.node_id) return; // skip current location
    // _TRANSIT and robot decks are not places; a bin reaches them only by a
    // robot lifting it (BinService.MoveByHand refuses them too).
    if (n.name === '_TRANSIT' || n.name.indexOf('_ROBOT:') === 0) return;
    html += '<option value="' + n.id + '">' + esc(n.name) + '</option>';
  });
  html += '</select></div>';
  html += '<button class="btn btn-sm" data-action="moveBin">Move</button>';
  html += '<button class="btn btn-sm" data-action="requestTransport">Request Transport</button>';
  html += '</div></div>';

  // Record Count
  html += '<div class="action-group"><h4>Record Count</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Actual UoP</label><input type="number" id="bd-count-uop" min="0" style="width:80px" value="' + b.uop_remaining + '"></div>';
  html += '<div class="form-group"><label>Counter</label><input type="text" id="bd-count-actor" placeholder="Name" style="width:150px"></div>';
  html += '<button class="btn btn-sm" data-action="recordCount">Record</button>';
  html += '</div></div>';

  // Edit Properties
  html += '<div class="action-group"><h4>Properties</h4>';
  html += '<div class="inline-form">';
  html += '<div class="form-group"><label>Label</label><input type="text" id="bd-edit-label" value="' + esc(b.label) + '" style="width:150px"></div>';
  html += '<div class="form-group"><label>Description</label><input type="text" id="bd-edit-desc" value="' + esc(b.description || '') + '" style="width:200px"></div>';
  html += '<div class="form-group"><label>Bin Type</label><select id="bd-edit-bin-type" style="width:160px">';
  (PAGE_BIN_TYPES || []).forEach(function(bt) {
    var sel = (bt.id === b.bin_type_id) ? ' selected' : '';
    html += '<option value="' + bt.id + '"' + sel + '>' + esc(bt.code) + '</option>';
  });
  html += '</select></div>';
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
      if (!resp || !resp.bin) { refreshBinRow(id); return; }
      clearFields(reset || []);
      paintBinRow(resp);
      updateOpenBin(resp, reset);
    })
    .catch(function(e) { toast('Error: ' + (e.error || e), 'error'); });
}

function loadPayload() {
  var code = document.getElementById('bd-load-payload').value;
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
  var nodeId = parseInt(document.getElementById('bd-move-node').value);
  if (!nodeId) { toast('Select a destination', 'info'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    toast('Bin is already at this location', 'info'); return;
  }
  runBinAction('move', { node_id: nodeId }, ['bd-move-node']);
}

function requestTransport() {
  var nodeId = parseInt(document.getElementById('bd-move-node').value);
  if (!nodeId) { toast('Select a destination', 'info'); return; }
  if (currentBinData && currentBinData.bin.node_id && nodeId === currentBinData.bin.node_id) {
    toast('Bin is already at this location', 'info'); return;
  }
  apiPost('/api/bins/request-transport', { bin_id: currentBinId, destination_node_id: nodeId })
    .then(function(data) { toast(data.message || 'Transport requested', 'info'); openBinDetail(currentBinId); refreshBinRow(currentBinId); })
    .catch(function(e) { toast('Error: ' + (e.error || e), 'error'); });
}

// askRobotToSetDown is the bins page's Return button: the recover_carried_bin
// recovery action, which sends the bin on a robot back to where a claim
// declares it is used from — the same chooser the cancel-return watch uses.
//
// THE ONLY ACTION ON THIS PAGE THAT PUTS A ROBOT IN MOTION, which is why the
// answer is shown rather than swallowed. On success the detail names the
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

function recordCount() {
  var uop = parseInt(document.getElementById('bd-count-uop').value) || 0;
  var actor = document.getElementById('bd-count-actor').value.trim();
  runBinAction('record_count', { actual_uop: uop, actor: actor }, ['bd-count-uop', 'bd-count-actor']);
}

function updateBinProps() {
  var label = document.getElementById('bd-edit-label').value.trim();
  var desc = document.getElementById('bd-edit-desc').value.trim();
  var binTypeID = parseInt(document.getElementById('bd-edit-bin-type').value, 10);
  var params = { label: label, description: desc };
  if (binTypeID) params.bin_type_id = binTypeID;
  runBinAction('update', params, ['bd-edit-label', 'bd-edit-desc', 'bd-edit-bin-type']);
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

// refreshBinRow reads one bin without history and repaints its row. Only the
// callers whose answer does not carry the bin use it (Return, Request
// Transport); an action's answer is painted directly.
function refreshBinRow(id) {
  readBin(id, false)
    .then(paintBinRow)
    .catch(function(err) { console.error('refreshBinRow', id, err); });
}

// paintBinRow draws one table row from a detail answer as bins.html draws it
// (bins-row.js binRowView): Location's display name, the Return button, and
// the sort and search values with the cells.
function paintBinRow(detail) {
  var b = detail && detail.bin;
  if (!b) return;
  var row = document.querySelector('#bin-table tbody tr[data-id="' + b.id + '"]');
  if (!row) return;
  var v = binRowView(detail, PAGE_AUTH);
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
  // Label, Type, Location, Payload, UoP, Status, Flags
  cell(0, v.label.html, v.label.sort);
  if (tds[off + 1]) tds[off + 1].textContent = v.type.text;
  cell(2, v.location.html, v.location.sort);
  cell(3, v.payload.html, v.payload.sort);
  cell(4, v.uop.html, v.uop.sort);
  cell(5, v.status.html, v.status.sort);
  // The detail API does not carry the notes flag; keep the one the server
  // painted rather than dropping it on every refresh.
  var notes = tds[off + 6] && tds[off + 6].querySelector('[title="Has notes"]');
  cell(6, v.flags.html + (notes ? notes.outerHTML : '') + v.flags.returnHTML);
}

function clearSelection() {
  document.querySelectorAll('.bin-cb').forEach(function(cb) { cb.checked = false; });
  var hdr = document.querySelector('#bin-table thead input[type=checkbox]');
  if (hdr) hdr.checked = false;
  updateBulkBar();
}

// ===== CYCLE COUNT =====
function openCycleCount() {
  // Preview: count visible rows with payloads
  var rows = document.querySelectorAll('#bin-table tbody tr');
  var count = 0;
  rows.forEach(function(row) {
    if (row.style.display !== 'none' && row.dataset.payload) count++;
  });
  document.getElementById('cc-preview-count').textContent = count + ' bins to count';
  document.getElementById('cc-step1').classList.remove('hide');
  document.getElementById('cc-step2').classList.add('hide');
  document.getElementById('cc-step3').classList.add('hide');
  showModal('cc-modal');
}

function closeCycleCount() {
  hideModal('cc-modal');
  ccState = newCCState();
}

// A wizard dismissed by its backdrop is closed too: without this its state
// (bins, index, results) outlived the dismiss.
var ccModalEl = document.getElementById('cc-modal');
if (ccModalEl) ccModalEl.addEventListener('backdropclose', function() { ccState = newCCState(); });

function ccStart() {
  var rows = document.querySelectorAll('#bin-table tbody tr');
  ccState.bins = [];
  rows.forEach(function(row) {
    if (row.style.display !== 'none' && row.dataset.payload) {
      ccState.bins.push({
        id: parseInt(row.dataset.id),
        label: row.dataset.label,
        node: row.dataset.node,
        payload: row.dataset.payload,
        uop: parseInt(row.dataset.uop) || 0,
        uopCapacity: parseInt(row.dataset.uopCapacity) || 0
      });
    }
  });
  if (ccState.bins.length === 0) { toast('No bins with payloads to count', 'info'); return; }
  ccState.index = 0;
  ccState.results = [];
  document.getElementById('cc-step1').classList.add('hide');
  document.getElementById('cc-step2').classList.remove('hide');
  document.getElementById('cc-total').textContent = ccState.bins.length;
  ccShowBin();
}

// ccShowBin opens step 2 on the next bin and reads it again (history-free):
// the count the wizard shows, and the one Confirm records, is the bin's count
// now, not the one the table was painted with when the page loaded. Confirm
// and Record Discrepancy wait for that read.
function ccShowBin() {
  var st = ccState;
  var bin = st.bins[st.index];
  var seq = ++st.seq;
  st.ready = false;
  document.getElementById('cc-index').textContent = st.index + 1;
  var pct = ((st.index) / st.bins.length * 100);
  document.getElementById('cc-progress-bar').style.width = pct + '%';
  ccPaintCard(bin, '&hellip;');
  document.getElementById('cc-actual').value = '';
  readBin(bin.id, false)
    .then(function(resp) {
      if (st !== ccState || seq !== st.seq) return;
      bin.uop = resp.bin.uop_remaining;
      bin.uopCapacity = resp.bin.uop_capacity || 0;
      ccPaintCard(bin, String(bin.uop));
      document.getElementById('cc-actual').value = bin.uop;
      st.ready = true;
    })
    .catch(function(e) {
      if (st !== ccState || seq !== st.seq) return;
      ccPaintCard(bin, 'could not read (' + esc(errText(e)) + ')');
      toast('Could not read ' + bin.label + ': ' + errText(e) + '. Skip it or flag it.', 'error');
    });
}

function ccPaintCard(bin, expectedHTML) {
  document.getElementById('cc-bin-card').innerHTML =
    '<div class="cc-label">' + esc(bin.label) + '</div>' +
    '<div class="text-muted">' + esc(bin.node || 'unassigned') + ' &middot; ' + esc(bin.payload) + '</div>' +
    '<div style="margin-top:0.5rem">Expected UoP: <strong>' + expectedHTML + '</strong></div>';
  document.getElementById('cc-actual').max = bin.uopCapacity || '';
  var hint = document.getElementById('cc-capacity-hint');
  if (bin.uopCapacity > 0) {
    hint.textContent = 'Max: ' + bin.uopCapacity;
    hint.style.display = 'block';
  } else {
    hint.textContent = '';
    hint.style.display = 'none';
  }
  document.getElementById('cc-actual').focus();
}

// ccPost posts one wizard step and waits for it. The step is counted only
// when the server took it; a refusal is shown and the wizard stays on the
// bin (Skip or Flag moves on).
async function ccPost(action, params, result) {
  var st = ccState;
  if (st.busy) return;
  var bin = st.bins[st.index];
  if (!bin) return;
  st.busy = true;
  try {
    await postBinAction(bin.id, action, params);
  } catch (e) {
    if (st === ccState) toast(bin.label + ' not recorded: ' + errText(e), 'error');
    return;
  } finally {
    st.busy = false;
  }
  if (st !== ccState) return;
  st.results.push(Object.assign({ id: bin.id, label: bin.label }, result));
  ccAdvance();
}

function ccConfirm() {
  var st = ccState;
  var bin = st.bins[st.index];
  if (!bin || !st.ready) return;
  var actor = document.getElementById('cc-actor').value.trim() || 'cycle_count';
  ccPost('record_count', { actual_uop: bin.uop, actor: actor },
    { result: 'match', expected: bin.uop, actual: bin.uop });
}

function ccDiscrepancy() {
  var st = ccState;
  var bin = st.bins[st.index];
  if (!bin || !st.ready) return;
  var actual = parseInt(document.getElementById('cc-actual').value) || 0;
  if (bin.uopCapacity > 0) {
    actual = Math.max(0, Math.min(actual, bin.uopCapacity));
    document.getElementById('cc-actual').value = actual;
  }
  var actor = document.getElementById('cc-actor').value.trim() || 'cycle_count';
  ccPost('record_count', { actual_uop: actual, actor: actor },
    { result: 'discrepancy', expected: bin.uop, actual: actual });
}

function ccSkip() {
  var st = ccState;
  var bin = st.bins[st.index];
  if (!bin || st.busy) return;
  st.results.push({ id: bin.id, label: bin.label, result: 'skipped' });
  ccAdvance();
}

function ccFlag() {
  if (!ccState.bins[ccState.index]) return;
  ccPost('flag', {}, { result: 'flagged' });
}

function ccAdvance() {
  ccState.index++;
  if (ccState.index >= ccState.bins.length) {
    ccSummary();
  } else {
    ccShowBin();
  }
}

function ccSummary() {
  document.getElementById('cc-step2').classList.add('hide');
  document.getElementById('cc-step3').classList.remove('hide');
  var matched = 0, disc = 0, skipped = 0, flagged = 0;
  ccState.results.forEach(function(r) {
    if (r.result === 'match') matched++;
    else if (r.result === 'discrepancy') disc++;
    else if (r.result === 'skipped') skipped++;
    else if (r.result === 'flagged') flagged++;
  });
  var html = '<div class="grid grid-4 mb-2">';
  html += '<div class="stat"><div class="value">' + matched + '</div><div class="label">Matched</div></div>';
  html += '<div class="stat"><div class="value" style="color:var(--warning)">' + disc + '</div><div class="label">Discrepancies</div></div>';
  html += '<div class="stat"><div class="value">' + skipped + '</div><div class="label">Skipped</div></div>';
  html += '<div class="stat"><div class="value" style="color:var(--danger)">' + flagged + '</div><div class="label">Flagged</div></div>';
  html += '</div>';
  if (disc > 0) {
    html += '<h4 style="font-size:0.85rem;margin-bottom:0.5rem">Discrepancies</h4>';
    html += '<table class="table-compact"><thead><tr><th>Bin</th><th>Expected</th><th>Actual</th><th>Diff</th></tr></thead><tbody>';
    ccState.results.forEach(function(r) {
      if (r.result === 'discrepancy') {
        var diff = r.actual - r.expected;
        html += '<tr><td><code>' + esc(r.label) + '</code></td><td>' + r.expected + '</td><td>' + r.actual + '</td>';
        html += '<td style="color:' + (diff < 0 ? 'var(--danger)' : 'var(--success)') + '">' + (diff > 0 ? '+' : '') + diff + '</td></tr>';
      }
    });
    html += '</tbody></table>';
  }
  document.getElementById('cc-summary').innerHTML = html;
}

// ===== SSE =====
// Subscribed on the shared onSSE bus (shared/utils.js); the handler receives
// the parsed payload. Replaces the retired app.js IIFE window.onBinUpdate
// dispatch (Q-002).
//
// The event carries ids only. The counted echoes of this page's own actions
// are dropped (the action's answer already drew the bin); any other update to
// the open bin costs one read: history-free, or with history when the Journal
// is the open tab. The redraw keeps the open tab and anything typed.
var refreshOpenBin = debounce(function() {
  if (!binDetailOpen()) return;
  var id = currentBinId;
  readBin(id, openTab() === 'journal')
    .then(function(resp) { updateOpenBin(resp); })
    .catch(function(e) { console.error('bin-update refresh', id, e); });
}, 500);

onSSE('bin-update', function(data) {
  if (!data || !data.bin_id) return;
  // Taken for every bin, open or not, so a counted echo is consumed where it
  // lands and cannot be mistaken for a later real update.
  if (echoes.take(data.bin_id, Date.now())) return;
  if (!currentBinId || currentBinId !== data.bin_id) return;
  refreshOpenBin();
});

// ===== HELPERS =====
function esc(s) { return escapeHtml(s); }

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
function openCreateBTModal() {
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
  document.getElementById('bt-edit-code').value = d.code;
  document.getElementById('bt-edit-desc').value = d.desc || '';
  document.getElementById('bt-edit-w').value = d.width && d.width !== '0' ? d.width : '';
  document.getElementById('bt-edit-h').value = d.height && d.height !== '0' ? d.height : '';
  document.getElementById('bt-edit-l').value = d.length && d.length !== '0' ? d.length : '';
  renderRobotGroupChoices(document.getElementById('bt-edit-robot-group'), d.robotGroup || '');
  showModal('bt-edit-modal');
}
function closeBTEditModal() { hideModal('bt-edit-modal'); }

// ===== CREATE BIN MODAL =====
function openCreateBinModal() { showModal('bin-create-modal'); previewBinLabels(); }
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
// (bin_service.go batchLabels): trailing number incremented at its own
// width, else a 4-digit suffix appended.
function batchLabelsPreview(label, count) {
  count = Math.max(1, Math.min(count, 100));
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

// ===== KEYBOARD =====
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    closeBinDetail();
    closeBTCreateModal(); closeBTEditModal();
    closeCreateBinModal(); closeCycleCount();
  }
  // Cycle count shortcuts: only while the wizard is open on step 2. Step 2 is
  // hidden by class, not by style, so the old style test was always true and
  // every Enter, Tab and F on the page went to the wizard.
  var ccModal = document.getElementById('cc-modal');
  var step2 = document.getElementById('cc-step2');
  if (ccModal && ccModal.classList.contains('active') && step2 && !step2.classList.contains('hide')) {
    if (e.key === 'Enter' && !e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      ccConfirm();
    } else if (e.key === 'Tab') {
      e.preventDefault();
      ccSkip();
    } else if (e.key === 'f' || e.key === 'F') {
      if (document.activeElement && document.activeElement.tagName !== 'INPUT' && document.activeElement.tagName !== 'TEXTAREA') {
        e.preventDefault();
        ccFlag();
      }
    }
  }
});

// Action handler called via data-action="toggleBinTypesAccordion" —
// formerly inline document.getElementById(...).classList.toggle.
function toggleBinTypesAccordion() {
  var el = document.getElementById('bt-accordion');
  if (el) el.classList.toggle('open');
}

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
    ccConfirm,
    ccDiscrepancy,
    ccFlag,
    ccSkip,
    ccStart,
    clearBin,
    confirmBinCreate,
    clearSelection,
    closeBTCreateModal,
    closeBTEditModal,
    closeBinDetail,
    closeCreateBinModal,
    closeCycleCount,
    doBinAction,
    filterBins,
    loadPayload,
    lockBin,
    moveBin,
    openBinDetailRow,
    openCreateBTModal,
    openCreateBinModal,
    openCycleCount,
    openEditBTModal,
    previewBinLabels,
    recordCount,
    requestTransport,
    switchTab,
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
