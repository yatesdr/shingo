import { esc, postAction } from './operator-util.js';
import { getView, getSelectedNodeID, findNodeByID } from './operator-state.js';

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

    // remaining_uop != null guard covers both absent field and explicit
    // JSON-null. null != null is false so both fall through to 0.
    const rt = state.entry && state.entry.runtime;
    const claim = state.entry && (state.entry.active_claim || state.entry.target_claim);
    const remainingUOP = rt && rt.remaining_uop != null ? rt.remaining_uop : 0;
    const capacityUOP = claim && claim.uop_capacity ? claim.uop_capacity : 0;
    const canSendPartial = remainingUOP > 0;

    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="release-cancel">CANCEL</button>';
    html += '<button type="button" class="os-action-btn empty-tools" data-action="release-submit">NOTHING PULLED</button>';
    html += '<button type="button" class="os-action-btn"' +
        (canSendPartial ? '' : ' disabled') +
        ' data-action="release-submit-partial" title="Return the partial bin to the supermarket without capturing leftovers as lineside inventory. Count is captured at this moment — any further consumption before the robot picks the bin up is not tracked.">' +
        'SEND PARTIAL BACK' +
        (capacityUOP > 0
            ? ' (' + remainingUOP + ' of ' + capacityUOP + ' — at this moment)'
            : ' (' + remainingUOP + ' — at this moment)') +
        '</button>';
    const hasPicks = Object.keys(state.selected).length > 0;
    html += '<button type="button" class="os-action-btn request"' +
        (hasPicks ? '' : ' disabled') +
        ' data-action="release-submit-parts">CONFIRM & RELEASE</button>';
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

    const softCap = linesideSoftThresholdForEntry(state.entry);
    const warnAt = softCap > 0 ? softCap * 2 : 0;

    let html = '';
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">Lineside qty: ' + esc(code) + '</div>';
    html += '<div class="os-modal-payload">How many ' + esc(code) + ' parts did you pull?</div>';
    html += '</div>';

    html += '<div class="os-release-prompt">';
    html += '<input type="number" id="os-release-qty" min="1" step="1" value="' +
        (state.selected[code] || '') + '" ' +
        'style="width:100%;padding:14px;font-size:28px;text-align:center;border-radius:6px;' +
        'border:1px solid var(--os-gray,#444);background:#111;color:#fff;">';
    html += '<div id="os-release-softcap-warn" class="os-release-softcap-warn" ' +
        'data-warn-at="' + warnAt + '" hidden>';
    html += 'Typo check: this is more than 2\u00D7 the configured lineside soft cap (' +
        softCap + '). Release anyway if that\u2019s right.';
    html += '</div>';
    html += '</div>';

    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="release-back">BACK</button>';
    html += '<button type="button" class="os-action-btn request" data-action="release-qty-ok:' + esc(code) + '">OK</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
    const input = document.getElementById('os-release-qty');
    const warn = document.getElementById('os-release-softcap-warn');
    function refreshSoftCapWarn() {
        if (!warn) return;
        if (warnAt <= 0) { warn.hidden = true; return; }
        const v = parseInt(input.value || '0', 10);
        warn.hidden = !(v > warnAt);
    }
    if (input) {
        input.focus();
        input.select();
        input.addEventListener('input', refreshSoftCapWarn);
        refreshSoftCapWarn();
    }
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

    if (action.startsWith('release-qty-ok:')) {
        const code = action.slice('release-qty-ok:'.length);
        const input = document.getElementById('os-release-qty');
        const qty = input ? parseInt(input.value || '0', 10) : 0;
        if (qty > 0) {
            state.selected[code] = qty;
        } else {
            delete state.selected[code];
        }
        renderReleasePromptStep1();
        return;
    }

    //   release-submit         "NOTHING PULLED"        → capture_lineside, empty buckets
    //   release-submit-parts   "CONFIRM & RELEASE"     → capture_lineside, picked buckets
    //   release-submit-partial "SEND PARTIAL BACK"     → send_partial_back, no buckets
    if (action === 'release-submit' || action === 'release-submit-parts' || action === 'release-submit-partial') {
        const url = state.url;
        const view = getView();
        const calledBy = (view && view.station && view.station.name) ? view.station.name : 'operator';
        let body;
        if (action === 'release-submit-partial') {
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
