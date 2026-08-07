import { delegateActions } from '/static/app.js';

var kafkaBrokerIdx = parseInt(document.getElementById('page-data').dataset.brokerCount) || 0;
var notifRecipientIdx = parseInt(document.getElementById('page-data').dataset.recipientCount) || 0;

function addKafkaBroker() {
  var row = document.createElement('div');
  row.className = 'flex gap-1 mb-1 kafka-broker-row';
  row.innerHTML = '<input type="text" name="kafka_host_' + kafkaBrokerIdx + '" placeholder="Host" style="flex:2">' +
    '<input type="number" name="kafka_port_' + kafkaBrokerIdx + '" placeholder="9093" value="9093" style="flex:1">' +
    '<button type="button" class="btn btn-danger btn-sm" data-action="removeKafkaBroker">-</button>';
  document.getElementById('kafka-broker-rows').appendChild(row);
  kafkaBrokerIdx++;
}

function removeKafkaBroker(btn) {
  btn.parentElement.remove();
}

function addNotifRecipient() {
  var row = document.createElement('div');
  row.className = 'flex gap-1 mb-1 notif-recipient-row';
  row.innerHTML = '<input type="email" name="notif_recipient_' + notifRecipientIdx + '" placeholder="user@company.com" style="flex:2">' +
    '<button type="button" class="btn btn-danger btn-sm" data-action="removeNotifRecipient">Delete</button>';
  document.getElementById('notif-recipient-rows').appendChild(row);
  notifRecipientIdx++;
}

function removeNotifRecipient(btn) {
  btn.parentElement.remove();
}

async function clearNotifications() {
  var container = document.getElementById('notif-recipient-rows');
  container.innerHTML = '';
  notifRecipientIdx = 0;
  var form = container.closest('form');
  if (!form) return;
  form.querySelectorAll('[name^="notif_smtp_"], [name="notif_from_address"], [name="notif_enabled"]').forEach(function(el) {
    if (el.type === 'checkbox') el.checked = false;
    else el.value = '';
  });
}

async function testNotifAlert(btn) {
  var alertType = btn.dataset.alertType;
  var el = document.getElementById('notif-test-result');
  btn.disabled = true;
  btn.textContent = 'Sending...';
  el.style.display = 'none';
  try {
    var res = await fetch('/config/test-alert?type=' + alertType, { method: 'POST' });
    var data = await res.json();
    el.style.display = 'block';
    el.className = data.ok ? 'alert alert-ok mb-1' : 'alert alert-err mb-1';
    el.textContent = data.ok ? data.message : 'Error: ' + data.message;
  } catch (err) {
    el.style.display = 'block';
    el.className = 'alert alert-err mb-1';
    el.textContent = 'Error: ' + err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = alertType === 'fault' ? 'Test Fault Alert' : alertType === 'fail' ? 'Test Fail Alert' : alertType === 'cleared' ? 'Test Fault Cleared' : 'Test Fault Chain (sending...)';
  }
}

async function testNotifEmail() {
  var el = document.getElementById('notif-test-result');
  var btn = document.getElementById('btn-test-email');
  btn.disabled = true;
  btn.textContent = 'Sending...';
  el.style.display = 'none';
  try {
    var res = await fetch('/config/test-email', { method: 'POST' });
    var data = await res.json();
    el.style.display = 'block';
    el.className = data.ok ? 'alert alert-ok mb-1' : 'alert alert-err mb-1';
    el.textContent = data.ok ? data.message : 'Error: ' + data.message;
  } catch (err) {
    el.style.display = 'block';
    el.className = 'alert alert-err mb-1';
    el.textContent = 'Error: ' + err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Send Test Email';
  }
}


// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addKafkaBroker,
    clearNotifications,
    removeKafkaBroker,
    addNotifRecipient,
    removeNotifRecipient,
    testNotifAlert,
    testNotifEmail
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
