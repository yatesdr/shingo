var activeLineID = JSON.parse(document.getElementById('page-data').dataset.activeLineId);

// Enable start button when a "To" style is selected
(function() {
    var toSel = document.getElementById('co-to');
    var startBtn = document.getElementById('co-start-btn');
    if (toSel && startBtn) {
        toSel.addEventListener('change', function() {
            startBtn.disabled = !this.value;
        });
    }
})();

// Style the flow chart steps based on current state
(function() {
    var flow = document.querySelector('.co-flow[data-state-index]');
    if (!flow) return;
    var idx = parseInt(flow.dataset.stateIndex);
    var steps = flow.querySelectorAll('.co-step');
    // Map state index ranges to step buttons:
    // Step 0 (Move to Staging):  states 1-2 (stopping, counting_out)
    // Step 1 (Finalize Counts):  state 3 (storing)
    // Step 2 (Move to Line):     state 4 (delivering)
    // Step 3 (Auto Run):         states 5-6 (counting_in, ready)
    var stepRanges = [[1,2],[3,3],[4,4],[5,6]];
    var activeStep = -1;
    for (var i = 0; i < stepRanges.length; i++) {
        if (idx >= stepRanges[i][0] && idx <= stepRanges[i][1]) {
            activeStep = i;
            break;
        }
    }
    for (var i = 0; i < steps.length; i++) {
        if (i < activeStep) {
            steps[i].classList.add('co-done');
        } else if (i === activeStep) {
            steps[i].classList.add('co-active');
        }
        if (i !== activeStep) {
            steps[i].disabled = true;
        }
    }
})();

async function startChangeover() {
    var fromVal = document.getElementById('co-from').value;
    var toVal = document.getElementById('co-to').value;
    if (!toVal) return;
    try {
        await ShingoEdge.api.post('/api/changeover/start', {
            line_id: activeLineID,
            from_job_style: fromVal,
            to_job_style: toVal,
            operator: ''
        });
        ShingoEdge.toast('Changeover started', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function advanceChangeover() {
    try {
        await ShingoEdge.api.post('/api/changeover/advance', {
            line_id: activeLineID,
            operator: ''
        });
        ShingoEdge.toast('Changeover advanced', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function cancelChangeover() {
    var ok = await ShingoEdge.confirm('Cancel this changeover? The line will return to its current state.');
    if (!ok) return;
    try {
        await ShingoEdge.api.post('/api/changeover/cancel', {
            line_id: activeLineID
        });
        ShingoEdge.toast('Changeover cancelled', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

ShingoEdge.createSSE('/events', {
    onChangeoverUpdate: function() { location.reload(); },
    onCounterAnomaly: function() { location.reload(); }
});
