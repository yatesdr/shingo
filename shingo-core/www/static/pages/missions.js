(function() {
  var currentOffset = 0;
  var currentLimit = 50;
  var activeState = '';

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

  function stateLabel(state) {
    if (!state) return '-';
    var map = {
      'FINISHED': 'completed', 'delivered': 'completed', 'confirmed': 'completed',
      'FAILED': 'failed', 'failed': 'failed',
      'STOPPED': 'cancelled', 'cancelled': 'cancelled'
    };
    return map[state] || state;
  }

  function stateBadgeClass(state) {
    var label = stateLabel(state);
    return 'badge-' + label;
  }

  function timeAgo(ts) {
    if (!ts) return '-';
    var d = new Date(ts);
    var now = new Date();
    var diff = Math.floor((now - d) / 1000);
    if (diff < 60) return diff + 's ago';
    if (diff < 3600) return Math.floor(diff/60) + 'm ago';
    if (diff < 86400) return Math.floor(diff/3600) + 'h ago';
    return Math.floor(diff/86400) + 'd ago';
  }

  function formatAbsTime(ts) {
    if (!ts) return '';
    return new Date(ts).toLocaleString();
  }

  function buildQuery() {
    var params = new URLSearchParams();
    var since = document.getElementById('filter-since').value;
    var until = document.getElementById('filter-until').value;
    var station = document.getElementById('filter-station').value.trim();
    var robot = document.getElementById('filter-robot').value.trim();
    if (since) params.set('since', since);
    if (until) params.set('until', until);
    if (station) params.set('station_id', station);
    if (robot) params.set('robot_id', robot);
    if (activeState) params.set('state', activeState);
    params.set('limit', currentLimit);
    params.set('offset', currentOffset);
    return params.toString();
  }

  function loadMissions() {
    var q = buildQuery();
    fetch('/api/missions?' + q).then(function(r) { return r.json(); }).then(function(data) {
      var tbody = document.getElementById('mission-list');
      tbody.innerHTML = '';
      var missions = data.missions || [];
      for (var i = 0; i < missions.length; i++) {
        var m = missions[i];
        var tr = document.createElement('tr');
        tr.className = 'mission-row';
        tr.setAttribute('data-order-id', m.order_id);
        tr.title = 'Click to view mission details for order ' + m.order_id;
        tr.onclick = (function(id) { return function() { window.location.href = '/missions/' + id; }; })(m.order_id);
        tr.innerHTML =
          '<td>' + m.order_id + '</td>' +
          '<td>' + (m.robot_id || '-') + '</td>' +
          '<td>' + (m.station_id || '-') + '</td>' +
          '<td>' + (m.source_node || '?') + ' &rarr; ' + (m.delivery_node || '?') + '</td>' +
          '<td><span class="badge ' + stateBadgeClass(m.terminal_state) + '">' + stateLabel(m.terminal_state) + '</span></td>' +
          '<td title="' + (m.duration_ms ? m.duration_ms + 'ms' : '') + '">' + formatDuration(m.duration_ms) + '</td>' +
          '<td title="' + formatAbsTime(m.core_completed) + '">' + timeAgo(m.core_completed) + '</td>';
        tbody.appendChild(tr);
      }
      if (missions.length === 0) {
        tbody.innerHTML = '<tr><td colspan="7" style="text-align:center;color:var(--text-muted)">No missions found</td></tr>';
      }
      renderPagination(data.total, data.offset, data.limit);
    });
  }

  function loadStats() {
    var q = buildQuery();
    fetch('/api/missions/stats?' + q).then(function(r) { return r.json(); }).then(function(s) {
      document.getElementById('stat-total').textContent = s.total_missions || 0;
      document.getElementById('stat-avg').textContent = formatDuration(s.avg_duration_ms);
      document.getElementById('stat-p95').textContent = formatDuration(s.p95_duration_ms);
      document.getElementById('stat-rate').textContent = s.success_rate ? s.success_rate.toFixed(1) + '%' : '-';
    });
  }

  function renderPagination(total, offset, limit) {
    var el = document.getElementById('pagination');
    if (total <= limit) { el.innerHTML = ''; return; }
    var page = Math.floor(offset / limit) + 1;
    var pages = Math.ceil(total / limit);
    var html = '<span style="color:var(--text-muted)">' + total + ' total</span>';
    if (page > 1) html += ' <button class="btn btn-sm" onclick="window._missionPage(' + (offset - limit) + ')">Prev</button>';
    html += ' <span>Page ' + page + '/' + pages + '</span>';
    if (page < pages) html += ' <button class="btn btn-sm" onclick="window._missionPage(' + (offset + limit) + ')">Next</button>';
    el.innerHTML = html;
  }

  window._missionPage = function(offset) {
    currentOffset = offset;
    loadMissions();
  };

  function applyFilters() {
    currentOffset = 0;
    loadMissions();
    loadStats();
  }

  // State filter buttons
  document.querySelectorAll('.state-btn').forEach(function(btn) {
    btn.addEventListener('click', function() {
      document.querySelectorAll('.state-btn').forEach(function(b) { b.classList.remove('active'); });
      btn.classList.add('active');
      activeState = btn.getAttribute('data-state');
      applyFilters();
    });
  });

  document.getElementById('btn-apply').addEventListener('click', applyFilters);

  // Enter key triggers filter apply on text/date inputs
  ['filter-since', 'filter-until', 'filter-station', 'filter-robot'].forEach(function(id) {
    document.getElementById(id).addEventListener('keydown', function(e) {
      if (e.key === 'Enter') applyFilters();
    });
  });

  // Initial load
  loadMissions();
  loadStats();
})();
