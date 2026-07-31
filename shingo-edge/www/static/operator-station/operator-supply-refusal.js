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

// askConfirm is the HMI's own confirmation, not the browser's.
//
// This was window.confirm and that was wrong twice over. It does not look like
// anything else in this HMI — different type, different buttons, a URL in the
// title bar — on a screen an operator reads at a glance from a distance. And on
// a kiosk it is a modal owned by the browser rather than the page: it blocks the
// event loop, so the board's poll stops behind it, and "prevent this page from
// creating more dialogs" can suppress it outright with nothing to fall back to.
//
// Same overlay furniture as the changeover picker and the cell's own refusal
// prompt (os-co-picker-*), so it reads as part of the application.
function askConfirm(title, detail, confirmLabel, onConfirm) {
    if (document.querySelector('.os-refusal-confirm-overlay')) return;

    const overlay = document.createElement('div');
    overlay.className = 'os-co-picker-overlay os-refusal-confirm-overlay';
    const panel = document.createElement('div');
    panel.className = 'os-co-picker';

    const t = document.createElement('div');
    t.className = 'os-co-picker-title';
    t.textContent = title;
    panel.appendChild(t);

    if (detail) {
        const d = document.createElement('div');
        d.className = 'os-co-picker-verdict';
        d.textContent = detail;
        panel.appendChild(d);
    }

    const go = document.createElement('button');
    go.className = 'os-co-picker-btn';
    go.textContent = confirmLabel;
    go.addEventListener('click', function () { overlay.remove(); onConfirm(); });
    panel.appendChild(go);

    // CANCEL is present and is the way out. Unlike the cell's refusal prompt —
    // which deliberately has no dismiss, because dismissing it IS answering it —
    // this one is a question about an action not yet taken, so backing out has
    // to be free.
    const cancel = document.createElement('button');
    cancel.className = 'os-co-picker-btn';
    cancel.textContent = 'CANCEL';
    cancel.addEventListener('click', function () { overlay.remove(); });
    panel.appendChild(cancel);

    overlay.addEventListener('click', function (evt) {
        if (evt.target === overlay) overlay.remove();
    });

    overlay.appendChild(panel);
    document.body.appendChild(overlay);
}

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
    askConfirm(
        'NO ' + code + ' AVAILABLE?',
        'The cell waiting on this part will be told, and asked whether to keep waiting or change over.',
        'YES — TELL THEM',
        function () { postAction(refusalURL(nodeID), { payload_code: code }, onDone); },
    );
}

export function confirmUndoSupplyRefusal(nodeID, code, onDone) {
    if (!code) return;
    askConfirm(
        'WITHDRAW THE REFUSAL FOR ' + code + '?',
        'The cell may already have acted on being told the part was not coming.',
        'YES — I CAN SUPPLY',
        function () { doUndo(nodeID, code, onDone); },
    );
}

function doUndo(nodeID, code, onDone) {
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
