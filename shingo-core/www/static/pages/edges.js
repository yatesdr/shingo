// edges.js — rename an enrolled station from the Stations page.
//
// THIS IS THE ONLY WRITE ON THE PAGE, AND IT WRITES ONE COLUMN. The rename goes
// to POST /api/edges/rename?uid=<uid>, which is registry.SetDisplayName — one
// UPDATE against edge_registry.display_name. Nothing else moves: not the uid,
// not the rows on orders / mission_telemetry / outbox that carry it, not the
// consumer group, not the wire address. That is why the button is safe to put
// in front of an operator, and it is the whole reason display_name was split
// off the identity in v66.
//
// The uid is deliberately not editable here. It is not a name, and there is no
// re-issue endpoint on Core by design — replacement hardware takes the EXISTING
// uid off this page and into the new Pi's shingoedge.yaml.

import { apiPost, toast, uiPrompt } from '/static/app.js';

function rowFor(btn) {
  const tr = btn.closest('tr');
  if (!tr) return null;
  return {
    tr,
    uid: tr.dataset.uid || '',
    nameEl: tr.querySelector('.edge-name'),
  };
}

async function renameEdge(btn) {
  const row = rowFor(btn);
  if (!row || !row.uid) return;

  const current = row.nameEl ? row.nameEl.textContent.trim() : '';
  const next = await uiPrompt('Display name for ' + row.uid, { value: current });
  if (next === null) return;

  const trimmed = next.trim();
  if (!trimmed) {
    toast('Name cannot be empty', 'error');
    return;
  }
  if (trimmed === current) return;

  btn.disabled = true;
  try {
    await apiPost('/api/edges/rename?uid=' + encodeURIComponent(row.uid),
      { display_name: trimmed });
    // Update the cell in place rather than reloading: the rename touched one
    // column and the rest of the row is unchanged. Every OTHER screen picks the
    // new name up on its own next render, because none of them stored it.
    if (row.nameEl) row.nameEl.textContent = trimmed;
    toast('Renamed to ' + trimmed, 'success');
  } catch (e) {
    toast('Rename failed: ' + e, 'error');
  } finally {
    btn.disabled = false;
  }
}

document.addEventListener('click', (ev) => {
  const btn = ev.target.closest('[data-action="renameEdge"]');
  if (!btn) return;
  ev.preventDefault();
  renameEdge(btn);
});
