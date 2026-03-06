function openPieceCount(id, desc, loc, multiplier, remaining, total) {
    document.getElementById('pc-id').value = id;
    document.getElementById('pc-desc').textContent = desc;
    document.getElementById('pc-loc').textContent = loc;
    document.getElementById('pc-mult').textContent = multiplier;
    document.getElementById('pc-multiplier').value = multiplier;
    document.getElementById('pc-pieces').value = Math.round(remaining * multiplier);
    previewProdUnits();
    ShingoEdge.showModal('piece-count');
}

function previewProdUnits() {
    var pieces = parseFloat(document.getElementById('pc-pieces').value) || 0;
    var mult = parseFloat(document.getElementById('pc-multiplier').value) || 1;
    document.getElementById('pc-preview').textContent = Math.round(pieces / mult);
}

async function submitPieceCount() {
    var id = document.getElementById('pc-id').value;
    var pieces = parseFloat(document.getElementById('pc-pieces').value) || 0;
    try {
        await ShingoEdge.api.put('/api/payloads/' + id + '/count', { piece_count: pieces });
        ShingoEdge.toast('Count updated', 'success');
        ShingoEdge.hideModal('piece-count');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function editReorderPoint(el, id, currentVal) {
    var input = document.createElement('input');
    input.type = 'number';
    input.className = 'inline-edit-input';
    input.value = currentVal;
    input.min = 0;
    input.dataset.id = id;
    input.dataset.original = currentVal;

    el.replaceWith(input);
    input.focus();
    input.select();

    function commit() {
        var newVal = parseInt(input.value);
        if (isNaN(newVal) || newVal < 0) newVal = parseInt(input.dataset.original);
        var span = document.createElement('span');
        span.className = 'cell-clickable';
        span.textContent = newVal;
        span.onclick = function() { editReorderPoint(span, id, newVal); };
        input.replaceWith(span);
        if (newVal !== parseInt(input.dataset.original)) {
            saveReorderPoint(id, newVal);
        }
    }

    input.addEventListener('blur', commit);
    input.addEventListener('keydown', function(e) {
        if (e.key === 'Enter') { e.preventDefault(); input.blur(); }
        if (e.key === 'Escape') { input.value = input.dataset.original; input.blur(); }
    });
}

async function saveReorderPoint(id, val) {
    try {
        await ShingoEdge.api.put('/api/payloads/' + id + '/reorder-point', { reorder_point: val });
        ShingoEdge.toast('Reorder point updated', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function toggleAutoReorder(id, enabled) {
    try {
        await ShingoEdge.api.put('/api/payloads/' + id + '/auto-reorder', { enabled: enabled });
        ShingoEdge.toast('Auto-reorder ' + (enabled ? 'enabled' : 'disabled'), 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// SSE: real-time payload updates
ShingoEdge.createSSE('/events', {
    onPayloadUpdate: function(data) {
        var row = document.querySelector('tr[data-payload-id="' + data.payload_id + '"]');
        if (!row) return;
        var remainCell = row.querySelector('.payload-remaining');
        if (remainCell) remainCell.textContent = data.new_remaining;
        location.reload();
    },
    onCounterAnomaly: function() { location.reload(); },
    onOrderUpdate: function() {},
    onPayloadReorder: function() {}
});
