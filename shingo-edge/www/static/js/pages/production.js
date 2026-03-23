var _pd = document.getElementById('page-data').dataset;
var _shifts = JSON.parse(_pd.shifts);
var _hourlyCounts = JSON.parse(_pd.hourlyCounts);
var _styles = JSON.parse(_pd.styles);
var _activeLineID = JSON.parse(_pd.activeLineId);
var _filterStyleID = JSON.parse(_pd.filterStyleId);
var _currentDate = _pd.date;

function parseHHMM(s) {
    var parts = s.split(':');
    return parseInt(parts[0]) * 60 + parseInt(parts[1] || '0');
}

function shiftClockHours(shift) {
    var startMin = parseHHMM(shift.start_time);
    var endMin = parseHHMM(shift.end_time);
    var startHour = Math.floor(startMin / 60);
    var endHour = Math.floor((endMin - 1) / 60);
    if (endMin % 60 === 0) endHour = (endMin / 60) - 1;
    if (endMin <= startMin) endHour += 24; // wraps midnight

    var hours = [];
    for (var h = startHour; h <= endHour; h++) {
        hours.push(h % 24);
    }
    return hours;
}

function renderProductionTable() {
    if (!_shifts || _shifts.length === 0) return;

    // Compute max columns
    var maxCols = 0;
    var shiftHours = [];
    for (var i = 0; i < _shifts.length; i++) {
        var hours = shiftClockHours(_shifts[i]);
        shiftHours.push(hours);
        if (hours.length > maxCols) maxCols = hours.length;
    }

    // Header
    var thead = document.getElementById('production-thead');
    var hdr = '<tr><th style="min-width:120px">Shift</th>';
    for (var c = 0; c < maxCols; c++) {
        hdr += '<th style="text-align:center;min-width:60px">Hr ' + (c + 1) + '</th>';
    }
    hdr += '<th style="text-align:center;min-width:70px">Total</th></tr>';
    thead.innerHTML = hdr;

    // Body
    var tbody = document.getElementById('production-tbody');
    var html = '';
    for (var i = 0; i < _shifts.length; i++) {
        var shift = _shifts[i];
        var hours = shiftHours[i];
        var total = 0;
        var label = shift.name || ('Shift ' + shift.shift_number);
        var timeRange = shift.start_time + ' - ' + shift.end_time;

        html += '<tr>';
        html += '<td><strong>' + ShingoEdge.escapeHtml(label) + '</strong><br><span style="font-size:0.8rem;color:var(--text-muted)">' + ShingoEdge.escapeHtml(timeRange) + '</span></td>';

        for (var c = 0; c < maxCols; c++) {
            if (c < hours.length) {
                var h = hours[c];
                var count = _hourlyCounts[h] || 0;
                total += count;
                var hourLabel = String(h).padStart(2, '0') + ':00';
                html += '<td style="text-align:center" data-hour="' + h + '">';
                html += '<div class="prod-count" id="hc-' + h + '">' + count + '</div>';
                html += '<div style="font-size:0.7rem;color:var(--text-muted)">' + hourLabel + '</div>';
                html += '</td>';
            } else {
                html += '<td></td>';
            }
        }

        html += '<td style="text-align:center;font-weight:600" id="shift-total-' + shift.shift_number + '">' + total + '</td>';
        html += '</tr>';
    }

    // Grand total row
    var grandTotal = 0;
    for (var h in _hourlyCounts) {
        grandTotal += _hourlyCounts[h];
    }
    html += '<tr style="border-top:2px solid var(--border)">';
    html += '<td><strong>Total</strong></td>';
    for (var c = 0; c < maxCols; c++) { html += '<td></td>'; }
    html += '<td style="text-align:center;font-weight:700" id="grand-total">' + grandTotal + '</td>';
    html += '</tr>';

    tbody.innerHTML = html;
}

// --- Style cycling: All -> style1 -> style2 -> ... -> All ---
function cycleStyle() {
    if (!_styles || _styles.length === 0) return;
    var currentIdx = -1; // -1 = "All"
    if (_filterStyleID) {
        for (var i = 0; i < _styles.length; i++) {
            if (_styles[i].id === _filterStyleID) { currentIdx = i; break; }
        }
    }
    var nextIdx = currentIdx + 1;
    var nextStyle = 'all';
    if (nextIdx < _styles.length) {
        nextStyle = _styles[nextIdx].id;
    }
    navigateWithStyle(nextStyle);
}

function navigateWithStyle(style) {
    var lineID = document.getElementById('prod-line-select').value;
    var date = document.getElementById('prod-date').value;
    window.location = '/production?process=' + lineID + '&date=' + date + '&style=' + style;
}

function buildUrl(lineID, date) {
    var url = '/production?process=' + lineID + '&date=' + date;
    if (_filterStyleID) url += '&style=' + _filterStyleID;
    return url;
}

function onProdLineChange() {
    var lineID = document.getElementById('prod-line-select').value;
    var date = document.getElementById('prod-date').value;
    // Reset to "all" when switching lines since styles differ
    window.location = '/production?process=' + lineID + '&date=' + date;
}

function onProdDateChange() {
    var lineID = document.getElementById('prod-line-select').value;
    var date = document.getElementById('prod-date').value;
    window.location = buildUrl(lineID, date);
}

function changeDate(offset) {
    var d = new Date(_currentDate + 'T00:00:00');
    d.setDate(d.getDate() + offset);
    var yyyy = d.getFullYear();
    var mm = String(d.getMonth() + 1).padStart(2, '0');
    var dd = String(d.getDate()).padStart(2, '0');
    var lineID = document.getElementById('prod-line-select').value;
    window.location = buildUrl(lineID, yyyy + '-' + mm + '-' + dd);
}

// Initial render
renderProductionTable();

// SSE: real-time counter updates
ShingoEdge.createSSE('/events', {
    onCounterUpdate: function(data) {
        if (data.process_id !== _activeLineID) return;
        // If filtering by style, skip deltas from other styles
        if (_filterStyleID && data.style_id !== _filterStyleID) return;

        var today = new Date();
        var todayStr = today.getFullYear() + '-' + String(today.getMonth()+1).padStart(2,'0') + '-' + String(today.getDate()).padStart(2,'0');
        if (_currentDate !== todayStr) return;

        var hour = today.getHours();
        var delta = data.delta || 0;
        if (!_hourlyCounts[hour]) _hourlyCounts[hour] = 0;
        _hourlyCounts[hour] += delta;

        var cell = document.getElementById('hc-' + hour);
        if (cell) cell.textContent = _hourlyCounts[hour];

        recalcTotals();
    }
});

function recalcTotals() {
    if (!_shifts) return;
    var grandTotal = 0;
    for (var i = 0; i < _shifts.length; i++) {
        var hours = shiftClockHours(_shifts[i]);
        var total = 0;
        for (var c = 0; c < hours.length; c++) {
            total += _hourlyCounts[hours[c]] || 0;
        }
        var el = document.getElementById('shift-total-' + _shifts[i].shift_number);
        if (el) el.textContent = total;
        grandTotal += total;
    }
    var gt = document.getElementById('grand-total');
    if (gt) gt.textContent = grandTotal;
}
