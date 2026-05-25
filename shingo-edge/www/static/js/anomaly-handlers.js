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
