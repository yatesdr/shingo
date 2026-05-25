import { delegateActions } from '/static/app.js';

var kafkaBrokerIdx = parseInt(document.getElementById('page-data').dataset.brokerCount) || 0;

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


// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body. Multiple event types share the same handler
// map — most handlers are click-only but a few (e.g. updatePreview)
// are referenced via data-action-change / data-action-input too,
// so binding the map across every event type keeps the page wiring
// single-source.
delegateActions(document.body, {
    addKafkaBroker,
    removeKafkaBroker
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
