import { debounce, escapeHtml, formatDuration, formatTime, installLiveDurations, onSSE, serverNow } from '/static/shared/utils.js';
import { formatClockSeconds } from '/static/components/plantclock.js';
import { relevantNotices } from '/static/pages/fleet-notices.js';

// Mission detail: the order's life as STAGES, one row per status span.
//
// The source of truth is order_history (served as `history`): every status the
// order held, with the instant it entered it. One row per span — stage, where,
// started, took, share — and one bar on a true time scale in four classes.
// The spans tile the order's life by construction: each ends where the next
// begins, the last ends at the terminal row (or now, in flight). There is no
// "unaccounted" remainder because nothing is reconciled against a second clock.
//
// Robot actions (the fleet's per-block legs and vendor transitions, from
// mission_events) are still recorded — engine/stranded_transit.go reads the
// BLOCK_FINISHED rows — and render once, inside a closed disclosure, grouped by
// the stage they happened in.
(function() {
  var orderID = document.getElementById('mission-order-id').textContent;

  // STAGE_CLASS sorts every non-terminal order status into one of the bar's
  // four classes. Keyed on protocol statuses (pinned by
  // mission_state_vocabulary_drift_test.go: every key is a real status and every
  // non-terminal status has a key). Terminal statuses are absent on purpose:
  // they end the order's life rather than occupy any of it.
  //
  //   waiting — the order exists and no robot has been asked for yet.
  //   moving  — handed to the fleet: dispatched, acknowledged, in transit.
  //   held    — a robot is parked on the order: staged at a wait, or a fault
  //             with no stage before it to fold into.
  //   confirm — delivered, waiting for the station to confirm. No robot is on
  //             the order any more, so it is not held; it draws neutral.
  var STAGE_CLASS = {
    'pending': 'waiting',
    'sourcing': 'waiting',
    'queued': 'waiting',
    'submitted': 'waiting',
    'reshuffling': 'waiting',
    'dispatched': 'moving',
    'acknowledged': 'moving',
    'in_transit': 'moving',
    'staged': 'held',
    'delivered': 'confirm',
    'faulted': 'held'
  };

  var CLASS_LABEL = { waiting: 'waiting to dispatch', moving: 'moving', held: 'held', confirm: 'waiting for confirm' };
  var CLASS_ORDER = ['waiting', 'moving', 'held', 'confirm'];

  // STOPPED is the fleet's teardown state, read from the vendor event stream on
  // purpose: whether the fleet tore the mission down is the FLEET's fact, so its
  // own word is the right one to test. It decides only the `not run` leg form.
  var STOPPED_STATE = 'STOPPED';

  function ms(ts) { return Date.parse(ts); }

  // A muted em dash with a title saying which absence it is (guide rule 4: no
  // data, zero and not applicable look different).
  function noData(why) {
    return '<span class="text-muted" title="' + escapeHtml(why) + '">—</span>';
  }

  function stateBadge(status, rawState) {
    if (!status) return '';
    var title = rawState && rawState !== status ? ' title="fleet reported: ' + escapeHtml(rawState) + '"' : '';
    return '<span class="badge badge-' + status + '"' + title + '>' + status + '</span>';
  }

  // ── Stages ─────────────────────────────────────────────────────────────

  // isTerminal is "has no stage class": the table above lists every status that
  // can hold an order, so anything else ends it.
  function isTerminal(status) {
    return !Object.prototype.hasOwnProperty.call(STAGE_CLASS, status);
  }

  // buildStages folds order_history into status spans.
  //
  // Consecutive rows with the same status are one span (a re-queue with a new
  // reason is still the same wait). A faulted row folds into the span it
  // interrupted — its time counts there, and is counted as `faults` / `lostMs` —
  // and when the fleet recovers back into that same status the continuation
  // folds in too, so nine replans are one row saying so, not eighteen. A fault
  // with nothing before it stands as its own held span.
  //
  // Returns { spans, end, lifeMs }: `end` is the terminal row, or null while in
  // flight, when the last span is open (ongoing) and runs to nowMs.
  function buildStages(history, nowMs) {
    var rows = history || [];
    var spans = [];
    var end = null;
    var cur = null;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var at = ms(r.created_at);
      if (isTerminal(r.status)) {
        end = { status: r.status, at: r.created_at, detail: r.detail || '' };
        break;
      }
      var next = rows[i + 1];
      var took = Math.max(0, (next ? ms(next.created_at) : nowMs) - at);
      if (r.status === 'faulted' && cur) {
        cur.faults++;
        cur.lostMs += took;
        cur.ms += took;
        continue;
      }
      if (cur && r.status === cur.status) {
        cur.ms += took;
        continue;
      }
      cur = {
        status: r.status,
        cls: STAGE_CLASS[r.status],
        startedAt: r.created_at,
        startMs: at,
        detail: r.detail || '',
        ms: took,
        faults: 0,
        lostMs: 0,
        legs: [],
        events: []
      };
      spans.push(cur);
    }
    if (!end && spans.length) spans[spans.length - 1].ongoing = true;
    var lifeMs = 0;
    for (var s = 0; s < spans.length; s++) lifeMs += spans[s].ms;
    return { spans: spans, end: end, lifeMs: lifeMs };
  }

  // spanAt returns the span an instant falls in: the last one that started at
  // or before it. Before the first span it is the first.
  function spanAt(spans, t) {
    var found = spans[0];
    for (var i = 0; i < spans.length; i++) {
      if (spans[i].startMs <= t) found = spans[i];
    }
    return found;
  }

  // ── Legs (robot actions) ───────────────────────────────────────────────

  function hasVendorTimes(blk) {
    return blk && blk.startTime > 0 && blk.terminateTime >= blk.startTime;
  }

  function legLabel(blk) {
    var task = (blk.binTask || '').toLowerCase();
    var verb = 'handle';
    if (task.indexOf('wait') >= 0) verb = 'wait';
    else if (task.indexOf('unload') >= 0 || task.indexOf('drop') >= 0 || task.indexOf('release') >= 0) verb = 'unload';
    else if (task.indexOf('load') >= 0 || task.indexOf('pick') >= 0) verb = 'load';
    return verb + (blk.location ? ' @ ' + blk.location : '');
  }

  // A leg's duration is one of three answers and they render three ways:
  //   - a measurement (both vendor endpoints, terminate >= start), through
  //     formatDuration, which prints a real zero as "0 s";
  //   - unknown: the fleet did not report usable endpoints;
  //   - not run: the trailing run of zero-length blocks on a mission the fleet
  //     STOPPED. Tearing an order down stamps its outstanding blocks rather than
  //     executing them, so equal stamps there record the teardown, not work.
  // The not-run rule is scoped to the TRAILING run: a zero before a block with
  // real duration was a genuine sub-second reading. An unknown block breaks the
  // run rather than extending it — missing data is not evidence.
  function classifyLegs(legs, stopped) {
    var notRunFrom = legs.length;
    if (stopped) {
      while (notRunFrom > 0) {
        var b = legs[notRunFrom - 1].block;
        if (!hasVendorTimes(b) || b.terminateTime !== b.startTime) break;
        notRunFrom--;
      }
    }
    for (var i = 0; i < legs.length; i++) {
      var blk = legs[i].block;
      if (i >= notRunFrom) legs[i].form = 'not run';
      else if (!hasVendorTimes(blk)) legs[i].form = 'unknown';
      else legs[i].tookMs = (blk.terminateTime - blk.startTime) * 1000;
    }
  }

  // attachEvents files every mission event under the stage it happened in and
  // returns how many it filed. A leg goes by its block's START (vendor time)
  // when the fleet reported one: a wait block's completion row is written as
  // the robot is released, which is already the next stage, but the waiting
  // happened in the staged one. Everything else goes by the row's own Core
  // timestamp.
  function attachEvents(groups, events) {
    var legs = [];
    var n = 0;
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      if (!ev.is_leg) {
        spanAt(groups, ms(ev.created_at)).events.push({ ev: ev });
        n++;
        continue;
      }
      var blocks = [];
      try { blocks = JSON.parse(ev.blocks_json || '[]'); } catch (e) { console.error('mission leg parse', e); }
      for (var b = 0; b < blocks.length; b++) legs.push({ ev: ev, block: blocks[b] });
    }
    classifyLegs(legs, events.some(function(e) { return e && e.new_state === STOPPED_STATE; }));
    for (var k = 0; k < legs.length; k++) {
      var t = hasVendorTimes(legs[k].block) ? legs[k].block.startTime * 1000 : ms(legs[k].ev.created_at);
      var span = spanAt(groups, t);
      span.legs.push(legs[k]);
      span.events.push(legs[k]);
      n++;
    }
    return n;
  }

  // where names the places the robot worked during the span, in order, from
  // the legs filed under it. Empty when the fleet reported none — an order
  // waiting in the queue has no robot and no place to name.
  function where(span) {
    var seen = [];
    for (var i = 0; i < span.legs.length; i++) {
      var loc = span.legs[i].block.location;
      if (loc && seen[seen.length - 1] !== loc) seen.push(loc);
    }
    return seen.join(' → ');
  }

  // ── Rendering ──────────────────────────────────────────────────────────

  function share(partMs, lifeMs) {
    if (!(lifeMs > 0)) return '';
    var pct = partMs / lifeMs * 100;
    if (partMs > 0 && pct < 0.5) return '&lt;1%';
    return Math.round(pct) + '%';
  }

  // The stage table's clock shows seconds on every row: spans routinely start
  // within the same minute (dispatch, acknowledge and depart are seconds apart),
  // and minute resolution made their order unreadable. The full plant-local
  // datetime rides on the title.
  function clockCell(ts) {
    return '<td class="tnum" title="' + escapeHtml(formatTime(ts)) + '">' + formatClockSeconds(ts) + '</td>';
  }

  function tookCell(span) {
    if (span.ongoing) {
      return '<td class="col-num tnum"><span data-since="' + escapeHtml(span.startedAt) + '">'
        + formatDuration(span.ms) + '</span> <span class="text-muted">so far</span></td>';
    }
    return '<td class="col-num tnum">' + formatDuration(span.ms) + '</td>';
  }

  function renderStages(st) {
    var bar = document.getElementById('stage-bar');
    var legend = document.getElementById('stage-legend');
    var tbody = document.getElementById('stage-rows');

    if (!st.spans.length && !st.end) {
      bar.innerHTML = '';
      legend.innerHTML = '';
      tbody.innerHTML = '<tr><td colspan="5" class="empty-cell" title="order_history has no rows for this order">No status history recorded</td></tr>';
      return;
    }

    // The bar: one segment per span, width proportional to its time — a true
    // scale, no minimum width, so a five-second stage next to an eleven-minute
    // one looks like what it is. The hover names it.
    var segs = '';
    var byClass = { waiting: 0, moving: 0, held: 0, confirm: 0 };
    for (var i = 0; i < st.spans.length; i++) {
      var sp = st.spans[i];
      byClass[sp.cls] += sp.ms;
      segs += '<div class="stage-seg stage-' + sp.cls + '" style="flex:' + sp.ms + ' 0 0"'
        + ' title="' + sp.status + ' · ' + formatDuration(sp.ms) + '"></div>';
    }
    bar.innerHTML = segs;

    var keys = '';
    for (var c = 0; c < CLASS_ORDER.length; c++) {
      var k = CLASS_ORDER[c];
      keys += '<span class="stage-key"><span class="stage-swatch stage-' + k + '"></span>'
        + CLASS_LABEL[k] + ' <span class="tnum">' + formatDuration(byClass[k]) + '</span>'
        + ' <span class="text-muted tnum">' + share(byClass[k], st.lifeMs) + '</span></span>';
    }
    legend.innerHTML = keys;

    var html = '';
    for (var j = 0; j < st.spans.length; j++) {
      var s = st.spans[j];
      var faults = '';
      if (s.faults > 0) {
        faults = '<div class="stage-faults">' + s.faults + (s.faults === 1 ? ' fault' : ' faults')
          + ', ' + formatDuration(s.lostMs) + ' lost</div>';
      }
      html += '<tr class="stage-row">'
        + '<td><span class="stage-swatch stage-' + s.cls + '"></span>' + stateBadge(s.status)
        + (s.detail ? '<div class="stage-detail">' + escapeHtml(s.detail) + '</div>' : '') + faults + '</td>'
        + '<td>' + escapeHtml(where(s)) + '</td>'
        + clockCell(s.startedAt)
        + tookCell(s)
        + '<td class="col-num tnum">' + share(s.ms, st.lifeMs) + '</td>'
        + '</tr>';
    }
    if (st.end) {
      html += '<tr class="stage-row stage-end">'
        + '<td>' + stateBadge(st.end.status)
        + (st.end.detail ? '<div class="stage-detail">' + escapeHtml(st.end.detail) + '</div>' : '') + '</td>'
        + '<td></td>' + clockCell(st.end.at) + '<td></td><td></td></tr>';
    }
    tbody.innerHTML = html;
  }

  function formatRoute(order) {
    if (order.steps_json) {
      try {
        var steps = JSON.parse(order.steps_json);
        var nodes = [];
        for (var i = 0; i < steps.length; i++) {
          if (!steps[i].node) continue;
          nodes.push(escapeHtml(steps[i].node)
            + (steps[i].action === 'wait' ? ' <span class="text-muted">(wait)</span>' : ''));
        }
        if (nodes.length > 0) return nodes.join(' &rarr; ');
      } catch (e) { console.error('orderRoute steps parse', e); }
    }
    return escapeHtml(order.source_node || '?') + ' &rarr; ' + escapeHtml(order.delivery_node || '?');
  }

  function field(label, title, value) {
    return '<div title="' + escapeHtml(title) + '"><strong>' + label + '</strong><br>' + value + '</div>';
  }

  // The summary reads the live order and its history, so an order still in
  // flight shows when it was created, that it has not ended, and how long it has
  // been alive — not a row of dashes waiting for a telemetry summary that is
  // only written at a terminal state.
  function renderSummary(data, st) {
    var o = data.order || {};
    var t = data.telemetry;
    var robot = (t && t.robot_id) || o.robot_id;
    var startedAt = st.spans.length ? st.spans[0].startedAt : o.created_at;

    var html = '<div class="mission-summary-grid">';
    html += field('Order ID', 'Shingo order ID', '<a href="/orders?open=' + o.id + '">' + o.id + '</a>');
    html += field('Type', 'Transport order type', escapeHtml(o.order_type || ''));
    html += field('Station', 'Edge station that requested this order', escapeHtml(o.station_id || ''));
    html += field('Robot', 'Robot vehicle ID assigned by the fleet',
      robot ? escapeHtml(robot) : '<span class="text-muted">not assigned</span>');
    html += field('Route', 'The order\'s planned steps, source to delivery', formatRoute(o));
    html += field('Status', 'Current order status in Shingo', stateBadge(o.status));
    html += field('Created', 'When Shingo created this order', formatTime(startedAt));
    html += field('Ended', 'When the order reached a terminal status',
      st.end ? formatTime(st.end.at) : '<span class="text-muted">in flight</span>');
    // Duration is the Missions list's figure under the Missions list's name:
    // created to the fleet's terminal report (telemetry.duration_ms). Order life
    // runs on to the terminal history row, so the two differ by the wait for
    // confirmation; each is named for what it measures. No telemetry row until
    // a terminal state, so in flight it is the list's in-flight figure too:
    // ticking from the order's created_at, "so far" (missions.js refreshList).
    // A finished order the fleet never summarised is a titled absence.
    var duration;
    if (t && t.duration_ms > 0) {
      duration = '<span class="tnum">' + formatDuration(t.duration_ms) + '</span>';
    } else if (!st.end && o.created_at) {
      duration = '<span class="tnum" data-since="' + escapeHtml(o.created_at) + '">'
        + formatDuration(Math.max(0, serverNow() - Date.parse(o.created_at))) + '</span>'
        + ' <span class="text-muted">so far</span>';
    } else {
      duration = noData('not reported by the fleet');
    }
    html += field('Duration', 'Core created to the fleet\'s terminal report: the Missions list\'s Duration', duration);
    html += field('Order life', 'Created to terminal status (to now, in flight): the sum of the stages below',
      st.end
        ? '<span class="tnum">' + formatDuration(st.lifeMs) + '</span>'
        : '<span class="tnum" data-since="' + escapeHtml(startedAt) + '">' + formatDuration(st.lifeMs) + '</span>');
    html += '</div>';

    // The fleet's own clock, only once there is a fleet summary to read it from
    // (written at a terminal state). Absent values say so rather than dash out.
    if (t) {
      var missing = 'not reported by the fleet';
      html += '<div class="mission-summary-grid mission-summary-fleet">';
      html += field('Fleet duration', 'Time measured by the fleet backend (create to terminal)',
        t.vendor_duration_ms > 0 ? '<span class="tnum">' + formatDuration(t.vendor_duration_ms) + '</span>' : noData(missing));
      html += field('Fleet created', 'When the fleet backend created the transport order',
        t.vendor_created ? formatTime(t.vendor_created) : noData(missing));
      html += field('Fleet completed', 'When the fleet backend reported the terminal state',
        t.vendor_completed ? formatTime(t.vendor_completed) : noData(missing));
      html += '</div>';
    }
    document.getElementById('mission-summary').innerHTML = html;
  }

  // position renders the robot's map coordinates, or nothing. The robot-status
  // cache the event snapshot is taken from holds 0/0 when the fleet supplies no
  // position (the simulator never does), so the exact origin reads as absence,
  // not as a place (guide rule 4: no data is not zero).
  function position(ev) {
    if (ev.robot_x == null || ev.robot_y == null) return '';
    if (ev.robot_x === 0 && ev.robot_y === 0) return '';
    return '(' + ev.robot_x.toFixed(1) + ', ' + ev.robot_y.toFixed(1) + ')';
  }

  function legWhat(leg) {
    var took;
    if (leg.form === 'not run') took = '<span class="text-muted" title="the fleet stopped this mission before this leg">not run</span>';
    else if (leg.form === 'unknown') took = '<span class="text-muted" title="duration not reported by the fleet">unknown</span>';
    else took = '<span class="tnum">' + formatDuration(leg.tookMs) + '</span>';
    return escapeHtml(legLabel(leg.block)) + ' · ' + took;
  }

  // renderActions fills the closed disclosure: every recorded robot action,
  // grouped under the stage it happened in. This is the only place mission
  // events render; the old timeline and event log printed each one twice.
  function renderActions(groups, total) {
    document.getElementById('mission-actions-count').textContent = String(total);
    var host = document.getElementById('mission-actions');
    if (!total) {
      host.innerHTML = '<p class="text-muted">The fleet reported no robot actions for this order.</p>';
      return;
    }
    var html = '';
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      if (!g.events.length) continue;
      g.events.sort(function(a, b) { return ms(a.ev.created_at) - ms(b.ev.created_at); });
      html += '<div class="actions-group"><div class="actions-head">' + stateBadge(g.status)
        + ' <span class="text-muted tnum">' + formatClockSeconds(g.startedAt) + '</span></div>'
        + '<table class="table-compact w-full"><tbody>';
      for (var j = 0; j < g.events.length; j++) {
        var item = g.events[j];
        var ev = item.ev;
        var what = item.block ? legWhat(item)
          : stateBadge(ev.old_status, ev.old_state) + ' &rarr; ' + stateBadge(ev.new_status, ev.new_state);
        html += '<tr class="action-row">'
          + clockCell(ev.created_at)
          + '<td>' + what + '</td>'
          + '<td>' + escapeHtml(ev.robot_id || '') + '</td>'
          + '<td>' + escapeHtml(ev.robot_station || '') + '</td>'
          + '<td class="tnum">' + position(ev) + '</td>'
          + '<td class="col-num tnum">' + (ev.robot_battery != null ? Math.round(ev.robot_battery) + '%' : '') + '</td>'
          + '</tr>';
      }
      html += '</tbody></table></div>';
    }
    host.innerHTML = html;
  }

  // Notices go through relevantNotices (fleet-notices.js): only what concerns
  // this order's robot, not the fleet's roll-call of why every other robot
  // passed. Errors and warnings are about the order and are shown as sent.
  function renderMessages(telemetry, robotID) {
    if (!telemetry) return;
    var msgs = [];
    try {
      var errors = JSON.parse(telemetry.errors_json || '[]');
      var warnings = JSON.parse(telemetry.warnings_json || '[]');
      var notices = relevantNotices(JSON.parse(telemetry.notices_json || '[]'), robotID);
      for (var i = 0; i < errors.length; i++) msgs.push({type: 'error', msg: errors[i]});
      for (var j = 0; j < warnings.length; j++) msgs.push({type: 'warning', msg: warnings[j]});
      for (var k = 0; k < notices.length; k++) msgs.push({type: 'notice', msg: notices[k]});
    } catch(e) { return; }

    if (msgs.length === 0) return;

    document.getElementById('mission-messages-card').style.display = '';
    var el = document.getElementById('mission-messages');
    var html = '';
    for (var m = 0; m < msgs.length; m++) {
      var item = msgs[m];
      var badgeClass = item.type === 'error' ? 'badge-failed' : item.type === 'warning' ? 'badge-staged' : 'badge-dispatched';
      html += '<div style="margin-bottom:.5rem;padding:.5rem;border:1px solid var(--border);border-radius:4px">';
      html += '<span class="badge ' + badgeClass + '">' + item.type + '</span> ';
      html += '<strong>Code ' + item.msg.code + '</strong>: ' + escapeHtml(item.msg.desc || '');
      if (item.msg.timestamp) html += ' <span style="color:var(--text-muted);font-size:.85em">' + formatTime(new Date(item.msg.timestamp)) + '</span>';
      if (item.msg.times > 1) html += ' <span style="color:var(--text-muted)">(x' + item.msg.times + ')</span>';
      html += '</div>';
    }
    el.innerHTML = html;
  }

  function loadMission() {
    fetch('/api/missions/' + orderID).then(function(r) { return r.json(); }).then(function(data) {
      document.getElementById('mission-loading').style.display = 'none';
      document.getElementById('mission-content').style.display = '';
      var st = buildStages(data.history, serverNow());
      // Actions file under the stages; an order with no history at all still
      // gets its actions listed, under one unnamed group.
      var groups = st.spans.length ? st.spans
        : [{ status: '', startedAt: null, startMs: -Infinity, legs: [], events: [] }];
      var total = attachEvents(groups, data.events || []);
      renderSummary(data, st);
      renderStages(st);
      renderMessages(data.telemetry, (data.telemetry && data.telemetry.robot_id) || (data.order && data.order.robot_id) || '');
      renderActions(groups, total);
      installLiveDurations(document.getElementById('mission-content'));
    }).catch(function(err) {
      document.getElementById('mission-loading').textContent = 'Failed to load mission: ' + err.message;
    });
  }

  // Vendor telemetry arrives as 'mission-event'; Core's own status transitions
  // as 'order-update' (debounced so a burst of transitions coalesces). Either
  // reloads the whole mission — history, stages and actions are one read.
  onSSE('mission-event', function(data) {
    if (data && String(data.order_id) === String(orderID)) loadMission();
  });
  onSSE('order-update', debounce(function(data) {
    if (data && String(data.order_id) === String(orderID)) loadMission();
  }, 200));

  loadMission();
})();
