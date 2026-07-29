import { api, debounce, el, h } from '/static/app.js';
import { onSSE } from '/static/shared/utils.js';

(function() {
  var orderID = document.getElementById('mission-order-id').textContent;

  function formatDuration(ms) {
    if (!ms || ms <= 0) return '-';
    if (ms < 1000) return ms + 'ms';
    var s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    s = s % 60;
    if (m < 60) return m + 'm ' + s + 's';
    var h = Math.floor(m / 60);
    m = m % 60;
    return h + 'h ' + m + 'm';
  }

  function formatTime(ts) {
    if (!ts) return '-';
    var d = new Date(ts);
    return d.toLocaleString();
  }

  function stateLabel(state) {
    if (!state) return '-';
    var map = {
      'FINISHED': 'completed', 'delivered': 'completed', 'confirmed': 'completed',
      'FAILED': 'failed', 'failed': 'failed',
      'STOPPED': 'cancelled', 'cancelled': 'cancelled',
      'CREATED': 'created', 'TOBEDISPATCHED': 'dispatched',
      'RUNNING': 'in_transit', 'WAITING': 'staged'
    };
    return map[state] || state;
  }

  function stateBadge(state) {
    var label = stateLabel(state);
    // 'completed' / 'created' are display labels, not protocol statuses —
    // map them onto real badge-<status> classes so they pick up the Signal
    // palette (green / slate) instead of the unstyled grey fallback.
    var classMap = { completed: 'badge-confirmed', created: 'badge-pending' };
    var cls = classMap[label] || ('badge-' + label);
    return '<span class="badge ' + cls + '">' + label + '</span>';
  }

  // Duration segments and timeline dots take the hue of the status they are
  // LABELLED with (see stateLabel), not a hue of their own. This table used
  // to be independent and disagreed with the badges rendered beside it:
  // RUNNING is labelled "in_transit" but was painted with the dispatched
  // blue, TOBEDISPATCHED is labelled "dispatched" but was painted --info
  // cyan — the two hues were swapped relative to their own badges — and
  // WAITING ("staged", a benign dwell) was painted --warning amber. Same
  // "one palette, three renderers" defect P13 fixed for the map's
  // STATUS_COLOR; keyed off the shared --status-*-dot tokens now, so the
  // segment, the dot and the badge can't drift apart again.
  var stateColors = {
    'CREATED': 'var(--status-pending-dot)',
    'TOBEDISPATCHED': 'var(--status-dispatched-dot)',
    'RUNNING': 'var(--status-in-transit-dot)',
    'WAITING': 'var(--status-staged-dot)',
    'FINISHED': 'var(--status-delivered-dot)',
    'FAILED': 'var(--danger)',
    'STOPPED': 'var(--text-muted)'
  };

  function formatRoute(order) {
    // If steps_json is available, show each node in the route
    if (order.steps_json) {
      try {
        var steps = JSON.parse(order.steps_json);
        if (steps.length > 0) {
          var nodes = [];
          for (var i = 0; i < steps.length; i++) {
            if (steps[i].node) {
              var label = steps[i].node;
              if (steps[i].action === 'wait') label += ' <span style="font-size:.75em;color:var(--text-muted)">(wait)</span>';
              nodes.push(label);
            }
          }
          if (nodes.length > 0) return nodes.join(' &rarr; ');
        }
      } catch(e) { console.error('orderRoute steps parse', e); }
    }
    // Fallback: source → delivery
    return (order.source_node || '?') + ' &rarr; ' + (order.delivery_node || '?');
  }

  function loadMission() {
    fetch('/api/missions/' + orderID).then(function(r) { return r.json(); }).then(function(data) {
      document.getElementById('mission-loading').style.display = 'none';
      document.getElementById('mission-content').style.display = '';
      renderSummary(data);
      renderDurationBar(data.events || [], data.telemetry);
      renderTimeline(data.events || []);
      renderMessages(data.telemetry);
      renderEventLog(data.events || []);
    }).catch(function(err) {
      document.getElementById('mission-loading').textContent = 'Failed to load mission: ' + err.message;
    });
  }

  function renderSummary(data) {
    var o = data.order || {};
    var t = data.telemetry || {};
    var el = document.getElementById('mission-summary');

    var html = '<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:1rem">';
    html += '<div title="Shingo order ID"><strong>Order ID</strong><br><a href="/orders?open=' + o.id + '">' + o.id + '</a></div>';
    html += '<div title="Transport order type (retrieve, store, move, etc.)"><strong>Type</strong><br>' + (o.order_type || '-') + '</div>';
    html += '<div title="Edge station that requested this order"><strong>Station</strong><br>' + (o.station_id || '-') + '</div>';
    html += '<div title="Robot vehicle ID assigned by the fleet"><strong>Robot</strong><br>' + (t.robot_id || o.robot_id || '-') + '</div>';
    html += '<div title="Source node to delivery node"><strong>Route</strong><br>' + formatRoute(o) + '</div>';
    html += '<div title="Current order status in Shingo"><strong>Status</strong><br>' + stateBadge(o.status) + '</div>';
    html += '<div title="Total time from order creation in Shingo to completion"><strong>Total Duration</strong><br>' + formatDuration(t.duration_ms) + '</div>';
    html += '<div title="Time measured by the fleet backend (RDS create to terminal)"><strong>Fleet Duration</strong><br>' + formatDuration(t.vendor_duration_ms) + '</div>';
    html += '</div>';

    html += '<div style="margin-top:1rem;display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:1rem;font-size:.85em;color:var(--text-muted)">';
    html += '<div title="When Shingo created this order"><strong>Core Created</strong><br>' + formatTime(t.core_created) + '</div>';
    html += '<div title="When Shingo recorded the terminal state"><strong>Core Completed</strong><br>' + formatTime(t.core_completed) + '</div>';
    html += '<div title="When the fleet backend (RDS) created the transport order"><strong>Vendor Created</strong><br>' + formatTime(t.vendor_created) + '</div>';
    html += '<div title="When the fleet backend (RDS) reported the terminal state"><strong>Vendor Completed</strong><br>' + formatTime(t.vendor_completed) + '</div>';
    html += '</div>';

    el.innerHTML = html;
  }

  // ── Leg decomposition ─────────────────────────────────────────────────
  //
  // The bar used to be one giant in_transit block, because a whole retrieve is
  // a single VENDOR state — which answered "it took 9 minutes" and nothing
  // about where the 9 minutes went. That was the question that started this
  // whole exercise.
  //
  // The fleet reports per-block startTime/terminateTime (epoch SECONDS) and
  // Core now stores them on BLOCK_FINISHED rows in mission_events. A block is
  // the robot DOING something at a location; the gap between two blocks is it
  // travelling between them. That gives travel-to-source, load, travel-to-dest,
  // unload without inventing anything.
  //
  // Two rules that matter more than the drawing:
  //   - A leg with no vendor time is UNKNOWN, never zero-length. Absent is not
  //     instant, and a zero-width segment would read as "took no time".
  //   - Legs sum to the mission duration or the difference is shown as
  //     unaccounted. Rescaling to fit would be a lie that looks tidy.
  var BLOCK_LEG_STATE = 'BLOCK_FINISHED';

  function isBlockLegEvent(ev) {
    return ev && ev.new_state === BLOCK_LEG_STATE;
  }

  // parseBlockLegs pulls the stored block records out of the BLOCK_FINISHED
  // events, oldest first. Returns [] when the fleet never reported any (older
  // missions, or a backend that does not send blocks) — the caller falls back
  // to the state-diff bar.
  function parseBlockLegs(events) {
    var out = [];
    for (var i = 0; i < events.length; i++) {
      if (!isBlockLegEvent(events[i])) continue;
      try {
        var blocks = JSON.parse(events[i].blocks_json || '[]');
        for (var b = 0; b < blocks.length; b++) {
          out.push(blocks[b]);
        }
      } catch (e) { console.error('parseBlockLegs', e); }
    }
    return out;
  }

  // hasVendorTimes reports whether BOTH endpoints were reported. Checking the
  // timestamps rather than durationSeconds is deliberate: Core writes 0 for
  // "not reported" AND for "inverted", so a duration of 0 cannot tell an
  // unknown leg from a genuinely instant one.
  function hasVendorTimes(blk) {
    return blk && blk.startTime > 0 && blk.terminateTime >= blk.startTime;
  }

  // formatDuration renders 0 as "-", which is right for a summary field that
  // may be absent and wrong for a leg: a block whose start and terminate are
  // the same second genuinely took under a second, and "-" reads as "not
  // reported". Absent is `unknown` (handled separately); zero is `0s`.
  function formatLegDuration(ms) {
    if (ms <= 0) return '0s';
    return formatDuration(ms);
  }

  function legLabel(blk) {
    var task = (blk.binTask || '').toLowerCase();
    var verb = 'handle';
    if (task.indexOf('wait') >= 0) verb = 'wait';
    else if (task.indexOf('unload') >= 0 || task.indexOf('drop') >= 0 || task.indexOf('release') >= 0) verb = 'unload';
    else if (task.indexOf('load') >= 0 || task.indexOf('pick') >= 0) verb = 'load';
    return verb + (blk.location ? ' @ ' + blk.location : '');
  }

  // buildLegs turns the block records into the segment list the bar draws.
  // Hues come from the existing per-phase status set — travel is the robot
  // moving (in_transit), a block is the robot stopped at a node doing work
  // (staged). Absence gets no hue at all, because "we do not know" is not a
  // phase.
  function buildLegs(blocks, totalMs) {
    var legs = [];
    var knownMs = 0;

    for (var i = 0; i < blocks.length; i++) {
      var blk = blocks[i];

      // Travel INTO this block: the gap since the previous block ended. Both
      // endpoints are vendor times, so this arithmetic never crosses clocks.
      if (i > 0 && hasVendorTimes(blocks[i - 1]) && hasVendorTimes(blk)) {
        var gapMs = (blk.startTime - blocks[i - 1].terminateTime) * 1000;
        if (gapMs > 0) {
          legs.push({ label: 'travel → ' + (blk.location || '?'), ms: gapMs, color: 'var(--status-in-transit-dot)' });
          knownMs += gapMs;
        }
      }

      if (hasVendorTimes(blk)) {
        var ms = (blk.terminateTime - blk.startTime) * 1000;
        legs.push({ label: legLabel(blk), ms: ms, color: 'var(--status-staged-dot)' });
        knownMs += ms;
      } else {
        legs.push({ label: legLabel(blk), ms: null, color: 'var(--text-muted)' });
      }
    }

    // Whatever the blocks do not account for. This is real time the mission
    // spent somewhere the fleet did not report a block for — queued before
    // dispatch, travelling to the first pickup, delivery bookkeeping after the
    // last drop. Naming it is more honest than stretching the legs to fill.
    if (totalMs > 0) {
      var remainder = totalMs - knownMs;
      if (remainder > 0) {
        legs.push({ label: 'unaccounted', ms: remainder, color: 'var(--text-muted)', faint: true });
      }
    }
    return legs;
  }

  function renderLegBar(bar, legend, legs, totalMs) {
    var html = '';
    var legendHtml = '';
    var sumMs = 0;

    for (var i = 0; i < legs.length; i++) {
      var leg = legs[i];
      var swatch, seg;

      if (leg.ms === null) {
        // Fixed width, hatched: it occupies space so it is visible and
        // countable, but claims no share of the timeline it cannot measure.
        seg = '<div class="duration-segment leg-unknown" style="flex:0 0 52px"'
            + ' title="' + leg.label + ': duration not reported by the fleet"></div>';
        swatch = '<span class="leg-swatch leg-unknown"></span>';
        legendHtml += '<span>' + swatch + leg.label + ': <span class="leg-unknown-text">unknown</span></span>';
      } else {
        sumMs += leg.ms;
        var pct = totalMs > 0 ? Math.max((leg.ms / totalMs) * 100, 1) : 1;
        seg = '<div class="duration-segment" style="flex:' + pct + ';background:' + leg.color
            + (leg.faint ? ';opacity:.35' : '') + '"'
            + ' title="' + leg.label + ': ' + formatLegDuration(leg.ms) + '"></div>';
        swatch = '<span class="leg-swatch" style="background:' + leg.color + (leg.faint ? ';opacity:.35' : '') + '"></span>';
        legendHtml += '<span>' + swatch + leg.label + ': ' + formatLegDuration(leg.ms) + '</span>';
      }
      html += seg;
    }

    bar.innerHTML = html;
    // State the arithmetic so the bar can be checked rather than trusted.
    var unknownCount = legs.filter(function(l) { return l.ms === null; }).length;
    var footer = '<span class="leg-total">legs ' + formatDuration(sumMs)
      + ' of ' + formatDuration(totalMs) + ' total';
    if (unknownCount > 0) footer += ' · ' + unknownCount + ' leg(s) unknown';
    footer += '</span>';
    legend.innerHTML = legendHtml + footer;
  }

  function renderDurationBar(events, telemetry) {
    var bar = document.getElementById('duration-bar');
    var legend = document.getElementById('duration-legend');

    // Prefer the real legs when the fleet reported any.
    var blocks = parseBlockLegs(events);
    if (blocks.length > 0) {
      var totalMs = (telemetry && telemetry.duration_ms) || 0;
      renderLegBar(bar, legend, buildLegs(blocks, totalMs), totalMs);
      return;
    }

    // Fallback: the old state-diff bar, for missions predating block capture.
    events = events.filter(function(ev) { return !isBlockLegEvent(ev); });
    if (events.length < 2) {
      bar.innerHTML = '<span style="color:var(--text-muted)">Not enough data for duration breakdown</span>';
      legend.innerHTML = '';
      return;
    }

    var segments = [];
    var totalMs = 0;
    for (var i = 1; i < events.length; i++) {
      var prev = new Date(events[i-1].created_at);
      var curr = new Date(events[i].created_at);
      var ms = curr - prev;
      if (ms < 0) ms = 0;
      totalMs += ms;
      segments.push({ state: events[i-1].new_state, ms: ms });
    }

    if (totalMs === 0) {
      bar.innerHTML = '<span style="color:var(--text-muted)">Zero duration</span>';
      return;
    }

    var html = '';
    var legendHtml = '';
    for (var j = 0; j < segments.length; j++) {
      var seg = segments[j];
      var pct = Math.max((seg.ms / totalMs) * 100, 1);
      var color = stateColors[seg.state] || 'var(--text-muted)';
      html += '<div class="duration-segment" style="flex:' + pct + ';background:' + color + '" title="' + stateLabel(seg.state) + ': ' + formatDuration(seg.ms) + '"></div>';
      legendHtml += '<span><span style="display:inline-block;width:12px;height:12px;border-radius:2px;background:' + color + ';vertical-align:middle;margin-right:4px"></span>' + stateLabel(seg.state) + ': ' + formatDuration(seg.ms) + '</span>';
    }
    bar.innerHTML = html;
    legend.innerHTML = legendHtml;
  }

  function renderTimeline(events) {
    var el = document.getElementById('mission-timeline');
    if (events.length === 0) {
      el.innerHTML = '<span style="color:var(--text-muted)">No events recorded</span>';
      return;
    }

    var html = '';
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      var timeSincePrev = '';
      if (i > 0) {
        var prev = new Date(events[i-1].created_at);
        var curr = new Date(ev.created_at);
        var ms = curr - prev;
        timeSincePrev = '<span class="timeline-delta">+' + formatDuration(ms) + '</span>';
      }

      var posInfo = '';
      if (ev.robot_station) {
        posInfo = ev.robot_station;
      }
      if (ev.robot_x != null && ev.robot_y != null) {
        posInfo += (posInfo ? ' ' : '') + '(' + ev.robot_x.toFixed(1) + ', ' + ev.robot_y.toFixed(1) + ')';
      }

      var batteryInfo = '';
      if (ev.robot_battery != null) {
        batteryInfo = Math.round(ev.robot_battery) + '%';
      }

      html += '<div class="timeline-entry">';
      html += '<div class="timeline-dot" style="background:' + (stateColors[ev.new_state] || 'var(--text-muted)') + '"></div>';
      html += '<div class="timeline-body">';
      html += '<div class="timeline-header">';
      html += '<span class="timeline-time">' + formatTime(ev.created_at) + '</span> ';
      html += timeSincePrev;
      html += '</div>';
      // A block completion is a LEG, not a status transition. Rendering it
      // through stateBadge would print "→ BLOCK_FINISHED" against an unstyled
      // pill, which is both ugly and wrong — old_state is empty on these rows
      // because nothing transitioned.
      if (isBlockLegEvent(ev)) {
        var legBlocks = [];
        try { legBlocks = JSON.parse(ev.blocks_json || '[]'); } catch (e) { console.error('timeline leg parse', e); }
        var b0 = legBlocks[0] || {};
        html += '<div><span class="badge badge-staged">leg</span> ' + legLabel(b0) + ' &mdash; '
          + (hasVendorTimes(b0)
              ? formatDuration((b0.terminateTime - b0.startTime) * 1000)
              : '<span class="leg-unknown-text">duration unknown</span>')
          + '</div>';
      } else {
        html += '<div>' + stateBadge(ev.old_state) + ' &rarr; ' + stateBadge(ev.new_state) + '</div>';
      }
      if (ev.robot_id) {
        html += '<div class="timeline-meta">';
        html += '<span>Robot: ' + ev.robot_id + '</span>';
        if (posInfo) html += ' <span class="robot-snapshot">@ ' + posInfo + '</span>';
        if (batteryInfo) html += ' <span>Battery: ' + batteryInfo + '</span>';
        html += '</div>';
      }

      // Show block states if available
      if (ev.blocks_json && ev.blocks_json !== '[]') {
        try {
          var blocks = JSON.parse(ev.blocks_json);
          if (blocks.length > 0) {
            html += '<div class="timeline-meta">Blocks: ';
            for (var b = 0; b < blocks.length; b++) {
              html += '<span class="badge badge-sm">' + blocks[b].location + ': ' + stateLabel(blocks[b].state) + '</span> ';
            }
            html += '</div>';
          }
        } catch(e) { console.error('renderEvent blocks parse', e); }
      }

      html += '</div></div>';
    }
    el.innerHTML = html;
  }

  function renderMessages(telemetry) {
    if (!telemetry) return;
    var msgs = [];
    try {
      var errors = JSON.parse(telemetry.errors_json || '[]');
      var warnings = JSON.parse(telemetry.warnings_json || '[]');
      var notices = JSON.parse(telemetry.notices_json || '[]');
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
      html += '<strong>Code ' + item.msg.code + '</strong>: ' + (item.msg.desc || '-');
      if (item.msg.timestamp) html += ' <span style="color:var(--text-muted);font-size:.85em">' + formatTime(new Date(item.msg.timestamp)) + '</span>';
      if (item.msg.times > 1) html += ' <span style="color:var(--text-muted)">(x' + item.msg.times + ')</span>';
      html += '</div>';
    }
    el.innerHTML = html;
  }

  function renderEventLog(events) {
    var tbody = document.getElementById('event-log');
    if (events.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" style="text-align:center;color:var(--text-muted)">No events</td></tr>';
      return;
    }

    var html = '';
    for (var i = 0; i < events.length; i++) {
      var ev = events[i];
      var pos = '';
      if (ev.robot_x != null && ev.robot_y != null) {
        pos = ev.robot_x.toFixed(1) + ', ' + ev.robot_y.toFixed(1);
      }
      var what;
      if (isBlockLegEvent(ev)) {
        var lb = [];
        try { lb = JSON.parse(ev.blocks_json || '[]'); } catch (e) { console.error('event log leg parse', e); }
        what = '<span class="badge badge-staged">leg</span> ' + legLabel(lb[0] || {});
      } else {
        what = stateBadge(ev.old_state) + ' &rarr; ' + stateBadge(ev.new_state);
      }
      html += '<tr>';
      html += '<td style="white-space:nowrap">' + formatTime(ev.created_at) + '</td>';
      html += '<td>' + what + '</td>';
      html += '<td>' + (ev.robot_id || '-') + '</td>';
      html += '<td>' + (ev.robot_station || '-') + '</td>';
      html += '<td>' + (pos || '-') + '</td>';
      html += '<td>' + (ev.robot_battery != null ? Math.round(ev.robot_battery) + '%' : '-') + '</td>';
      html += '</tr>';
    }
    tbody.innerHTML = html;
  }

  // SSE live updates for active missions — subscribed on the shared onSSE bus
  // (shared/utils.js); the handler receives the parsed payload. Replaces the
  // retired app.js IIFE window.onMissionEvent dispatch (Q-002).
  onSSE('mission-event', function(data) {
    if (data && String(data.order_id) === String(orderID)) {
      loadMission(); // Reload full data on any event for this mission
    }
  });

  // Lifecycle status transitions (sourcing→dispatched→in_transit→staged→delivered→
  // confirmed) are broadcast as 'order-update' (status_changed/dispatched/completed/…),
  // NOT 'mission-event' (telemetry only) — so the timeline went stale until a hard
  // refresh. Reload on those too, debounced so a burst of transitions coalesces.
  onSSE('order-update', debounce(function(data) {
    if (data && String(data.order_id) === String(orderID)) {
      loadMission();
    }
  }, 200));

  loadMission();
})();
