// operator-supply-refusal.js — the loader operator's "I cannot supply this",
// and the withdrawal of it.
//
// One person telling another person the parts are not coming. Not an inventory
// reading and not a verdict: Shingo's own count cannot make this statement,
// because Shingo sees only part of the greater Martinrea system and somebody on
// a reach truck can see material — and the absence of it — that no query here
// will ever know about.
//
// WHY THIS IS ITS OWN MODULE. The control appears in two modals that are built
// in completely different ways: the Load Bin modal wires a button in a static
// template, and the node modal emits an HTML string dispatched by data-action.
// Neither can borrow the other's plumbing, and a copy in each is two confirm
// wordings and two failure paths that drift the first time one is edited. What
// they actually share is the DECISION, so that is what lives here; each modal
// keeps only the four lines of its own idiom.

import { postAction, showToast } from './operator-util.js';

// The card key. A refusal is always (loader window, part) — never a bare part —
// because that pair is exactly what the operator is standing in front of, and
// there is no wider thing they can see well enough to make a claim about.
function refusalURL(nodeID) {
    return '/api/process-nodes/' + nodeID + '/supply-refusal';
}

// THE CONFIRM IS NOT CEREMONY. Reporting no parts tells another operator their
// material is not coming and may end with them abandoning a run; withdrawing
// surprises a cell that has already acted on being told. Either one is a single
// tap on a screen being read from a forklift seat.
export function confirmRefuseSupply(nodeID, code, onDone) {
    if (!code) { showToast('Pick a part first', 'error'); return; }
    if (!window.confirm('Tell the cell there are no ' + code + ' available?')) return;
    postAction(refusalURL(nodeID), { payload_code: code }, onDone);
}

export function confirmUndoSupplyRefusal(nodeID, code, onDone) {
    if (!code) return;
    if (!window.confirm('Withdraw the refusal for ' + code + '?')) return;
    // DELETE carries a body because the key is (node, part) and a part number
    // cannot ride the path without inventing an encoding for the ones with a
    // slash in them.
    fetch(refusalURL(nodeID), {
        method: 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ payload_code: code }),
    }).then(function (res) {
        if (!res.ok) { showToast('Could not withdraw the refusal', 'error'); return; }
        if (onDone) onDone();
    }).catch(function () { showToast('Could not withdraw the refusal', 'error'); });
}

// standingRefusalFor answers "is this card already refused" from the board data
// the modals already hold, so neither has to fetch to decide which label to
// draw. Returns the refusal or null.
export function standingRefusalFor(entry, code) {
    if (!entry || !code) return null;
    return (entry.supply_refusals || {})[code] || null;
}

// The wording, in one place, so the two modals cannot label the same action
// differently. REPORT is an action; NO PARTS AVAILABLE is a state, and the
// state belongs on the card once the report has been made.
export const REFUSE_LABEL = 'NO PARTS AVAILABLE';
export const UNDO_LABEL = 'UNDO — I CAN SUPPLY';
