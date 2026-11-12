import { delegateActions } from '/static/shared/dom.js';
import { confirm } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';

// replenishment.js — cell-autoreorder admin page.
//
// Two inline edit verbs, both PUT /api/replenishment/cell-reorder:
//   replenishmentApplyCell  — set reorder_point + auto_reorder
//   replenishmentClearCell  — reorder_point 0, auto_reorder off
//
// Page reloads after a successful write so the row reads what was saved.
//
// SCOPE NOTE — this file used to carry a loader-threshold half: apply/delete
// on loader_payload_thresholds, plus a seven-input calculator modal and a
// recalculate-all sweep. It was deleted 2026-07-21 along with its server side.
// Core owns the loader UoP threshold (Nodes -> loader config -> demand_registry
// -> the threshold monitor); the Edge write path terminated in SendClaimSync(),
// a no-op stub retired when Core took ownership of the loader aggregate. Every
// one of those controls accepted input, reported success, and changed nothing.

async function replenishmentApplyCell(btn) {
  const row = btn.closest('tr');
  const claimID    = parseInt(row.dataset.claimId, 10);
  const value      = parseInt(row.querySelector('.reorder-point-input').value, 10) || 0;
  const autoOn     = row.querySelector('.auto-reorder-input').checked;
  const r = await fetch('/api/replenishment/cell-reorder', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      claim_id: claimID,
      reorder_point: value,
      auto_reorder: autoOn,
    }),
  });
  if (!r.ok) {
    toast('Failed: ' + await r.text(), 'error');
    return;
  }
  window.location.reload();
}

// Clear a single claim: reorder_point 0, Auto reorder off.
async function replenishmentClearCell(btn) {
  const row = btn.closest('tr');
  const claimID = parseInt(row.dataset.claimId, 10);
  if (!await confirm('Clear this reorder point? It becomes 0 and Auto reorder turns off.')) {
    return;
  }
  const r = await fetch('/api/replenishment/cell-reorder', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      claim_id: claimID,
      reorder_point: 0,
      auto_reorder: false,
    }),
  });
  if (!r.ok) {
    toast('Failed: ' + await r.text(), 'error');
    return;
  }
  window.location.reload();
}

// ─── delegated event handlers ─────────────────────────
// All page-level data-action verbs route through delegateActions
// on document.body.
delegateActions(document.body, {
    replenishmentApplyCell,
    replenishmentClearCell
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
