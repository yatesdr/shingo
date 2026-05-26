import { api, toast } from '/static/js/shingoedge.js';

// Counter-anomaly bell + popover handlers. Bound to markup defined in
// templates/header.html. api / toast are provided
// by shingoedge.js, which loads earlier in the page.
function toggleAnomalyPopover() {
  var pop = document.getElementById('anomaly-popover');
  if (pop) pop.style.display = pop.style.display === 'none' ? 'block' : 'none';
}

// Close popover on outside click
document.addEventListener('click', function(e) {
  var wrap = document.querySelector('.anomaly-bell-wrap');
  var pop = document.getElementById('anomaly-popover');
  if (wrap && pop && !wrap.contains(e.target)) {
    pop.style.display = 'none';
  }
});

async function confirmAnomaly(id) {
  try {
    await api.post('/api/confirm-anomaly/' + id, {});
    var el = document.getElementById('anomaly-' + id);
    if (el) el.remove();
    updateAnomalyBadge();
    toast('Anomaly confirmed', 'success');
  } catch (e) { toast('Error: ' + e, 'error'); }
}

async function dismissAnomaly(id) {
  try {
    await api.post('/api/dismiss-anomaly/' + id, {});
    var el = document.getElementById('anomaly-' + id);
    if (el) el.remove();
    updateAnomalyBadge();
    toast('Anomaly dismissed', 'success');
  } catch (e) { toast('Error: ' + e, 'error'); }
}

function updateAnomalyBadge() {
  var items = document.querySelectorAll('.anomaly-item');
  var badge = document.getElementById('anomaly-count');
  if (badge) badge.textContent = items.length;
  if (items.length === 0) {
    var pop = document.getElementById('anomaly-popover');
    if (pop) pop.style.display = 'none';
  }
}

// The anomaly bell lives in the navbar (header.html), included by
// every page. Each per-page module registers its own delegateActions
// map on document.body — that registration's sentinel blocks any
// later document.body registration, so we can't use delegateActions
// here without erasing the page-script's handler map. Use a direct
// delegated click listener scoped to the anomaly wrap instead.
document.addEventListener('click', function(e) {
  if (!e.target || !e.target.closest) return;
  var act = e.target.closest('[data-action]');
  if (!act) return;
  var raw = act.dataset.action || '';
  var parts = raw.split(':');
  if (parts[0] === 'toggleAnomalyPopover') {
    toggleAnomalyPopover();
  } else if (parts[0] === 'confirmAnomaly') {
    confirmAnomaly(parts[1]);
  } else if (parts[0] === 'dismissAnomaly') {
    dismissAnomaly(parts[1]);
  }
});
