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
    const payloads = allowedPayloadsForEntry(entry);
    const selected = {};
    // Pre-populate every allowed-payload chip so the operator sees a qty
    // on each one and the PULL PARTS button is always enabled. Common case
    // is "operator pulled the whole bin to lineside" — one tap submits;
    // tap any chip to keypad-edit qty up or down. Single payload reads
    // runtime.remaining_uop_cached; multi-payload (manual_swap) reads per-part
    // from the bin manifest. Anything we don't have a signal for falls
    // back to 0 — chip stays editable.
    payloads.forEach(function(code) { selected[code] = 0; });
    if (payloads.length === 1) {
        const rt = entry && entry.runtime;
        const remainingUOP = rt && rt.remaining_uop_cached != null ? rt.remaining_uop_cached : 0;
        selected[payloads[0]] = remainingUOP;
    } else if (payloads.length > 1) {
        // Multi-payload bin holds a mix; runtime.remaining_uop_cached is a single
        // number that doesn't split across parts, so the manifest is the
        // only per-part source.
        const binState = entry && entry.bin_state;
        if (binState && binState.manifest) {
            try {
                const manifest = JSON.parse(binState.manifest);
                if (Array.isArray(manifest)) {
                    manifest.forEach(item => {
                        if (item && item.part_number && item.quantity != null &&
                            payloads.includes(item.part_number)) {
                            selected[item.part_number] = item.quantity;
                        }
                    });
                }
            } catch (e) {
                console.error('release prompt manifest parse', e);
            }
        }
    }
    // Phase 0b override audit: snapshot the auto-suggested values at
    // modal-open. Whatever the chip grid was pre-populated with becomes
    // the baseline shipped to Core as qty_by_part_suggested; any
    // operator edit afterward gets recorded as an override. The deep
    // copy is necessary because `selected` is mutated by the keypad.
    const suggested = Object.assign({}, selected);

    // Single-payload nodes also drive the RELEASE PARTIAL count baseline.
    // Capture runtime.remaining_uop_cached so the partial-count keypad has a
    // pre-population value and Core can detect overrides on that path too.
    const rtSnapshot = entry && entry.runtime;
    const partialCountSuggested = rtSnapshot && rtSnapshot.remaining_uop_cached != null
        ? rtSnapshot.remaining_uop_cached : 0;

    releasePromptState = {
        url, entry, payloads, selected, suggested,
        partialCount: partialCountSuggested,
        partialCountSuggested,
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

    if (state.payloads.length === 0) {
        html += '<div class="os-release-prompt"><div style="color:#999;padding:12px 0;font-size:14px">No allowed payloads on this node.</div></div>';
    } else {
        // Primary group: the chip grid and the PULL PARTS button visually
        // belong together — the chips show what's about to be captured and
        // the button submits it. Highlighted so the operator's eye lands
        // here first; the partial/empty escape hatch and CANCEL sit below
        // in a quieter row.
        html += '<div class="os-release-primary">';
        html += '<div class="os-release-primary-label">Anything pulled to lineside? Tap a part to edit qty.</div>';
        html += '<div class="os-release-part-grid">';
        state.payloads.forEach(function(code) {
            const qty = state.selected[code] != null ? state.selected[code] : 0;
            const picked = qty > 0;
            html += '<button type="button" class="os-action-btn os-release-part-btn' +
                (picked ? ' picked' : '') + '" data-action="release-pick:' + esc(code) + '">' +
                esc(code) + ' (' + qty + ')</button>';
        });
        html += '</div>';
        html += '<button type="button" class="os-action-btn request"' +
            ' data-action="release-submit-parts">PULL PARTS LINESIDE, RELEASE</button>';
        html += '</div>';
    }

    // RELEASE PARTIAL exposes the operator-overridable count. Operator
    // taps the count to keypad-edit (Phase 0b — was a fixed runtime read).
    // RELEASE EMPTY has no count; the bin is already zero by definition.
    const rt = state.entry && state.entry.runtime;
    const remainingUOP = rt && rt.remaining_uop_cached != null ? rt.remaining_uop_cached : 0;
    const partialCount = state.partialCount != null ? state.partialCount : remainingUOP;
    if (remainingUOP > 0) {
        html += '<div class="os-release-partial-count">';
        html += '<div class="os-release-primary-label">Bin returning to supermarket with:</div>';
        html += '<button type="button" class="os-release-qty-display"' +
            ' data-action="release-partial-edit">' + partialCount + ' UOP</button>';
        html += '</div>';
    }

    html += '<div class="os-modal-actions">';
    // Label reflects the actual action: bin still has UOP → returns as-is
    // (partial); bin is at zero → manifest cleared (empty). Same wire
    // disposition mapping as before; just makes it visible to the operator.
    const submitLabel = remainingUOP > 0 ? 'RELEASE PARTIAL' : 'RELEASE EMPTY';
    const submitTitle = remainingUOP > 0
        ? 'No parts pulled to lineside. Bin returns to the supermarket with its current UOP intact.'
        : 'No parts pulled to lineside. Bin is empty — manifest cleared.';
    html += '<button type="button" class="os-action-btn release-empty"' +
        ' data-action="release-submit"' +
        ' title="' + esc(submitTitle) + '">' +
        submitLabel + '</button>';
    // Underpack release: bin physically empty before count reaches zero.
    // Only offered when the system still thinks the bin has UOP — otherwise
    // RELEASE EMPTY is the right path. Routes through a confirmation
    // modal because it's a destructive-ish action: the missing inventory
    // is recorded as a forensics signal.
    if (remainingUOP > 0) {
        html += '<button type="button" class="os-action-btn release-empty"' +
            ' data-action="release-underpack-confirm"' +
            ' title="Bin is physically empty, but the system still shows UOP remaining. Records the gap as missing inventory.">' +
            'BIN EMPTY (UNDER COUNT)</button>';
    }
    html += '<button type="button" class="os-action-btn close" data-action="release-cancel">CANCEL</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
    nodeModal.hidden = false;
}

// renderReleasePromptUnderpackConfirm shows the destructive-action
// confirmation for the BIN EMPTY (UNDER COUNT) flow. Pre-condition:
// remainingUOP > 0 at modal-open (the button only appears in that
// case). Display the missing amount so the operator knows the
// magnitude of what they're declaring missing.
function renderReleasePromptUnderpackConfirm() {
    const state = releasePromptState;
    if (!state) return;
    const rt = state.entry && state.entry.runtime;
    const remainingUOP = rt && rt.remaining_uop_cached != null ? rt.remaining_uop_cached : 0;

    let html = '';
    html += '<div class="os-modal-header">';
    html += '<div class="os-modal-node-name">Declare bin empty?</div>';
    html += '<div class="os-modal-payload">Confirm: bin is physically empty, system shows UOP remaining.</div>';
    html += '</div>';

    html += '<div class="os-release-prompt">';
    html += '<div class="os-release-primary-label">';
    html += 'System shows <strong>' + esc(String(remainingUOP)) + ' UOP</strong> remaining.<br>';
    html += 'This will record <strong>' + esc(String(remainingUOP)) + ' units</strong> as missing inventory.';
    html += '</div>';
    html += '</div>';

    html += '<div class="os-modal-actions">';
    html += '<button type="button" class="os-action-btn close" data-action="release-back">BACK</button>';
    html += '<button type="button" class="os-action-btn release-empty"' +
        ' data-action="release-submit-underpack">DECLARE EMPTY</button>';
    html += '</div>';

    nodeModalContent.innerHTML = html;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
}

function renderReleasePromptStep2(code) {
    const state = releasePromptState;
    if (!state) return;

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

    // Underpack confirmation step. Operator clicked "BIN EMPTY (UNDER
    // COUNT)" on step 1; show the missing-inventory amount and require
    // an explicit second tap before shipping. Cancel returns to step 1.
    if (action === 'release-underpack-confirm') {
        renderReleasePromptUnderpackConfirm();
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
                // Always store a numeric qty so the chip's "(N)" suffix
                // stays consistent — pre-population now seeds every chip
                // with 0, so deleting on qty=0 would orphan that initial
                // state and the chip would stop showing its number.
                state.selected[code] = qty > 0 ? qty : 0;
                renderReleasePromptStep2(code);
            },
        });
        return;
    }

    // Phase 0b: operator edits the RELEASE PARTIAL count. Pre-populated
    // from runtime.remaining_uop_cached at modal-open; any change becomes an
    // override that Core records to bin_uop_audit.
    if (action === 'release-partial-edit') {
        const current = state.partialCount != null ? state.partialCount : 0;
        openKeypad(0, current, {
            title: 'Bin remaining (UOP)',
            onOk: function(_nodeID, qty) {
                state.partialCount = qty > 0 ? qty : 0;
                renderReleasePromptStep1();
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

    //   release-submit            "RELEASE PARTIAL" / "RELEASE EMPTY"
    //                              → send_partial_back when the bin still has UOP
    //                                (preserve manifest); capture_lineside empty
    //                                when the bin is already empty (operator
    //                                confirms zero, manifest cleared). Label is
    //                                chosen at render time from remaining_uop so
    //                                the operator sees what they're submitting.
    //   release-submit-parts      "PULL PARTS LINESIDE, RELEASE" → capture_lineside, picked buckets.
    //   release-submit-underpack  "DECLARE EMPTY" (after confirmation) →
    //                              release_underpack disposition. Same wire
    //                              shape as RELEASE EMPTY (manifest clear);
    //                              distinct audit op so forensics can trend
    //                              missing-inventory patterns separately.
    if (action === 'release-submit' || action === 'release-submit-parts' || action === 'release-submit-underpack') {
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
        const remainingUOP = rt && rt.remaining_uop_cached != null ? rt.remaining_uop_cached : 0;
        let body;
        if (action === 'release-submit-underpack') {
            // Underpack: same wire shape as RELEASE EMPTY. The
            // disposition string is what flips the audit op at Core
            // to released_underpack so forensics can trend
            // missing-inventory separately.
            body = {
                disposition: 'release_underpack',
                called_by: calledBy,
            };
        } else if (action === 'release-submit' && remainingUOP > 0) {
            // Phase 0b: ship the operator-entered count + system-suggested
            // baseline so Core can audit overrides.
            body = {
                disposition: 'send_partial_back',
                partial_count: state.partialCount != null ? state.partialCount : remainingUOP,
                partial_count_suggested: state.partialCountSuggested != null
                    ? state.partialCountSuggested : remainingUOP,
                called_by: calledBy,
            };
        } else {
            // Drop zero-qty entries — chips are pre-populated with 0 so
            // the button is always enabled, but a 0 bucket has no business
            // hitting the server.
            const qtyByPart = {};
            const qtyByPartSuggested = {};
            if (action === 'release-submit-parts') {
                Object.keys(state.selected || {}).forEach(function(code) {
                    if (state.selected[code] > 0) qtyByPart[code] = state.selected[code];
                });
                // Phase 0b override audit: ship the modal-open snapshot
                // alongside the operator's submit. Core compares the two
                // and records every divergence. Include every part the
                // chip grid showed, even those left at 0 — a zero
                // suggested with a positive operator submit is a valid
                // override (operator added a part the system didn't
                // suggest), and vice versa.
                Object.keys(state.suggested || {}).forEach(function(code) {
                    qtyByPartSuggested[code] = state.suggested[code] || 0;
                });
            }
            body = {
                disposition: 'capture_lineside',
                qty_by_part: qtyByPart,
                qty_by_part_suggested: qtyByPartSuggested,
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
