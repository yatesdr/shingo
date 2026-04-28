import { postAction } from './operator-util.js';

const keypadModal = document.getElementById('keypad-modal');
const keypadDisplay = document.getElementById('keypad-display');

let keypadState = null;
let onKeypadOk = null;

export function openKeypad(nodeID, remaining, opts) {
    const initial = remaining > 0 ? String(remaining) : '0';
    keypadState = { nodeID, value: initial };
    onKeypadOk = (opts && opts.onOk) || null;
    keypadDisplay.textContent = initial;
    keypadModal.hidden = false;
}

export function closeKeypad() {
    keypadState = null;
    keypadModal.hidden = true;
}

document.querySelector('.os-keypad-grid').addEventListener('click', evt => {
    const key = evt.target.dataset.key;
    if (!key || !keypadState) return;
    if (key === 'back') {
        keypadState.value = keypadState.value.length > 1 ? keypadState.value.slice(0, -1) : '0';
    } else {
        keypadState.value = keypadState.value === '0' ? key : keypadState.value + key;
    }
    keypadDisplay.textContent = keypadState.value;
});

document.getElementById('keypad-cancel').addEventListener('click', closeKeypad);
document.getElementById('keypad-clear').addEventListener('click', () => {
    if (!keypadState) return;
    keypadState.value = '0';
    keypadDisplay.textContent = '0';
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
