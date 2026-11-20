import { postAction } from './operator-util.js';

const keypadModal = document.getElementById('keypad-modal');
const keypadDisplay = document.getElementById('keypad-display');
const keypadTitle = document.getElementById('keypad-title');
const keypadNote = document.getElementById('keypad-note');

let keypadState = null;
let onKeypadOk = null;
let onKeypadCancel = null;
let keypadNoteFn = null;

// opts.note(value) is the line under the number, redrawn on every key: what
// the typed count makes, or that it is above the pack. It answers a string, or
// {text, tone} with tone 'warn' for a count the box takes but flags; '' or null
// hides the line.
export function openKeypad(nodeID, remaining, opts) {
    opts = opts || {};
    const initial = remaining > 0 ? String(remaining) : '0';
    keypadState = { nodeID, value: initial, dirty: false };
    onKeypadOk = opts.onOk || null;
    onKeypadCancel = opts.onCancel || null;
    keypadNoteFn = typeof opts.note === 'function' ? opts.note : null;
    if (keypadTitle) {
        keypadTitle.textContent = opts.title || 'Enter Remaining Quantity';
    }
    showValue(initial);
    keypadModal.classList.add('active');
}

function showValue(value) {
    keypadDisplay.textContent = value;
    if (!keypadNote) return;
    const got = keypadNoteFn ? keypadNoteFn(parseInt(value || '0', 10)) : null;
    const note = typeof got === 'string' ? { text: got } : (got || { text: '' });
    keypadNote.textContent = note.text || '';
    keypadNote.hidden = !note.text;
    if (note.tone) keypadNote.dataset.tone = note.tone;
    else delete keypadNote.dataset.tone;
}

export function closeKeypad() {
    keypadState = null;
    keypadNoteFn = null;
    keypadModal.classList.remove('active');
}

document.querySelector('.os-keypad-grid').addEventListener('click', evt => {
    const key = evt.target.dataset.key;
    if (!key || !keypadState) return;
    if (key === 'back') {
        keypadState.value = keypadState.value.length > 1 ? keypadState.value.slice(0, -1) : '0';
        keypadState.dirty = true;
    } else if (!keypadState.dirty) {
        // First digit replaces the auto-filled seed value so the operator
        // doesn't have to clear before typing a fresh number.
        keypadState.value = key === '00' ? '0' : key;
        keypadState.dirty = true;
    } else {
        keypadState.value = keypadState.value === '0' ? key : keypadState.value + key;
    }
    showValue(keypadState.value);
});

document.getElementById('keypad-cancel').addEventListener('click', () => {
    const cb = onKeypadCancel;
    closeKeypad();
    if (cb) cb();
});
document.getElementById('keypad-clear').addEventListener('click', () => {
    if (!keypadState) return;
    keypadState.value = '0';
    showValue('0');
});
document.getElementById('keypad-ok').addEventListener('click', async () => {
    if (!keypadState) return;
    const qty = parseInt(keypadState.value || '0', 10);
    const nodeID = keypadState.nodeID;
    const cb = onKeypadOk;
    closeKeypad();
    if (cb) {
        await cb(nodeID, qty);
    } else {
        await postAction('/api/process-nodes/' + nodeID + '/release-partial', { qty });
    }
});

keypadModal.addEventListener('click', evt => {
    if (evt.target === keypadModal) closeKeypad();
});
