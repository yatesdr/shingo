import { confirm, delegateActions, toast } from '/static/js/shingoedge.js';

// replenishment.js — cell-autoreorder admin page.
//
// Two inline edit verbs, both PUT /api/replenishment/cell-reorder:
//   replenishmentApplyCell   — set reorder_point + auto_reorder
//   replenishmentRevertCell  — zero it and mark source=legacy
//
// Page reloads after a successful write so the source badge reflects.
//
// SCOPE NOTE — this file used to carry a loader-threshold half: apply/delete
// on loader_payload_thresholds, plus a seven-input calculator modal and a
// recalculate-all sweep. It was deleted 2026-07-21 along with its server side.
// Core owns the loader UOP threshold (Nodes -> loader config -> demand_registry
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
      source: 'manual',
      auto_reorder: autoOn,
    }),
  });
  if (!r.ok) {
    toast('Failed: ' + await r.text(), 'error');
    return;
  }
  window.location.reload();
}

// Revert a single claim to legacy: zero the reorder_point, disable
// autoreorder, mark source=legacy.
async function replenishmentRevertCell(btn) {
  const row = btn.closest('tr');
  const claimID = parseInt(row.dataset.claimId, 10);
  if (!await confirm('Revert this claim to legacy? reorder_point becomes 0 and autoreorder is disabled.')) {
    return;
  }
  const r = await fetch('/api/replenishment/cell-reorder', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      claim_id: claimID,
      reorder_point: 0,
      source: 'legacy',
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
    replenishmentRevertCell
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
