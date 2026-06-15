import { apiGet, apiPost, escapeHtml } from '/static/app.js';

// Inventory-page tuning surface for Core-owned loaders: per-payload UOP threshold
// + the lead-time Calc (the loader modal on Nodes is structure-only). A threshold
// change posts to the LoaderService, which re-derives demand_registry + pushes to
// the Edge on the next node-list sync. Calc suggests a threshold from observed
// lead times (ported ThresholdCalculatorService) and fills it in.

let nodesById = {};

async function load() {
  try {
    const nd = await apiGet('/api/nodes');
    const nodes = (nd && (nd.nodes || nd.data || nd)) || [];
    nodesById = {};
    (Array.isArray(nodes) ? nodes : []).forEach(function (n) {
      const id = n.id != null ? n.id : n.ID;
      if (id != null) nodesById[id] = n.name != null ? n.name : n.Name;
    });
  } catch (e) { /* node names cosmetic */ }
  try { const ld = await apiGet('/api/loader/list'); render((ld && ld.loaders) || []); } catch (e) { /* leave Loading… */ }
}

function render(items) {
  const body = document.getElementById('inv-loaders-body');
  if (!body) return;
  const rows = [];
  items.forEach(function (item) {
    const l = item.loader;
    (item.payloads || []).forEach(function (p) { rows.push(rowHtml(l, p.payload_code, l.core_node_name, p.uop_threshold || 0, 'payload', p.min_stock || 0)); });
    (item.homes || []).forEach(function (hm) {
      const nm = nodesById[hm.position_node_id] || ('node#' + hm.position_node_id);
      rows.push(rowHtml(l, hm.payload_code, nm, hm.uop_threshold || 0, 'home', 0));
    });
  });
  body.innerHTML = rows.length ? rows.join('') : '<tr><td colspan="6" class="dash-empty">No Core-owned loaders.</td></tr>';
  body.querySelectorAll('.inv-ldr-thr').forEach(function (inp) { inp.addEventListener('change', function () { save(inp, Number(inp.value) || 0); }); });
  body.querySelectorAll('.inv-ldr-calc').forEach(function (btn) {
    btn.addEventListener('click', function () {
      const cyc = body.querySelector('.inv-ldr-cycle[data-key="' + btn.dataset.key + '"]').value;
      if (!cyc || Number(cyc) <= 0) { alert('Enter cycle seconds first (the box left of Calc)'); return; }
      apiPost('/api/loader/calculate', { core_node_name: btn.dataset.anchor, payload_code: btn.dataset.pc, cycle_seconds: Number(cyc), days: 14 })
        .then(function (d) {
          if (d && d.error) { alert(d.error); return; }
          const l1 = d && d.outputs ? d.outputs.L1Threshold : 0;
          const inp = body.querySelector('.inv-ldr-thr[data-key="' + btn.dataset.key + '"]');
          if (inp) { inp.value = l1; save(inp, l1); }
          alert('Suggested UOP threshold ' + l1 + ' (confidence ' + (d ? d.confidence : '') + ')');
        }).catch(function (e) { alert('' + e); });
    });
  });
}

function rowHtml(l, pc, node, thr, kind, ms) {
  const key = l.id + '|' + kind + '|' + node + '|' + pc;
  return '<tr><td>' + escapeHtml(l.name || '') + '</td><td>' + escapeHtml(node) + '</td><td>' + escapeHtml(l.role) + '</td><td>' + escapeHtml(pc) + '</td>'
    + '<td><input type="number" placeholder="cycle" class="inv-ldr-cycle" data-key="' + escapeHtml(key) + '" style="width:60px"> '
    + '<button class="btn btn-sm inv-ldr-calc" data-key="' + escapeHtml(key) + '" data-anchor="' + escapeHtml(l.core_node_name) + '" data-pc="' + escapeHtml(pc) + '">Calc</button></td>'
    + '<td><input type="number" value="' + thr + '" class="inv-ldr-thr" data-key="' + escapeHtml(key) + '" data-lid="' + l.id + '" data-pc="' + escapeHtml(pc) + '" data-kind="' + kind + '" data-node="' + escapeHtml(node) + '" data-ms="' + (ms || 0) + '" style="width:80px"></td></tr>';
}

function save(inp, thr) {
  if (inp.dataset.kind === 'home') {
    let nid = null;
    for (const id in nodesById) { if (nodesById[id] === inp.dataset.node) { nid = Number(id); break; } }
    if (nid == null) return;
    apiPost('/api/loader/set-home', { loader_id: Number(inp.dataset.lid), position_node_id: nid, payload_code: inp.dataset.pc, uop_threshold: thr }).then(load).catch(function () {});
  } else {
    apiPost('/api/loader/set-payload', { loader_id: Number(inp.dataset.lid), payload_code: inp.dataset.pc, uop_threshold: thr, min_stock: Number(inp.dataset.ms) || 0 }).then(load).catch(function () {});
  }
}

load();
