import { esc, postAction, showToast } from './operator-util.js';
import { openKeypad } from './operator-keypad.js';

let loadBinState = null;
let loadViewRef = null;

export function setLoadView(fn) { loadViewRef = fn; }

export function openLoadBin(nodeID, allowedCodes, defaultCapacity) {
    loadBinState = {
        nodeID: nodeID,
        payloadCode: '',
        uopCount: 0,
        manifest: [],
        submitting: false,
    };
    const payloadEl = document.getElementById('load-bin-payload');
    payloadEl.innerHTML = '';
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Select a payload above</div>';
    (allowedCodes || []).forEach(function(code) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'os-action-btn';
        btn.style.cssText = 'font-size:14px;padding:10px 20px;margin:0 6px 6px 0;background:var(--os-gray)';
        btn.textContent = code;
        btn.dataset.code = code;
        btn.addEventListener('click', function() { selectLoadPayload(code); });
        payloadEl.appendChild(btn);
    });
    setSubmittingUI(false);
    document.getElementById('load-bin-modal').hidden = false;
    if (allowedCodes.length === 1) {
        selectLoadPayload(allowedCodes[0]);
    }
}

async function selectLoadPayload(code) {
    if (!loadBinState) return;
    loadBinState.payloadCode = code;
    document.querySelectorAll('#load-bin-payload button').forEach(function(btn) {
        btn.className = 'os-action-btn' + (btn.dataset.code === code ? ' request' : '');
    });
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Loading manifest...</div>';
    try {
        const res = await fetch('/api/payload/' + encodeURIComponent(code) + '/manifest');
        const data = res.ok ? await res.json() : { uop_capacity: 0, items: [] };
        const items = data.items || [];
        loadBinState.uopCount = data.uop_capacity || 0;
        // Capture the canonical manifest from the payload template — operators
        // shouldn't be redefining it on the fly. They see the line items so
        // they can verify the bin matches the template, but the values are
        // sent back to the server unchanged.
        loadBinState.manifest = items.map(function(item) {
            return {
                part_number: item.part_number,
                quantity: item.quantity || 0,
                description: item.description || '',
            };
        });
        if (items.length === 0) {
            rows.innerHTML = '<div style="color:#f66;padding:8px">No manifest template for this payload</div>';
            return;
        }
        renderRows();
    } catch (err) {
        console.error('selectLoadPayload manifest fetch', err);
        rows.innerHTML = '<div style="color:#f66;padding:8px">Failed to load manifest</div>';
    }
}

function renderRows() {
    const state = loadBinState;
    if (!state) return;
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '';

    // UOP Count — tappable display that opens the numeric keypad. Replaces
    // the up/down number-input arrows that were too small for the touchscreen.
    const uopRow = document.createElement('div');
    uopRow.style.cssText = 'display:grid;grid-template-columns:1fr auto;gap:12px;align-items:center;margin-bottom:12px;padding:10px;background:#1a2a1a;border-radius:6px;border:1px solid #2a4a2a';
    uopRow.innerHTML =
        '<div style="font-size:16px;font-weight:600;color:#fff">UOP Count</div>' +
        '<button type="button" id="os-load-uop-display" ' +
            'style="min-width:120px;background:#0f141a;border:1px solid var(--os-border);' +
            'border-radius:8px;padding:10px 16px;color:#fff;font-size:24px;font-weight:700;' +
            'cursor:pointer;touch-action:manipulation">' +
            state.uopCount + '</button>';
    rows.appendChild(uopRow);
    document.getElementById('os-load-uop-display').addEventListener('click', openUopKeypad);

    // Manifest line items — display-only. Operators can see what the payload
    // template expects (part numbers + qty per UOP) but can't edit; the values
    // come from the catalog and sending operator-edited counts here would let
    // a typo silently corrupt the bin's manifest.
    state.manifest.forEach(function(item) {
        const row = document.createElement('div');
        row.style.cssText = 'display:grid;grid-template-columns:1fr auto;gap:12px;align-items:center;margin-bottom:8px;padding:10px;background:#1a1a1a;border-radius:6px';
        row.innerHTML =
            '<div>' +
                '<div style="font-size:15px;color:#fff">' + esc(item.part_number) + '</div>' +
                (item.description
                    ? '<div style="font-size:12px;color:#999">' + esc(item.description) + '</div>'
                    : '') +
            '</div>' +
            '<div style="min-width:60px;text-align:right;color:#cfd;font-size:18px;font-weight:600">' +
                item.quantity + '</div>';
        rows.appendChild(row);
    });
}

function openUopKeypad() {
    const state = loadBinState;
    if (!state || state.submitting) return;
    openKeypad(0, state.uopCount, {
        title: 'UOP Count',
        onOk: function(_nodeID, qty) {
            state.uopCount = qty > 0 ? qty : 0;
            renderRows();
        },
    });
}

function closeLoadBin() {
    loadBinState = null;
    document.getElementById('load-bin-modal').hidden = true;
}

function setSubmittingUI(submitting) {
    if (loadBinState) loadBinState.submitting = submitting;
    const submit = document.getElementById('load-bin-submit');
    const cancel = document.getElementById('load-bin-cancel');
    if (submit) {
        submit.disabled = submitting;
        submit.textContent = submitting ? 'LOADING...' : 'CONFIRM LOAD';
        submit.style.opacity = submitting ? '0.6' : '';
    }
    if (cancel) cancel.disabled = submitting;
    const display = document.getElementById('os-load-uop-display');
    if (display) display.disabled = submitting;
}

async function submitLoadBin() {
    const state = loadBinState;
    if (!state || state.submitting) return;
    if (!state.payloadCode) {
        showToast('Select a payload first', 'error');
        return;
    }
    if (state.manifest.length === 0) {
        showToast('No manifest loaded', 'error');
        return;
    }
    if (state.uopCount <= 0) {
        showToast('Set UOP Count first', 'error');
        return;
    }
    const body = {
        payload_code: state.payloadCode,
        uop_count: state.uopCount,
        manifest: state.manifest,
    };
    const nodeID = state.nodeID;
    setSubmittingUI(true);
    const ok = await postAction('/api/process-nodes/' + nodeID + '/load-bin', body, loadViewRef);
    if (ok) {
        showToast('Bin loaded', 'success');
        closeLoadBin();
    } else {
        // postAction already toasted the server error — re-enable so the
        // operator can correct UOP and retry without dismissing the modal.
        setSubmittingUI(false);
    }
}

document.getElementById('load-bin-cancel').addEventListener('click', function() {
    if (loadBinState && loadBinState.submitting) return;
    closeLoadBin();
});
document.getElementById('load-bin-submit').addEventListener('click', submitLoadBin);

// CLEAR BIN was hidden in the template — its destructive semantics overlap
// the release prompt's "NOTHING PULLED" path and presenting it next to
// CONFIRM LOAD invited misclicks. The handler stays wired for any future
// re-introduction (a deliberate "wipe bin" surface), but no UI exposes it.
const clearBtn = document.getElementById('load-bin-clear');
if (clearBtn) {
    clearBtn.addEventListener('click', async function() {
        if (!loadBinState || loadBinState.submitting) return;
        const nodeID = loadBinState.nodeID;
        setSubmittingUI(true);
        const ok = await postAction('/api/process-nodes/' + nodeID + '/clear-bin', undefined, loadViewRef);
        if (ok) {
            showToast('Bin cleared', 'success');
            closeLoadBin();
        } else {
            setSubmittingUI(false);
        }
    });
}

document.getElementById('load-bin-modal').addEventListener('click', function(evt) {
    if (loadBinState && loadBinState.submitting) return;
    if (evt.target === document.getElementById('load-bin-modal')) closeLoadBin();
});
