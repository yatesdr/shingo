// bins-row.js — how one Bins table row draws a bin, as pure functions over the
// bin's detail answer (GET /api/bins/detail?history=0, or the answer of a bin
// action). The twin of the row in templates/bins.html: refreshBinRow in
// bins.js used to write the raw _TRANSIT / _ROBOT:<id> node name into
// Location, rebuild Flags without the Return button, and leave the sort and
// search values as the page loaded them. Every value the template renders for
// a row is computed here instead, from the same facts the handler uses.
//
// No DOM: bins.row.test.js runs these under plain Node.
import { escapeHtml } from '/static/shared/utils.js';

var TRANSIT_NODE = '_TRANSIT';
var CARRIER_PREFIX = '_ROBOT:';

// uopText is a bin's count as the table and the detail modal show it: wherever
// there is one, including a payload-less bin left negative by a clear (the bin
// Inventory lists under "Count below zero"). Only no payload AND no count is not
// applicable. Twin of the bins.html UoP cell.
export function uopText(b) {
  return (b.payload_code || b.uop_remaining) ? String(b.uop_remaining) : '<span class="text-muted">-</span>';
}

// flagsHTML is the row's labelled flag chips. Twin of the bins.html flags cell
// (the notes chip and the Return button are added by binRowView's caller and
// binRowView respectively; the detail answer carries no notes flag).
export function flagsHTML(b) {
  var flags = '';
  if (b.locked) flags += '<span class="chip chip-muted" title="Locked by ' + escapeHtml(b.locked_by || '') + '">locked</span>';
  if (b.claimed_by) flags += '<span class="chip chip-muted" title="Claimed by order #' + b.claimed_by + '">order #' + b.claimed_by + '</span>';
  if (!b.manifest_confirmed && b.payload_code) flags += '<span class="chip chip-warn" title="Manifest unconfirmed">unconfirmed</span>';
  if (b.anomaly_at) flags += '<span class="chip chip-err" title="Counts refused — payload mismatch (anomaly); reconcile this bin">counts refused</span>';
  return flags;
}

// binContents is the row's contents class (dot colour and the Contents filter).
export function binContents(b) {
  return b.payload_code
    ? (b.manifest_confirmed ? (b.uop_remaining > 0 ? 'loaded' : 'depleted') : 'unconfirmed')
    : 'empty';
}

// binTransit is handleBins' binRow decoration (handlers_bins.go): a bin at
// _TRANSIT or on a robot deck is in transit; at _TRANSIT with no claim it is
// stranded; the order behind it is its claim (current_order) or, with no
// claim, its most recent order (recent_orders[0], newest first like
// transitOrderFor's ListByBin(id, 1)).
export function binTransit(detail) {
  var b = detail.bin;
  var name = b.node_name || '';
  var t = { inTransit: false, stranded: false, carriedBy: '', payload: '', source: '', dest: '', orderID: 0, orderStatus: '' };
  if (name === TRANSIT_NODE) {
    t.inTransit = true;
    t.stranded = !b.claimed_by;
  } else if (name.indexOf(CARRIER_PREFIX) === 0) {
    t.inTransit = true;
    t.carriedBy = name.slice(CARRIER_PREFIX.length);
  }
  if (!t.inTransit) return t;
  var o = b.claimed_by ? (detail.current_order || null) : ((detail.recent_orders || [])[0] || null);
  if (o) {
    t.payload = o.payload_code || '';
    t.source = o.source_node || '';
    t.dest = o.delivery_node || '';
    t.orderID = o.id || 0;
    t.orderStatus = o.status || '';
  }
  return t;
}

// binRowView is everything bins.html renders for one row, from a detail
// answer: the row's data-* values (filter, search, cycle count), each cell's
// HTML and each sortable cell's data-sort-value. `auth` is PAGE_AUTH: the
// Return button is drawn only for a signed-in viewer, as the template does.
export function binRowView(detail, auth) {
  var b = detail.bin;
  var t = binTransit(detail);
  var esc = escapeHtml;
  var contents = binContents(b);
  var node = t.inTransit ? (t.source + ' → ' + t.dest) : (b.node_name || '');
  var payload = b.payload_code || t.payload;

  var loc;
  if (t.stranded) {
    loc = t.orderID
      ? '<span title="The order this route came from is over; the bin is not moving">last order #' + t.orderID +
        ' (' + esc(t.orderStatus) + '): ' + esc(t.source) + ' &rarr; ' + esc(t.dest) + '</span>'
      : 'lost in transit';
  } else if (t.inTransit) {
    loc = '<span title="In transit">' + (t.source ? esc(t.source) + ' &rarr; ' + esc(t.dest) : 'in transit') + '</span>';
  } else if (b.node_name) {
    loc = esc(b.node_name);
  } else {
    loc = '<span class="text-muted">-</span>';
  }
  if (t.carriedBy) {
    loc += '<div class="text-muted-xs">on ' + esc(t.carriedBy) + '</div>';
    if (b.anomaly_note) loc += '<div class="text-muted-xs bin-loc-note" title="' + esc(b.anomaly_note) + '">' + esc(b.anomaly_note) + '</div>';
  }
  if (t.stranded) loc += '<div class="text-muted-xs">stranded' + (b.anomaly_note ? ' &mdash; ' + esc(b.anomaly_note) : '') + '</div>';

  var payloadHTML;
  if (b.payload_code) {
    payloadHTML = '<code>' + esc(b.payload_code) + '</code>';
  } else if (t.payload) {
    payloadHTML = '<code>' + esc(t.payload) + '</code> <span class="text-muted" style="font-size:0.85em">' +
      (t.stranded ? '(last order)' : '(in transit)') + '</span>';
  } else {
    payloadHTML = '<span class="text-muted">-</span>';
  }

  var returnHTML = '';
  if (t.carriedBy && auth) {
    returnHTML = '<span data-action="stopPropagation"><button class="btn btn-sm" data-action="askRobotToSetDown:' + b.id + ':' + esc(t.carriedBy) + '"' +
      ' title="Return this bin to where it is used from">Return</button></span>';
  }

  return {
    data: {
      label: b.label || '',
      type: b.bin_type_code || '',
      status: b.status || '',
      node: node,
      payload: payload,
      uop: String(b.uop_remaining || 0),
      uopCapacity: String(b.uop_capacity || 0),
      locked: b.locked ? '1' : '0',
      claimed: b.claimed_by ? '1' : '0',
      confirmed: b.manifest_confirmed ? '1' : '0',
      contents: contents
    },
    label: { html: '<span class="bin-dot bin-dot-' + contents + '"></span><strong><code>' + esc(b.label) + '</code></strong>', sort: b.label || '' },
    type: { text: b.bin_type_code || '' },
    location: { html: loc, sort: node },
    payload: { html: payloadHTML, sort: payload },
    uop: { html: uopText(b), sort: String(b.uop_remaining) },
    status: { html: '<span class="badge badge-' + esc(b.status) + '">' + esc(b.status) + '</span>', sort: b.status || '' },
    flags: { html: flagsHTML(b), returnHTML: returnHTML }
  };
}
