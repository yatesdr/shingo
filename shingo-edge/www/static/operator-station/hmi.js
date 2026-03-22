const root = document.getElementById('hmi-root');
const stationID = parseInt(root.dataset.stationId, 10);
const canvas = document.getElementById('hmi-canvas');
const ctx = canvas.getContext('2d');
const overlay = document.getElementById('keypad-overlay');
const keypadDisplay = document.getElementById('keypad-display');
const styleOverlay = document.getElementById('style-overlay');
const stylePickerGrid = document.getElementById('style-picker-grid');

let stationView = null;
let hitZones = [];
let keypadState = null;

function fitCanvas() {
    const ratio = window.devicePixelRatio || 1;
    const w = root.clientWidth;
    const h = root.clientHeight;
    canvas.width = Math.floor(w * ratio);
    canvas.height = Math.floor(h * ratio);
    canvas.style.width = w + 'px';
    canvas.style.height = h + 'px';
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    render();
}

async function loadView() {
    const res = await fetch('/api/operator-stations/' + stationID + '/view');
    if (!res.ok) throw new Error(await res.text());
    stationView = await res.json();
    render();
}

function render() {
    const w = root.clientWidth;
    const h = root.clientHeight;
    ctx.clearRect(0, 0, w, h);
    ctx.fillStyle = '#101418';
    ctx.fillRect(0, 0, w, h);
    hitZones = [];

    if (!stationView) {
        drawCentered('Loading station...', w, h);
        return;
    }

    drawHeader(w);
    drawFooter(w, h);

    const cards = stationView.nodes || [];
    if (!cards.length) {
        drawCentered('No operator station nodes configured', w, h);
        return;
    }

    const top = 104;
    const bottom = 62;
    const gap = 14;
    const cols = w > 1100 ? 2 : 1;
    const rows = Math.ceil(cards.length / cols);
    const cardW = (w - gap * (cols + 1)) / cols;
    const cardH = (h - top - bottom - gap * (rows + 1)) / rows;

    cards.forEach((entry, i) => {
        const col = i % cols;
        const row = Math.floor(i / cols);
        const x = gap + col * (cardW + gap);
        const y = top + gap + row * (cardH + gap);
        drawCard(entry, x, y, cardW, cardH);
    });
}

function drawHeader(w) {
    ctx.fillStyle = '#1e2833';
    ctx.fillRect(0, 0, w, 92);
    ctx.fillStyle = '#fff';
    ctx.font = 'bold 28px Arial';
    ctx.fillText(stationView.station.name, 18, 32);

    ctx.font = '18px Arial';
    ctx.fillStyle = '#b8c3cf';
    ctx.fillText(stationView.process.name + (stationView.station.hierarchy_path ? ' / ' + stationView.station.hierarchy_path : ''), 18, 56);
    const current = stationView.current_style ? stationView.current_style.name : 'No Style';
    const target = stationView.target_style ? (' -> ' + stationView.target_style.name) : '';
    ctx.fillText(current + target, 18, 80);

    ctx.textAlign = 'right';
    ctx.fillStyle = stationView.station.health_status === 'online' ? '#6fda8b' : '#ff9b6b';
    ctx.fillText((stationView.station.health_status || 'offline').toUpperCase(), w - 18, 32);
    ctx.textAlign = 'left';

    let x = w - 336;
    if (!stationView.active_changeover) {
        hitZones.push(drawButton(x, 46, 148, 32, '#2f6f4a', 'START CO', openStylePicker, 15));
        x += 160;
    } else {
        hitZones.push(drawButton(x, 46, 148, 32, nextPhaseColor(), nextPhaseLabel(), advanceChangeoverPhase, 14));
        x += 160;
        if (needsProductionCutover()) {
            hitZones.push(drawButton(x, 46, 148, 32, '#7a4e26', 'START STYLE', completeProductionCutover, 14));
            x += 160;
        }
    }
    if (stationView.active_changeover && stationView.station_task && stationView.station_task.ready_for_local_change) {
        hitZones.push(drawButton(x, 46, 148, 32, '#7a4e26', 'COMPLETE CO', completeStationChangeover, 15));
        x += 160;
    }
    hitZones.push(drawButton(x, 46, 148, 32, '#2d4f7d', 'REFRESH', loadView, 15));
}

function drawFooter(w, h) {
    ctx.fillStyle = '#16202a';
    ctx.fillRect(0, h - 48, w, 48);
    ctx.fillStyle = '#d6dde5';
    ctx.font = '18px Arial';
    const co = stationView.active_changeover;
    const total = stationView.completed_node_changes + stationView.pending_node_changes;
    const progress = co ? (' [' + stationView.completed_node_changes + '/' + total + ' nodes]') : '';
    const prod = stationView.process && stationView.process.production_state ? (' | ' + stationView.process.production_state.replace(/_/g, ' ')) : '';
    const msg = co ? ('Changeover ' + co.phase.toUpperCase() + ': ' + co.from_style_name + ' -> ' + co.to_style_name + progress + prod) : ('Touch-ready operator station' + prod);
    ctx.fillText(msg, 18, h - 18);
}

function drawCentered(text, w, h) {
    ctx.fillStyle = '#fff';
    ctx.font = 'bold 28px Arial';
    ctx.textAlign = 'center';
    ctx.fillText(text, w / 2, h / 2);
    ctx.textAlign = 'left';
}

function drawCard(entry, x, y, w, h) {
    const runtime = entry.runtime || {};
    const assignment = entry.assignment || {};
    const nextStyle = entry.next_style || {};
    const status = runtime.material_status || 'empty';

    ctx.fillStyle = '#1b232d';
    roundRect(ctx, x, y, w, h, 18);
    ctx.fill();
    ctx.strokeStyle = status === 'replenishing' ? '#efb24d' : status === 'empty' ? '#cf5c5c' : '#314153';
    ctx.lineWidth = 3;
    ctx.stroke();

    ctx.fillStyle = '#fff';
    ctx.font = 'bold 24px Arial';
    ctx.fillText(entry.node.name, x + 18, y + 34);

    ctx.font = '18px Arial';
    ctx.fillStyle = '#b8c3cf';
    ctx.fillText((assignment.payload_description || assignment.payload_code || 'Unassigned'), x + 18, y + 66);
    ctx.fillText('Status: ' + status, x + 18, y + 94);
    ctx.fillText('Remaining: ' + (runtime.remaining_uop != null ? runtime.remaining_uop : '--'), x + 18, y + 120);
    ctx.fillText('Manifest: ' + (runtime.manifest_status || 'unknown'), x + 18, y + 146);
    if (stationView.target_style && (nextStyle.payload_description || nextStyle.payload_code)) {
        ctx.fillText('Next: ' + (nextStyle.payload_description || nextStyle.payload_code), x + 18, y + 172);
    }
    if (entry.changeover_task) {
        ctx.fillText('CO Task: ' + entry.changeover_task.state, x + 18, y + 198);
    }

    const buttons = [];
    buttons.push(drawButton(x + 18, y + h - 138, w - 36, 46, '#2970d6', 'REQUEST', () => requestMaterial(entry.node.id), 20));
    buttons.push(drawButton(x + 18, y + h - 84, (w - 50) / 2, 44, '#855c22', 'RELEASE EMPTY', () => releaseEmpty(entry.node.id), 18));
    buttons.push(drawButton(x + 32 + (w - 50) / 2, y + h - 84, (w - 50) / 2, 44, '#6d4a7a', 'RELEASE PARTIAL', () => openPartialKeypad(entry.node.id), 17));

    if (runtime.manifest_status !== 'confirmed') {
        buttons.push(drawButton(x + 18, y + h - 192, w - 36, 42, '#207f57', 'CONFIRM MANIFEST', () => confirmManifest(entry.node.id), 18));
    } else if (stationView.active_changeover && stationView.active_changeover.phase === 'runout' && (nextStyle.payload_code || nextStyle.payload_description)) {
        buttons.push(drawButton(x + 18, y + h - 192, w - 36, 42, '#2f6f4a', 'STAGE NEXT MATERIAL', () => stageNode(entry.node.id), 17));
    } else if (stationView.active_changeover && stationView.active_changeover.phase === 'tool_change') {
        buttons.push(drawButton(x + 18, y + h - 192, w - 36, 42, '#855c22', 'EMPTY LINE FOR TOOLS', () => emptyForToolChange(entry.node.id), 17));
    } else if (stationView.active_changeover && (stationView.active_changeover.phase === 'release' || stationView.active_changeover.phase === 'cutover' || stationView.active_changeover.phase === 'verify') && (nextStyle.payload_code || nextStyle.payload_description)) {
        buttons.push(drawButton(x + 18, y + h - 192, w - 36, 42, '#7a4e26', 'RELEASE NEW MATERIAL', () => releaseToProduction(entry.node.id), 17));
    } else if (stationView.target_style && (nextStyle.payload_code || nextStyle.payload_description)) {
        buttons.push(drawButton(x + 18, y + h - 192, w - 36, 42, '#7a4e26', 'SWITCH TO NEXT STYLE', () => switchNode(entry.node.id), 18));
    }

    hitZones.push(...buttons);
}

function drawButton(x, y, w, h, color, label, onClick, fontSize) {
    ctx.fillStyle = color;
    roundRect(ctx, x, y, w, h, 12);
    ctx.fill();
    ctx.fillStyle = '#fff';
    ctx.font = 'bold ' + (fontSize || 20) + 'px Arial';
    ctx.textAlign = 'center';
    ctx.fillText(label, x + w / 2, y + Math.min(h - 14, 29));
    ctx.textAlign = 'left';
    return { x, y, w, h, onClick };
}

function roundRect(context, x, y, w, h, r) {
    context.beginPath();
    context.moveTo(x + r, y);
    context.arcTo(x + w, y, x + w, y + h, r);
    context.arcTo(x + w, y + h, x, y + h, r);
    context.arcTo(x, y + h, x, y, r);
    context.arcTo(x, y, x + w, y, r);
    context.closePath();
}

canvas.addEventListener('click', async (evt) => {
    const rect = canvas.getBoundingClientRect();
    const x = evt.clientX - rect.left;
    const y = evt.clientY - rect.top;
    for (const zone of hitZones) {
        if (x >= zone.x && x <= zone.x + zone.w && y >= zone.y && y <= zone.y + zone.h) {
            await zone.onClick();
            return;
        }
    }
});

async function requestMaterial(nodeID) {
    await postAction('/api/op-nodes/' + nodeID + '/request', {});
}

async function releaseEmpty(nodeID) {
    await postAction('/api/op-nodes/' + nodeID + '/release-empty', {});
}

function openPartialKeypad(nodeID) {
    keypadState = { nodeID, value: '0' };
    keypadDisplay.textContent = '0';
    overlay.style.display = 'flex';
}

async function confirmManifest(nodeID) {
    await postAction('/api/op-nodes/' + nodeID + '/manifest/confirm', {});
}

async function switchNode(nodeID) {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/switch-node/' + nodeID, {});
}

async function stageNode(nodeID) {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/stage-node/' + nodeID, {});
}

async function emptyForToolChange(nodeID) {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/empty-node/' + nodeID, {});
}

async function releaseToProduction(nodeID) {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/release-node/' + nodeID, {});
}

function openStylePicker() {
    if (!stationView || stationView.active_changeover) return;
    stylePickerGrid.innerHTML = '';
    const styles = (stationView.available_styles || []).filter((style) => !stationView.current_style || style.id !== stationView.current_style.id);
    for (const style of styles) {
        const button = document.createElement('button');
        button.textContent = style.name;
        button.onclick = async () => {
            await postAction('/api/processes/' + stationView.process.id + '/changeover/start', {
                to_style_id: style.id,
                called_by: stationView.station.code || stationView.station.name,
                notes: 'Started from operator station HMI'
            });
            closeStylePicker();
        };
        stylePickerGrid.appendChild(button);
    }
    styleOverlay.style.display = 'flex';
}

function closeStylePicker() {
    styleOverlay.style.display = 'none';
}

function nextPhaseLabel() {
    const next = nextPhaseStep();
    if (!next) return 'COMPLETE';
    return 'TO ' + String(next.label || next.kind || 'NEXT').toUpperCase();
}

function nextPhaseColor() {
    if (!stationView || !stationView.active_changeover) return '#6b4a1f';
    const phase = stationView.active_changeover.phase;
    if (phase === 'runout') return '#6b4a1f';
    if (phase === 'tool_change') return '#7a5a22';
    if (phase === 'release') return '#2f6f4a';
    if (phase === 'cutover') return '#7a4e26';
    return '#2d4f7d';
}

async function advanceChangeoverPhase() {
    const next = nextPhaseStep();
    if (!next) return;
    await postAction('/api/processes/' + stationView.process.id + '/changeover/phase', { phase: next.kind });
}

async function completeStationChangeover() {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/switch-station/' + stationID, {});
}

async function completeProductionCutover() {
    await postAction('/api/processes/' + stationView.process.id + '/changeover/cutover', {});
}

function nextPhaseStep() {
    if (!stationView || !stationView.active_changeover) return null;
    const flow = Array.isArray(stationView.process.changeover_flow) ? stationView.process.changeover_flow : [];
    for (let i = 0; i < flow.length; i++) {
        if (flow[i].kind === stationView.active_changeover.phase && i + 1 < flow.length) {
            return flow[i + 1];
        }
    }
    return null;
}

function needsProductionCutover() {
    if (!stationView || !stationView.active_changeover || !stationView.target_style) return false;
    const phase = stationView.active_changeover.phase;
    if (phase !== 'cutover' && phase !== 'verify') return false;
    return !stationView.current_style || stationView.current_style.id !== stationView.target_style.id;
}

async function postAction(url, body) {
    const res = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    });
    if (!res.ok) {
        const err = await res.text();
        alert(err);
        return;
    }
    await loadView();
}

window.OperatorStationHMI = {
    pressKey(value) {
        if (!keypadState) return;
        keypadState.value = keypadState.value === '0' ? value : keypadState.value + value;
        keypadDisplay.textContent = keypadState.value;
    },
    backspace() {
        if (!keypadState) return;
        keypadState.value = keypadState.value.length > 1 ? keypadState.value.slice(0, -1) : '0';
        keypadDisplay.textContent = keypadState.value;
    },
    clearKeypad() {
        if (!keypadState) return;
        keypadState.value = '0';
        keypadDisplay.textContent = keypadState.value;
    },
    cancelKeypad() {
        keypadState = null;
        overlay.style.display = 'none';
    },
    closeStylePicker,
    async submitKeypad() {
        if (!keypadState) return;
        const qty = parseInt(keypadState.value || '0', 10);
        const nodeID = keypadState.nodeID;
        keypadState = null;
        overlay.style.display = 'none';
        await postAction('/api/op-nodes/' + nodeID + '/release-partial', { qty });
    }
};

window.addEventListener('resize', fitCanvas);

loadView().then(fitCanvas);
setInterval(loadView, 5000);
