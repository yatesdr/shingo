import { esc, postAction } from './operator-util.js';
import { getView, getSelectedNodeID, findNodeByID } from './operator-state.js';
import { openKeypad } from './operator-keypad.js';

const nodeModal = document.getElementById('node-modal');
const nodeModalContent = document.getElementById('node-modal-content');

let releasePromptState = null;
let renderModalRef = null;
let closeModalRef = null;
let loadViewRef = null;

export function setReleaseRefs(refs) {
    renderModalRef = refs.renderModal;
    closeModalRef = refs.closeModal;
    loadViewRef = refs.loadView;
}

export function isReleasePromptOpen() {
    return releasePromptState !== null;
}

function allowedPayloadsForEntry(entry) {
    if (!entry) return [];
    const claim = entry.active_claim;
    if (!claim) return [];
    if (claim.allowed_payload_codes && claim.allowed_payload_codes.length > 0) {
        return claim.allowed_payload_codes.slice();
    }
    if (claim.payload_code) return [claim.payload_code];
    return [];
}

// Pulled-to-lineside qty is a property of the running style, so read the
// active claim first; fall back to target claim on cold-start.
function linesideSoftThresholdForEntry(entry) {
    if (!entry) return 0;
    const claim = entry.active_claim || entry.target_claim;
    if (!claim) return 0;
    const v = parseInt(claim.lineside_soft_threshold, 10);
    return isNaN(v) || v < 0 ? 0 : v;
}

export function openReleasePrompt(url, entry) {
    releasePromptState = {
        url: url,
        entry: entry,
        payloads: allowedPayloadsForEntry(entry),
        selected: {},
    };
    renderReleasePromptStep1();
}

function renderReleasePromptStep1() {
    const state = releasePromptState;
    if (!state) return;

    let html = '';
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">Release</div>';
    html += '<div class="os-modal-payload">Anything pulled to lineside during the swap?</div>';
    html += '</div>';

    html += '<div class="os-release-prompt">';
    if (state.payloads.length === 0) {
        html += '<div style="color:#999;padding:12px 0;font-size:14px">No allowed payloads on this node.</div>';
    } else {
        html += '<div class="os-release-part-grid">';
        state.payloads.forEach(function(code) {
            const picked = state.selected[code] != null;
            html += '<button type="button" class="os-action-btn os-release-part-btn' +
                (picked ? ' picked' : '') + '" data-action="release-pick:' + esc(code) + '">' +
                esc(code) + (picked ? ' (' + state.selected[code] + ')' : '') + '</button>';
        });
        html += '</div>';
    }
    html += '</div>';

    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="release-cancel">CANCEL</button>';
    html += '<button type="button" class="os-action-btn release-empty"' +
        ' data-action="release-submit"' +
        ' title="No parts were pulled to lineside. If the bin still has UOP left it returns to the supermarket as-is; if the bin is empty the manifest is cleared.">' +
        'NOTHING PULLED</button>';
    const hasPicks = Object.keys(state.selected).length > 0;
    html += '<button type="button" class="os-action-btn request"' +
        (hasPicks ? '' : ' disabled') +
        ' data-action="release-submit-parts">PULL PARTS LINESIDE, RELEASE</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
    nodeModal.hidden = false;
}

function renderReleasePromptStep2(code) {
    const state = releasePromptState;
    if (!state) return;

    // Auto-fill qty with the bin's remaining_uop the first time we land on
    // step 2 for this code. Most of the time the operator pulled the whole
    // bin to lineside; they can tap the display to dial it down via the
    // keypad. The keypad is the only path to changing the value, so once
    // selected[code] is set we leave it alone.
    if (state.selected[code] == null) {
        const rt = state.entry && state.entry.runtime;
        const remainingUOP = rt && rt.remaining_uop != null ? rt.remaining_uop : 0;
        if (remainingUOP > 0) state.selected[code] = remainingUOP;
    }

    const qty = state.selected[code] || 0;
    const softCap = linesideSoftThresholdForEntry(state.entry);
    const warnAt = softCap > 0 ? softCap * 2 : 0;
    const showWarn = warnAt > 0 && qty > warnAt;

    let html = '';
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">Lineside qty: ' + esc(code) + '</div>';
    html += '<div class="os-modal-payload">Tap the number to change it.</div>';
    html += '</div>';

    html += '<div class="os-release-prompt">';
    html += '<button type="button" class="os-release-qty-display" data-action="release-qty-edit:' +
        esc(code) + '">' + qty + '</button>';
    if (showWarn) {
        html += '<div class="os-release-softcap-warn">';
        html += 'Typo check: this is more than 2\u00D7 the configured lineside soft cap (' +
            softCap + '). Release anyway if that\u2019s right.';
        html += '</div>';
    }
    html += '</div>';

    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="release-back">BACK</button>';
    const okDisabled = !(qty > 0);
    html += '<button type="button" class="os-action-btn request"' +
        (okDisabled ? ' disabled' : '') +
        ' data-action="release-qty-ok:' + esc(code) + '">OK</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
}

async function handleReleasePromptAction(evt) {
    const action = evt.currentTarget.dataset.action;
    const state = releasePromptState;
    if (!action || !state) return;

    if (action === 'release-cancel') {
        closeReleasePrompt();
        const sid = getSelectedNodeID();
        if (sid !== null) {
            const entry = findNodeByID(sid);
            if (entry && renderModalRef) renderModalRef(entry);
        }
        return;
    }

    if (action === 'release-back') {
        renderReleasePromptStep1();
        return;
    }

    if (action.startsWith('release-pick:')) {
        const code = action.slice('release-pick:'.length);
        renderReleasePromptStep2(code);
        return;
    }

    if (action.startsWith('release-qty-edit:')) {
        const code = action.slice('release-qty-edit:'.length);
        const current = state.selected[code] || 0;
        openKeypad(0, current, {
            title: 'Lineside qty: ' + code,
            onOk: function(_nodeID, qty) {
                if (qty > 0) {
                    state.selected[code] = qty;
                } else {
                    delete state.selected[code];
                }
                renderReleasePromptStep2(code);
            },
        });
        return;
    }

    if (action.startsWith('release-qty-ok:')) {
        // qty already lives in state.selected (auto-filled on entry, or
        // overwritten via the keypad). Step 1 handles the picked-state UI.
        renderReleasePromptStep1();
        return;
    }

    //   release-submit       "NOTHING PULLED"               → send_partial_back when the bin
    //                                                         still has UOP (preserve manifest);
    //                                                         capture_lineside empty when the
    //                                                         bin is already empty (operator
    //                                                         confirms zero, manifest cleared).
    //   release-submit-parts "PULL PARTS LINESIDE, RELEASE" → capture_lineside, picked buckets.
    if (action === 'release-submit' || action === 'release-submit-parts') {
        const url = state.url;
        const view = getView();
        // Trim before falling back: a whitespace-only station name (e.g. a
        // station row created with " ") is truthy in JS, would be sent as
        // called_by=" ", and rejected by the backend's TrimSpace check —
        // surfacing as "release requires called_by". Match the server's
        // TrimSpace semantics here so the fallback always wins.
        const stationName = (view && view.station && view.station.name) ? String(view.station.name).trim() : '';
        const calledBy = stationName || 'operator';
        const rt = state.entry && state.entry.runtime;
        const remainingUOP = rt && rt.remaining_uop != null ? rt.remaining_uop : 0;
        let body;
        if (action === 'release-submit' && remainingUOP > 0) {
            body = {
                disposition: 'send_partial_back',
                called_by: calledBy,
            };
        } else {
            const qtyByPart = (action === 'release-submit') ? {} : (state.selected || {});
            body = {
                disposition: 'capture_lineside',
                qty_by_part: qtyByPart,
                called_by: calledBy,
            };
        }
        closeReleasePrompt();
        evt.currentTarget.disabled = true;
        const ok = await postAction(url, body, loadViewRef);
        if (ok && closeModalRef) closeModalRef();
        return;
    }
}

function closeReleasePrompt() {
    releasePromptState = null;
}

// Stub view: scrap / repack / recall actions land in a later phase.
export function openStrandedStub(bucket, handleModalAction) {
    let html = '';
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">Stranded at lineside</div>';
    html += '<div class="os-modal-payload">' + esc(bucket.part_number) + ' — ' + (bucket.qty || 0) + ' unit' + ((bucket.qty || 0) === 1 ? '' : 's') + '</div>';
    html += '</div>';
    html += '<div style="padding:12px 0;color:#bbb;font-size:14px;line-height:1.4">';
    html += 'These parts were captured during a previous changeover and are not counting toward the active style.<br><br>';
    html += '<strong>Scrap / repack / recall actions will land in a later phase.</strong>';
    html += '</div>';
    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="close">CLOSE</button>';
    html += '</div>';
    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleModalAction);
    });
    nodeModal.hidden = false;
}
