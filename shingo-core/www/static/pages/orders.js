function orderControlPost(url, body) {
  var msg = document.getElementById('order-status-msg');
  if (msg) msg.textContent = 'Sending...';
  fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function(r) { return r.json().then(function(d) { return {ok:r.ok, data:d}; }); })
    .then(function(r) {
      if (msg) msg.textContent = r.ok ? 'OK - reloading...' : (r.data.error || 'Error');
      if (r.ok) setTimeout(function() { location.reload(); }, 800);
    })
    .catch(function(e) {
      if (msg) msg.textContent = 'Network error';
    });
}

function terminateOrder(id) {
  if (!confirm('Terminate order #' + id + '? This cannot be undone.')) return;
  orderControlPost('/api/orders/terminate', {order_id: id});
}

function setOrderPriority(id) {
  var p = parseInt(document.getElementById('order-priority').value, 10);
  if (isNaN(p)) return;
  orderControlPost('/api/orders/priority', {order_id: id, priority: p});
}
