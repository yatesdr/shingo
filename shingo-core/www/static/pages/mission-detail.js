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
    return '<span class="badge badge-' + label + '">' + label + '</span>';
  }

  var stateColors = {
    'CREATED': 'var(--text-muted)',
    'TOBEDISPATCHED': 'var(--info)',
    'RUNNING': 'var(--primary)',
    'WAITING': 'var(--warning)',
    'FINISHED': 'var(--success)',
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
      renderDurationBar(data.events || []);
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
    html += '<div title="Shingo order ID"><strong>Order ID</strong><br><a href="/orders/detail?id=' + o.id + '">' + o.id + '</a></div>';
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

  function renderDurationBar(events) {
    var bar = document.getElementById('duration-bar');
    var legend = document.getElementById('duration-legend');
    if (events.length < 2) {
      bar.innerHTML = '<span style="color:var(--text-muted)">Not enough data for duration breakdown</span>';
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
      html += '<div>' + stateBadge(ev.old_state) + ' &rarr; ' + stateBadge(ev.new_state) + '</div>';
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
      html += '<tr>';
      html += '<td style="white-space:nowrap">' + formatTime(ev.created_at) + '</td>';
      html += '<td>' + stateBadge(ev.old_state) + ' &rarr; ' + stateBadge(ev.new_state) + '</td>';
      html += '<td>' + (ev.robot_id || '-') + '</td>';
      html += '<td>' + (ev.robot_station || '-') + '</td>';
      html += '<td>' + (pos || '-') + '</td>';
      html += '<td>' + (ev.robot_battery != null ? Math.round(ev.robot_battery) + '%' : '-') + '</td>';
      html += '</tr>';
    }
    tbody.innerHTML = html;
  }

  // SSE live updates for active missions
  window.onMissionEvent = function(e) {
    try {
      var data = JSON.parse(e.data);
      if (String(data.order_id) === String(orderID)) {
        loadMission(); // Reload full data on any event for this mission
      }
    } catch(err) { console.error('onMissionEvent', err); }
  };

  loadMission();
})();
