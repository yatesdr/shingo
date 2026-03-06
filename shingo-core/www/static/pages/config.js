var kafkaBrokerIdx = parseInt(document.getElementById('page-data').dataset.brokerCount) || 0;

function addKafkaBroker() {
  var row = document.createElement('div');
  row.className = 'flex gap-1 mb-1 kafka-broker-row';
  row.innerHTML = '<input type="text" name="kafka_host_' + kafkaBrokerIdx + '" placeholder="Host" style="flex:2">' +
    '<input type="number" name="kafka_port_' + kafkaBrokerIdx + '" placeholder="9093" value="9093" style="flex:1">' +
    '<button type="button" class="btn btn-danger btn-sm" onclick="removeKafkaBroker(this)">-</button>';
  document.getElementById('kafka-broker-rows').appendChild(row);
  kafkaBrokerIdx++;
}

function removeKafkaBroker(btn) {
  btn.parentElement.remove();
}

function toggleDbFields() {
  var driver = document.getElementById('db-driver').value;
  document.getElementById('db-sqlite-fields').style.display = driver === 'sqlite' ? '' : 'none';
  document.getElementById('db-postgres-fields').style.display = driver === 'postgres' ? '' : 'none';
}
toggleDbFields();
