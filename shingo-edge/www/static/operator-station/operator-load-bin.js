import { esc, postAction, showToast } from './operator-util.js';

let loadBinState = null;
let loadViewRef = null;

export function setLoadView(fn) { loadViewRef = fn; }

export function openLoadBin(nodeID, allowedCodes, defaultCapacity) {
    loadBinState = { nodeID, payloadCode: '' };
    const payloadEl = document.getElementById('load-bin-payload');
    payloadEl.innerHTML = '';
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Select a payload above</div>';
    (allowedCodes || []).forEach(code => {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'os-action-btn';
        btn.style.cssText = 'font-size:14px;padding:10px 20px;margin:0 6px 6px 0;background:var(--os-gray)';
        btn.textContent = code;
        btn.dataset.code = code;
        btn.addEventListener('click', () => selectLoadPayload(code));
        payloadEl.appendChild(btn);
    });
    document.getElementById('load-bin-modal').hidden = false;
    if (allowedCodes.length === 1) {
        selectLoadPayload(allowedCodes[0]);
    }
}

async function selectLoadPayload(code) {
    if (!loadBinState) return;
    loadBinState.payloadCode = code;
    document.querySelectorAll('#load-bin-payload button').forEach(btn => {
        btn.className = 'os-action-btn' + (btn.dataset.code === code ? ' request' : '');
    });
    const rows = document.getElementById('load-bin-rows');
    rows.innerHTML = '<div style="color:#999;text-align:center;padding:12px">Loading manifest...</div>';
    try {
        const res = await fetch('/api/payload/' + encodeURIComponent(code) + '/manifest');
        const data = res.ok ? await res.json() : { uop_capacity: 0, items: [] };
        const items = data.items || [];
        const uopCapacity = data.uop_capacity || 0;
        rows.innerHTML = '';
        if (items.length === 0) {
            rows.innerHTML = '<div style="color:#f66;padding:8px">No manifest template for this payload</div>';
            return;
        }
        const uopRow = document.createElement('div');
        uopRow.style.cssText = 'display:grid;grid-template-columns:1fr 100px;gap:8px;align-items:center;margin-bottom:12px;padding:10px;background:#1a2a1a;border-radius:4px;border:1px solid #2a4a2a';
        uopRow.innerHTML =
            '<div style="font-size:16px;font-weight:600;color:#fff">UOP Count</div>' +
            '<input type="number" id="os-load-uop" value="' + uopCapacity + '" ' +
                'style="width:100%;font-size:18px;padding:8px;border:1px solid #444;border-radius:4px;background:#222;color:#fff;text-align:center;font-weight:600">';
        rows.appendChild(uopRow);

        items.forEach(item => {
            const row = document.createElement('div');
            row.style.cssText = 'display:grid;grid-template-columns:1fr 80px;gap:8px;align-items:center;margin-bottom:8px;padding:8px;background:#1a1a1a;border-radius:4px';
            row.innerHTML =
                '<div>' +
                    '<div style="font-size:15px;color:#fff">' + esc(item.part_number) + '</div>' +
                    '<div style="font-size:12px;color:#999">' + esc(item.description || '') + '</div>' +
                '</div>' +
                '<input type="number" class="os-manifest-qty" value="' + (item.quantity || 0) + '" ' +
                    'data-part="' + esc(item.part_number) + '" data-desc="' + esc(item.description || '') + '" ' +
                    'style="width:100%;font-size:18px;padding:8px;border:1px solid #444;border-radius:4px;background:#222;color:#fff;text-align:center">';
            rows.appendChild(row);
        });
    } catch (err) {
        console.error('selectLoadPayload manifest fetch', err);
        rows.innerHTML = '<div style="color:#f66;padding:8px">Failed to load manifest</div>';
    }
}

function closeLoadBin() {
    loadBinState = null;
    document.getElementById('load-bin-modal').hidden = true;
}

async function submitLoadBin() {
    if (!loadBinState || !loadBinState.payloadCode) {
        showToast('Select a payload first', 'error');
        return;
    }
    const manifest = [];
    document.querySelectorAll('.os-manifest-qty').forEach(input => {
        const qty = parseInt(input.value, 10) || 0;
        if (qty > 0) {
            manifest.push({
                part_number: input.dataset.part,
                quantity: qty,
                description: input.dataset.desc || ''
            });
        }
    });
    if (manifest.length === 0) {
        showToast('Enter at least one quantity', 'error');
        return;
    }
    const uopEl = document.getElementById('os-load-uop');
    const uopCount = uopEl ? parseInt(uopEl.value, 10) || 0 : 0;
    const body = { payload_code: loadBinState.payloadCode, uop_count: uopCount, manifest };
    const nodeID = loadBinState.nodeID;
    closeLoadBin();
    const ok = await postAction('/api/process-nodes/' + nodeID + '/load-bin', body, loadViewRef);
    if (ok) showToast('Bin loaded', 'success');
}

document.getElementById('load-bin-cancel').addEventListener('click', closeLoadBin);
document.getElementById('load-bin-submit').addEventListener('click', submitLoadBin);
document.getElementById('load-bin-clear').addEventListener('click', async () => {
    if (!loadBinState) return;
    const nodeID = loadBinState.nodeID;
    closeLoadBin();
    const ok = await postAction('/api/process-nodes/' + nodeID + '/clear-bin', undefined, loadViewRef);
    if (ok) showToast('Bin cleared', 'success');
});
document.getElementById('load-bin-modal').addEventListener('click', evt => {
    if (evt.target === document.getElementById('load-bin-modal')) closeLoadBin();
});
