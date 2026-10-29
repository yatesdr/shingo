// esc is the one escape for markup built by concatenation, quotes included:
// the prompt puts payload codes inside attributes.
import { esc } from '/static/shared/esc.js';
import { postAction } from './operator-util.js';
import { getView, getSelectedNodeID, findNodeByID } from './operator-state.js';
import { openKeypad } from './operator-keypad.js';

const nodeModal = document.getElementById('node-modal');
const nodeModalContent = document.getElementById('node-modal-content');

// releasePromptState is the prompt on screen, or null. It stays set while its
// release is in flight (posting), so the board does not repaint the modal under
// it and a refusal leaves it live; it is cleared when the answer is a yes, and
// when the modal closes (closeModal calls closeReleasePrompt). Pinned by
// operator-safe-taps.test.js.
let releasePromptState = null;
let renderModalRef = null;
let modalOpeningRef = null;
let closeModalFromRef = null;
let loadViewRef = null;

// refs: renderModal; modalOpening and closeModalFrom, so a release closes only
// the modal it came from (operator-modal.js); loadView.
export function setReleaseRefs(refs) {
    renderModalRef = refs.renderModal;
    modalOpeningRef = refs.modalOpening;
    closeModalFromRef = refs.closeModalFrom;
    loadViewRef = refs.loadView;
}

function modalOpening() {
    return modalOpeningRef ? modalOpeningRef() : 0;
}
function closeModalFrom(opening) {
    if (closeModalFromRef) closeModalFromRef(opening);
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

// calledBy is the station name, trimmed. A whitespace-only station name (a
// row created with " ") is truthy in JS, would ship as called_by=" ", and the
// backend's TrimSpace check rejects it as "release requires called_by" — so
// trim BEFORE falling back and the fallback always wins.
function calledByFromView() {
    const view = getView();
    const stationName = (view && view.station && view.station.name)
        ? String(view.station.name).trim() : '';
    return stationName || 'operator';
}

// capture_lineside with nothing pulled — the body a consume release at zero
// with no payload chips posts: the disposition names what was pulled to
// lineside, and the answer is "nothing". A produce release posts releaseBody
// instead.
function releasedNothingBody(calledBy) {
    return {
        disposition: 'capture_lineside',
        qty_by_part: {},
        qty_by_part_suggested: {},
        called_by: calledBy,
    };
}

// A PRODUCE RELEASE SAYS NOTHING ABOUT LINESIDE, because the question does not
// apply: the bin is going OUT, not coming back drawn-down. So this body
// carries no disposition at all, which Core reads as "no manifest action" and
// leaves the carrier's contents alone.
//
// IT DECLARES NOTHING EITHER. Whether the bin is booked full is the engine's,
// at the finalize (declaredFull, from the claim's pack tolerance), and the
// tile's RELEASE / RELEASE SHORT reads the same helper. It was named
// releasedFullBody, and a FULL name on a body that says nothing is the shape of
// the incident below.
//
// It used to post releasedNothingBody — capture_lineside with an empty
// qty_by_part — which ComputeReleaseRemainingUOP turns into &0, i.e. "the bin
// is empty, clear it". The screen said RELEASE FULL and the wire said empty.
// That contradiction was inert only while the engine's produce-role branch
// discarded the disposition; releaseOrderDropFastPath skips that branch, so at
// a changeover the meaningless payload became the instruction. Hopkinsville:
// 46 wipes, 140,762 parts, 2026-07-16 to 09-08.
function releaseBody(calledBy) {
    return { called_by: calledBy };
}

// ── A RELEASE WITH ONE OUTCOME DOES NOT ASK ─────────────────────────────
//
// The prompt exists to collect a DISPOSITION. In two shapes there is none to
// collect and it renders a single actionable button, so the operator taps
// RELEASE and is asked to tap RELEASE again — the "click a similar button
// twice for one action" the plants report:
//
//   produce role           the engine discards the disposition entirely for
//                          produce (release_commit.go, the produce arm), so
//                          RELEASE posts releaseBody and the prompt never
//                          opens; its consume framing — "anything pulled to
//                          lineside?", "bin returning to supermarket" — is
//                          backwards for a node pushing a bin OUT. This is the
//                          only produce path: no produce screen exists.
//   consume, no payloads,  no chip grid to pick from, and the PARTIAL and
//   remaining_uop == 0     UNDER COUNT arms are both remaining_uop > 0 only,
//                          leaving RELEASE EMPTY + CANCEL.
//
// Returns the body that single button would have posted, or null when the
// modal holds a real choice. remaining_uop > 0 ALWAYS renders: that is the
// operator picking between PULL PARTS / RELEASE PARTIAL / BIN EMPTY (UNDER
// COUNT), and the last two disagree about whether inventory went missing.
//
// Deliberately NOT collapsed: consume at zero WITH payload chips. Both its
// buttons post an identical body while every chip reads 0, but a chip is
// keypad-editable, so an operator can still declare a lineside pull the count
// never knew about. Collapsing that would silently delete the only path for
// "the bin said zero but I took parts off it".
function singleOutcomeReleaseBody(entry, payloads) {
    const claim = entry && entry.active_claim;
    if (!claim) return null;
    if (claim.role === 'produce') return releaseBody(calledByFromView());
    if (payloads.length > 0) return null;
    // STRICTER THAN THE RENDERER, DELIBERATELY. renderReleasePromptStep1 reads
    // a missing remaining_uop_cached as 0 because it is only choosing a label
    // and "RELEASE EMPTY" is the safe caption for an unknown count. Here the
    // same default would auto-post a manifest-clearing disposition for a bin
    // whose count never loaded. Unknown is not zero: fall through to the modal
    // and let the operator look at the bin.
    const rt = entry && entry.runtime;
    if (!rt || rt.remaining_uop_cached == null) return null;
    if (rt.remaining_uop_cached > 0) return null;
    return releasedNothingBody(calledByFromView());
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

export async function openReleasePrompt(url, entry) {
    const payloads = allowedPayloadsForEntry(entry);

    // Nothing to ask → don't ask. Posts the body the modal's single button
    // would have posted, so the wire shape is unchanged and the engine cannot
    // tell the difference. releasePromptState is never set, so the modal never
    // opens and isReleasePromptOpen() stays false for the SSE re-render guard.
    const direct = singleOutcomeReleaseBody(entry, payloads);
    if (direct) {
        const opening = modalOpening();
        const ok = await postAction(url, direct, loadViewRef);
        if (ok) closeModalFrom(opening);
        return;
    }

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
        //
        // The per-part split is NOT IMPLEMENTED here, and the attempt at it has
        // been deleted: it gated on Array.isArray of a manifest Core ships as
        // an object, and read a `quantity` the manifest line no longer carries.
        // Doing it properly means uop_remaining x each part's parts_per_cycle
        // from the payload template, which is a Core-side derivation this page
        // does not have. A multi-payload bin therefore gets no per-part
        // prefill, which is the honest state.
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
        changeover: isChangeoverRelease(url),
        screen: renderReleasePromptStep1,
        posting: false,
    };
    renderReleasePromptStep1();
}

// isChangeoverRelease reports whether the prompt answers a changeover's
// release (the tile's RELEASE: READY / TOOLING DONE, changeoverReleaseAction in
// operator-modal.js). Its cart is leaving a node whose style is changing, so
// nothing is pulled to the bench: the cutover freezes every bench pile, and the
// server refuses the pull (release gate G9).
function isChangeoverRelease(url) {
    return /\/changeover\/release(\?|$)/.test(String(url || ''));
}

// renderReleasePromptStep1 draws the consume release's first screen. The
// prompt is consume framing throughout — "anything pulled to lineside?",
// RELEASE PARTIAL / RELEASE EMPTY, "bin returning to supermarket" — and a
// produce release never reaches it: singleOutcomeReleaseBody posts releaseBody
// for every produce claim before the prompt opens. The produce screen this
// once branched to (RELEASE FULL) was reachable from nowhere and is gone.
function renderReleasePromptStep1() {
    const state = releasePromptState;
    if (!state) return;
    state.screen = renderReleasePromptStep1;

    nodeModalContent.innerHTML = releasePromptStep1HTML(state);
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) {
        btn.addEventListener('click', handleReleasePromptAction);
    });
    nodeModal.classList.add('active');
}

// releasePromptStep1HTML is the consume release's first screen. A changeover
// release shows no PULL PARTS: the cart goes back with its count (RELEASE
// PARTIAL) or is declared empty (BIN EMPTY / RELEASE EMPTY).
function releasePromptStep1HTML(state) {
    let html = '';
    html += '<div class="modal-header">';
    html += '<div class="modal-node-name">Release</div>';
    html += state.changeover
        ? '<div class="modal-payload">Changeover: the bin goes back to the supermarket with its parts.</div>'
        : '<div class="modal-payload">Anything pulled to lineside during the swap?</div>';
    html += '</div>';

    // A changeover release has no primary pull group: the partial count and
    // the buttons below say what the cart goes back with.
    if (!state.changeover && state.payloads.length === 0) {
        html += '<div class="os-release-prompt"><div style="color:#999;padding:12px 0;font-size:14px">No allowed payloads on this node.</div></div>';
    } else if (!state.changeover) {
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
            ' data-action="release-partial-edit">' + partialCount + ' UoP</button>';
        html += '</div>';
    }

    html += '<div class="modal-actions">';
    // Label reflects the actual action: bin still has UoP → returns as-is
    // (partial); bin is at zero → manifest cleared (empty). Same wire
    // disposition mapping as before; just makes it visible to the operator.
    const submitLabel = remainingUOP > 0 ? 'RELEASE PARTIAL' : 'RELEASE EMPTY';
    const submitTitle = remainingUOP > 0
        ? 'No parts pulled to lineside. Bin returns to the supermarket with its current UoP intact.'
        : 'No parts pulled to lineside. Bin is empty — manifest cleared.';
    html += '<button type="button" class="os-action-btn release-empty"' +
        ' data-action="release-submit"' +
        ' title="' + esc(submitTitle) + '">' +
        submitLabel + '</button>';
    // Underpack release: bin physically empty before count reaches zero.
    // Only offered when the system still thinks the bin has UoP — otherwise
    // RELEASE EMPTY is the right path. Routes through a confirmation
    // modal because it's a destructive-ish action: the missing inventory
    // is recorded as a forensics signal.
    if (remainingUOP > 0) {
        html += '<button type="button" class="os-action-btn release-empty"' +
            ' data-action="release-underpack-confirm"' +
            ' title="Bin is physically empty, but the system still shows UoP remaining. Records the gap as missing inventory.">' +
            'BIN EMPTY (UNDER COUNT)</button>';
    }
    html += '<button type="button" class="os-action-btn close" data-action="release-cancel">CANCEL</button>';
    html += '</div>';
    return html;
}

// renderReleasePromptUnderpackConfirm shows the destructive-action
// confirmation for the BIN EMPTY (UNDER COUNT) flow. Pre-condition:
// remainingUOP > 0 at modal-open (the button only appears in that
// case). Display the missing amount so the operator knows the
// magnitude of what they're declaring missing.
function renderReleasePromptUnderpackConfirm() {
    const state = releasePromptState;
    if (!state) return;
    state.screen = renderReleasePromptUnderpackConfirm;
    const rt = state.entry && state.entry.runtime;
    const remainingUOP = rt && rt.remaining_uop_cached != null ? rt.remaining_uop_cached : 0;

    let html = '';
    html += '<div class="modal-header">';
    html += '<div class="modal-node-name">Declare bin empty?</div>';
    html += '<div class="modal-payload">Confirm: bin is physically empty, system shows UoP remaining.</div>';
    html += '</div>';

    html += '<div class="os-release-prompt">';
    html += '<div class="os-release-primary-label">';
    html += 'System shows <strong>' + esc(String(remainingUOP)) + ' UoP</strong> remaining.<br>';
    html += 'This will record <strong>' + esc(String(remainingUOP)) + ' units</strong> as missing inventory.';
    html += '</div>';
    html += '</div>';

    html += '<div class="modal-actions">';
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
    html += '<div class="modal-header">';
    html += '<div class="modal-node-name">Lineside qty: ' + esc(code) + '</div>';
    html += '<div class="modal-payload">Tap the number to change it.</div>';
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

    html += '<div class="modal-actions">';
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
    if (!action || !state || state.posting) return;

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
            title: 'Bin remaining (UoP)',
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
    //                              → send_partial_back when the bin still has UoP
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
    // A produce release has no arm here: it never opens the prompt
    // (singleOutcomeReleaseBody posts releaseBody first).
    if (action === 'release-submit' || action === 'release-submit-parts' || action === 'release-submit-underpack') {
        const url = state.url;
        const calledBy = calledByFromView();
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
        await submitRelease(state, url, body);
        return;
    }
}

// submitRelease posts the prompt's release with every button of the prompt
// disabled. A yes closes the prompt and the modal it came from, and only that
// one: an answer that lands after the modal closed or another opened closes
// nothing. A refusal leaves the prompt on screen, live, as it was.
async function submitRelease(state, url, body) {
    const opening = modalOpening();
    state.posting = true;
    nodeModalContent.querySelectorAll('[data-action]').forEach(function(btn) { btn.disabled = true; });
    const ok = await postAction(url, body, loadViewRef);
    state.posting = false;
    if (releasePromptState !== state) return;
    if (ok) {
        closeReleasePrompt();
        closeModalFrom(opening);
        return;
    }
    state.screen();
}

// closeReleasePrompt drops the prompt; the modal's close calls it, so a
// prompt closed by its backdrop does not hold the board's repaints.
export function closeReleasePrompt() {
    releasePromptState = null;
}

