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

// claimEdge answers "what is this station?" for an edge that introduced itself.
//
// Deliberately a DIFFERENT call from rename, even though both end up writing
// display_name. Rename relabels a station somebody already accounted for; claim
// is the first time anybody has. Collapsing them would lose the only record of
// whether a human ever looked at this box — see edge_registry.claimed_at.
async function claimEdge(btn) {
  const row = rowFor(btn);
  if (!row || !row.uid) return;

  const name = await uiPrompt(
    'This edge introduced itself as ' + row.uid + '.\n\n' +
    'If it is a NEW station, name it.\n' +
    'If it REPLACES an existing station, cancel — put that station\'s uid on the box instead.',
    { value: '' });
  if (name === null) return;

  const trimmed = name.trim();
  if (!trimmed) {
    toast('Give it a name, or cancel if this replaces an existing station', 'error');
    return;
  }

  btn.disabled = true;
  try {
    await apiPost('/api/edges/claim?uid=' + encodeURIComponent(row.uid),
      { display_name: trimmed });
    toast('Claimed as ' + trimmed, 'success');
    window.location.reload();
  } catch (e) {
    toast('Claim failed: ' + e, 'error');
    btn.disabled = false;
  }
}

document.addEventListener('click', (ev) => {
  const rename = ev.target.closest('[data-action="renameEdge"]');
  if (rename) { ev.preventDefault(); renameEdge(rename); return; }
  const claim = ev.target.closest('[data-action="claimEdge"]');
  if (claim) { ev.preventDefault(); claimEdge(claim); }
});
