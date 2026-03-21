// --- Shifts ---
(function loadShifts() {
    var shifts = JSON.parse(document.getElementById('page-data').dataset.shifts);
    for (var i = 0; i < shifts.length; i++) {
        var s = shifts[i];
        var nameEl = document.querySelector('.shift-name[data-shift="' + s.shift_number + '"]');
        var startEl = document.querySelector('.shift-start[data-shift="' + s.shift_number + '"]');
        var endEl = document.querySelector('.shift-end[data-shift="' + s.shift_number + '"]');
        if (nameEl) nameEl.value = s.name;
        if (startEl) startEl.value = s.start_time;
        if (endEl) endEl.value = s.end_time;
    }
})();

async function saveShifts() {
    var shifts = [];
    for (var n = 1; n <= 3; n++) {
        var name = document.querySelector('.shift-name[data-shift="' + n + '"]').value.trim();
        var start = document.querySelector('.shift-start[data-shift="' + n + '"]').value;
        var end = document.querySelector('.shift-end[data-shift="' + n + '"]').value;
        shifts.push({ shift_number: n, name: name, start_time: start, end_time: end });
    }
    try {
        await ShingoEdge.api.put('/api/shifts', shifts);
        ShingoEdge.toast('Shifts saved', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Backups ---
var _backupUI = {
    connection: { state: 'idle', message: 'Connection test not yet run in this session.' },
    operation: { state: 'idle', message: 'No manual backup operation running.' },
    testedFingerprint: '',
    stationID: ''
};

function setButtonBusy(id, busy, idleText, busyText) {
    var btn = document.getElementById(id);
    if (!btn) return;
    btn.disabled = !!busy;
    btn.textContent = busy ? busyText : idleText;
}

function setBackupConnection(state, message) {
    _backupUI.connection.state = state;
    _backupUI.connection.message = message;
    renderBackupConnectionStatus();
}

function setBackupOperation(state, message) {
    _backupUI.operation.state = state;
    _backupUI.operation.message = message;
    renderBackupOperationStatus();
}

function renderBackupConnectionStatus() {
    var el = document.getElementById('backup-connection-status');
    if (!el) return;
    var badge = '';
    if (_backupUI.connection.state === 'ok') {
        badge = '<span class="status-badge status-connected" style="margin-right:0.5rem">Connected</span>';
    } else if (_backupUI.connection.state === 'error') {
        badge = '<span class="status-badge status-disconnected" style="margin-right:0.5rem">Failed</span>';
    }
    el.innerHTML = badge + ShingoEdge.escapeHtml(_backupUI.connection.message);
}

function renderBackupOperationStatus() {
    var el = document.getElementById('backup-operation-status');
    if (!el) return;
    var badge = '';
    if (_backupUI.operation.state === 'ok') {
        badge = '<span class="status-badge status-connected" style="margin-right:0.5rem">Ready</span>';
    } else if (_backupUI.operation.state === 'error') {
        badge = '<span class="status-badge status-disconnected" style="margin-right:0.5rem">Error</span>';
    } else if (_backupUI.operation.state === 'busy') {
        badge = '<span class="status-badge status-connected" style="margin-right:0.5rem">Working</span>';
    }
    el.innerHTML = badge + ShingoEdge.escapeHtml(_backupUI.operation.message);
}

function updateBackupEnabledHint() {
    var toggle = document.getElementById('backup-enabled');
    var hint = document.getElementById('backup-enabled-hint');
    if (!toggle || !hint) return;
    if (toggle.checked) {
        hint.textContent = 'Automatic backups are enabled. Saved settings will be used by the scheduler.';
    } else {
        hint.textContent = 'Automatic backups are disabled. Manual backup and connection testing remain available.';
    }
}

function backupConnectionFingerprint() {
    var data = ShingoEdge.getFormData('backup-form');
    return JSON.stringify({
        endpoint: data.endpoint || '',
        bucket: data.bucket || '',
        region: data.region || '',
        access_key: data.access_key || '',
        secret_key: data.secret_key || '',
        use_path_style: !!data.use_path_style,
        insecure_skip_tls_verify: !!data.insecure_skip_tls_verify
    });
}

function markBackupConnectionDirty() {
    _backupUI.testedFingerprint = '';
    setBackupConnection('idle', 'Connection settings changed. Run Test Connection before enabling automatic backups.');
}

async function saveBackupConfig() {
    setButtonBusy('backup-save-btn', true, 'Save Backup Settings', 'Saving...');
    try {
        var data = ShingoEdge.getFormData('backup-form');
        if (data.enabled && _backupUI.testedFingerprint !== backupConnectionFingerprint()) {
            throw 'run Test Connection after editing storage settings before enabling automatic backups';
        }
        await ShingoEdge.api.put('/api/backups/config', data);
        updateBackupEnabledHint();
        setBackupOperation('ok', data.enabled ? 'Backup settings saved. Automatic backups are enabled.' : 'Backup settings saved. Automatic backups are disabled.');
        ShingoEdge.toast(data.enabled ? 'Backup settings saved. Auto backup enabled.' : 'Backup settings saved. Auto backup disabled.', 'success');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperation('error', 'Failed to save backup settings: ' + e);
        ShingoEdge.toast('Error: ' + e, 'error');
    } finally {
        setButtonBusy('backup-save-btn', false, 'Save Backup Settings', 'Saving...');
    }
}

async function testBackupConfig() {
    setButtonBusy('backup-test-btn', true, 'Test Connection', 'Testing...');
    setBackupConnection('idle', 'Testing backup storage connection...');
    try {
        await ShingoEdge.api.post('/api/backups/test', ShingoEdge.getFormData('backup-form'));
        _backupUI.testedFingerprint = backupConnectionFingerprint();
        setBackupConnection('ok', 'Connection test succeeded for the configured bucket and credentials.');
        ShingoEdge.toast('Backup storage test succeeded', 'success');
    } catch (e) {
        setBackupConnection('error', 'Connection test failed: ' + e);
        ShingoEdge.toast('Backup test failed: ' + e, 'error');
    } finally {
        setButtonBusy('backup-test-btn', false, 'Test Connection', 'Testing...');
    }
}

async function runBackupNow() {
    setButtonBusy('backup-run-btn', true, 'Backup Now', 'Running...');
    setBackupOperation('busy', 'Manual backup in progress. Creating snapshot and uploading archive.');
    try {
        await ShingoEdge.api.post('/api/backups/run', {});
        setBackupOperation('ok', 'Manual backup completed successfully.');
        ShingoEdge.toast('Backup completed', 'success');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperation('error', 'Manual backup failed: ' + e);
        ShingoEdge.toast('Backup failed: ' + e, 'error');
    } finally {
        setButtonBusy('backup-run-btn', false, 'Backup Now', 'Running...');
    }
}

async function loadBackupStatus() {
    var el = document.getElementById('backup-status');
    if (!el) return;
    try {
        var status = await ShingoEdge.api.get('/api/backups/status');
        var lines = [];
        lines.push('<div><strong>Automatic Backups:</strong> ' + ShingoEdge.escapeHtml(status.enabled ? 'Enabled' : 'Disabled') + '</div>');
        lines.push('<div><strong>Scheduler:</strong> ' + ShingoEdge.escapeHtml(status.running ? 'Backup currently running' : 'Idle') + '</div>');
        if (status.pending) lines.push('<div><strong>Queued Trigger:</strong> ' + ShingoEdge.escapeHtml((status.pending_reasons || []).join(', ')) + '</div>');
        if (status.last_success_at) lines.push('<div><strong>Last Success:</strong> ' + ShingoEdge.escapeHtml(formatMaybeDate(status.last_success_at)) + (status.last_success_key ? ' <code>' + ShingoEdge.escapeHtml(status.last_success_key) + '</code>' : '') + '</div>');
        if (status.last_failure_at) lines.push('<div><strong>Last Failure:</strong> ' + ShingoEdge.escapeHtml(formatMaybeDate(status.last_failure_at)) + (status.last_error ? ' ' + ShingoEdge.escapeHtml(status.last_error) : '') + '</div>');
        if (status.stale) lines.push('<div><strong>Warning:</strong> ' + ShingoEdge.escapeHtml(status.stale_reason || 'backup status is stale') + '</div>');
        if (status.next_scheduled_at) lines.push('<div><strong>Next Scheduled Run:</strong> ' + ShingoEdge.escapeHtml(formatMaybeDate(status.next_scheduled_at)) + '</div>');
        if (status.restore_pending) lines.push('<div><strong>Pending Restore On Restart:</strong> <code>' + ShingoEdge.escapeHtml(status.pending_restore_key || 'pending') + '</code></div>');
        el.innerHTML = lines.join('');
    } catch (e) {
        el.textContent = 'Backup status unavailable: ' + e;
    }
}

async function loadBackups() {
    var body = document.getElementById('backup-body');
    if (!body) return;
    body.innerHTML = '<tr><td colspan="4" class="empty-cell">Loading backups...</td></tr>';
    try {
        var items = await ShingoEdge.api.get('/api/backups');
        if (!items || items.length === 0) {
            body.innerHTML = '<tr><td colspan="4" class="empty-cell">No backups found for this station</td></tr>';
            return;
        }
        body.innerHTML = items.map(function(item) {
            var created = item.created_at || item.last_modified || '';
            var age = created ? formatAge(created) : '';
            var action = '<button class="btn btn-sm btn-danger" onclick="stageRestore(' +
                JSON.stringify(item.key).replace(/"/g, '&quot;') + ',' +
                JSON.stringify(created).replace(/"/g, '&quot;') + ')">Restore On Restart</button>';
            if (item.restore_pending) action = '<span class="status-badge status-connected">Pending Restart</span>';
            return '<tr>' +
                '<td>' + ShingoEdge.escapeHtml(formatMaybeDate(created)) + (age ? '<div class="text-muted" style="font-size:0.85rem">' + ShingoEdge.escapeHtml(age) + '</div>' : '') + '</td>' +
                '<td>' + ShingoEdge.escapeHtml(formatBytes(item.size || 0)) + '</td>' +
                '<td><code>' + ShingoEdge.escapeHtml(item.key) + '</code></td>' +
                '<td>' + action + '</td>' +
                '</tr>';
        }).join('');
    } catch (e) {
        body.innerHTML = '<tr><td colspan="4" class="empty-cell">Failed to load backups: ' + ShingoEdge.escapeHtml(String(e)) + '</td></tr>';
    }
}

async function stageRestore(key, createdAt) {
    if (!_backupUI.stationID) {
        ShingoEdge.toast('Station ID must be configured before restore', 'error');
        return;
    }
    var warning = createdAt ? (' Backup age: ' + formatAge(createdAt) + '.') : '';
    var ok = await ShingoEdge.confirm('Stage restore from this backup? The backup will be applied on the next shingo-edge restart.' + warning);
    if (!ok) return;
    var typed = window.prompt('Type the station ID to confirm restore for this edge.', _backupUI.stationID);
    if (typed !== _backupUI.stationID) {
        ShingoEdge.toast('Restore cancelled: station ID confirmation did not match.', 'warning');
        return;
    }
    setBackupOperation('busy', 'Downloading and staging restore archive...');
    try {
        await ShingoEdge.api.post('/api/backups/restore', { key: key });
        setBackupOperation('ok', 'Restore staged successfully. Restart shingo-edge to apply it.');
        ShingoEdge.toast('Restore staged. Restart shingo-edge to apply it.', 'warning');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperation('error', 'Restore staging failed: ' + e);
        ShingoEdge.toast('Restore staging failed: ' + e, 'error');
    }
}

function formatBytes(bytes) {
    var units = ['B', 'KB', 'MB', 'GB', 'TB'];
    var value = bytes;
    var unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return (unit === 0 ? String(value) : value.toFixed(1)) + ' ' + units[unit];
}

function formatMaybeDate(value) {
    if (!value) return '';
    var d = new Date(value);
    if (isNaN(d)) return String(value);
    return d.toLocaleString();
}

function formatAge(value) {
    var d = new Date(value);
    if (isNaN(d)) return '';
    var sec = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000));
    if (sec < 60) return sec + 's old';
    if (sec < 3600) return Math.floor(sec / 60) + 'm old';
    if (sec < 86400) return Math.floor(sec / 3600) + 'h old';
    return Math.floor(sec / 86400) + 'd old';
}

// --- Section collapse ---
function toggleSection(id) {
    var el = document.getElementById(id);
    el.classList.toggle('collapsed');
    saveSectionState();
}

function saveSectionState() {
    var sections = document.querySelectorAll('.setup-section[id]');
    var state = {};
    for (var i = 0; i < sections.length; i++) {
        state[sections[i].id] = sections[i].classList.contains('collapsed');
    }
    localStorage.setItem('setup-sections', JSON.stringify(state));
}

(function restoreSectionState() {
    var saved = localStorage.getItem('setup-sections');
    if (!saved) return;
    try {
        var state = JSON.parse(saved);
        for (var id in state) {
            if (state[id]) {
                var el = document.getElementById(id);
                if (el) el.classList.add('collapsed');
            }
        }
    } catch (e) {}
})();

(function initBackupUI() {
    if (!document.getElementById('backup-body')) return;
    var toggle = document.getElementById('backup-enabled');
    if (toggle) toggle.addEventListener('change', updateBackupEnabledHint);
    var form = document.getElementById('backup-form');
    if (form) {
        form.querySelectorAll('input, select').forEach(function(el) {
            if (el.name === 'enabled' || el.name === 'schedule_interval' || el.name.indexOf('keep_') === 0) return;
            el.addEventListener('input', markBackupConnectionDirty);
            el.addEventListener('change', markBackupConnectionDirty);
        });
    }
    var pageData = document.getElementById('page-data');
    if (pageData) _backupUI.stationID = pageData.dataset.stationId || '';
    updateBackupEnabledHint();
    renderBackupConnectionStatus();
    renderBackupOperationStatus();
    loadBackupStatus();
    loadBackups();
    setInterval(loadBackupStatus, 30000);
})();

// --- Station Configuration (unified save) ---
async function saveStationConfig() {
    try {
        await Promise.all([
            ShingoEdge.api.put('/api/config/station-id', {
                station_id: document.getElementById('station-id-input').value.trim()
            }),
            ShingoEdge.api.put('/api/config/warlink', (function() {
                var d = ShingoEdge.getFormData('warlink-form');
                d.port = parseInt(d.port) || 8080;
                return d;
            })()),
            ShingoEdge.api.put('/api/config/messaging', { kafka_brokers: collectBrokers() }),
            ShingoEdge.api.put('/api/config/auto-confirm', {
                auto_confirm: document.getElementById('auto-confirm').checked
            })
        ]);
        ShingoEdge.toast('Configuration saved', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- WarLink mode toggle ---
function onWarlinkModeChange(mode) {
    var pollInput = document.querySelector('#warlink-form [name="poll_rate"]');
    if (pollInput) {
        pollInput.disabled = (mode !== 'poll');
        pollInput.style.opacity = (mode !== 'poll') ? '0.5' : '';
    }
}
// Apply initial state
(function() {
    var modeSelect = document.querySelector('#warlink-form [name="mode"]');
    if (modeSelect) onWarlinkModeChange(modeSelect.value);
})();

async function refreshPLCChips() {
    try {
        var plcs = await ShingoEdge.api.get('/api/plcs');
        var wrapper = document.getElementById('plc-chips-wrapper');
        if (!wrapper) return;
        if (!plcs || plcs.length === 0) {
            wrapper.innerHTML = '';
            return;
        }
        var html = '<label style="display:block;margin-bottom:0.25rem;font-weight:500;color:var(--text-muted)">Available PLCs</label>' +
            '<div id="plc-chips" style="display:flex;gap:0.5rem;flex-wrap:wrap">';
        for (var i = 0; i < plcs.length; i++) {
            var p = plcs[i];
            html += '<span class="plc-chip ' + (p.connected ? 'plc-chip-connected' : 'plc-chip-disconnected') + '" id="plc-status-' + p.name + '"><span class="plc-health-dot ' + (p.connected ? 'plc-health-online' : 'plc-health-unknown') + '" id="plc-health-' + p.name + '"></span>' + p.name + '</span>';
        }
        html += '</div>';
        wrapper.innerHTML = html;
    } catch (e) { /* ignore */ }
}

// --- Tag Picker ---
var _tagCache = {};

function loadTagsForPLC(plcName, cb) {
    if (_tagCache[plcName]) { cb(_tagCache[plcName]); return; }
    ShingoEdge.api.get('/api/plcs/all-tags/' + encodeURIComponent(plcName)).then(function(tags) {
        _tagCache[plcName] = tags || [];
        cb(_tagCache[plcName]);
    }).catch(function() { cb([]); });
}

function onPLCSelectChange(sel) {
    var form = sel.closest('.card-body');
    var tagInput = form.querySelector('[name="tag_name"]');
    if (tagInput) tagInput.value = '';
    var dropdown = form.querySelector('.tag-picker-dropdown');
    if (dropdown) dropdown.style.display = 'none';
    if (sel.value) {
        loadTagsForPLC(sel.value, function() {});
    }
}

function openTagPicker(input) {
    var form = input.closest('.card-body');
    var plcSel = form.querySelector('[name="plc_name"]');
    if (!plcSel || !plcSel.value) return;
    loadTagsForPLC(plcSel.value, function(tags) {
        renderTagDropdown(input, tags, input.value);
    });
}

function filterTagPicker(input) {
    var form = input.closest('.card-body');
    var plcSel = form.querySelector('[name="plc_name"]');
    if (!plcSel || !plcSel.value) return;
    var tags = _tagCache[plcSel.value] || [];
    renderTagDropdown(input, tags, input.value);
}

function renderTagDropdown(input, tags, filter) {
    var dropdown = input.parentElement.querySelector('.tag-picker-dropdown');
    var lc = (filter || '').toLowerCase();
    var filtered = tags.filter(function(t) { return !lc || t.name.toLowerCase().indexOf(lc) !== -1; });
    if (filtered.length === 0) {
        dropdown.innerHTML = '<div class="tag-picker-empty">No matching tags</div>';
    } else {
        var html = '';
        var limit = Math.min(filtered.length, 100);
        for (var i = 0; i < limit; i++) {
            var t = filtered[i];
            html += '<div class="tag-picker-item" onmousedown="selectTagPickerItem(this, \'' + ShingoEdge.escapeHtml(t.name).replace(/'/g, "\\'") + '\')">';
            html += '<span class="tag-picker-name">' + ShingoEdge.escapeHtml(t.name) + '</span>';
            if (t.enabled === false) {
                html += '<span class="tag-picker-unpublished">(not published)</span>';
            }
            html += '<span class="tag-picker-type">' + ShingoEdge.escapeHtml(t.type) + '</span>';
            html += '</div>';
        }
        if (filtered.length > limit) {
            html += '<div class="tag-picker-empty">' + (filtered.length - limit) + ' more...</div>';
        }
        dropdown.innerHTML = html;
    }
    dropdown.style.display = '';
}

function selectTagPickerItem(el, tagName) {
    var picker = el.closest('.tag-picker');
    var input = picker.querySelector('.tag-picker-input');
    input.value = tagName;
    picker.querySelector('.tag-picker-dropdown').style.display = 'none';
}

// Close tag picker on outside click
document.addEventListener('click', function(e) {
    if (!e.target.closest('.tag-picker')) {
        var dropdowns = document.querySelectorAll('.tag-picker-dropdown');
        for (var i = 0; i < dropdowns.length; i++) dropdowns[i].style.display = 'none';
    }
});

// --- Cat-ID Chips ---
var _catIDData = { 'js-add': [], 'js-edit': [] };

function renderCatIDChips(prefix) {
    var container = document.getElementById(prefix + '-catids');
    var ids = _catIDData[prefix] || [];
    var html = '';
    for (var i = 0; i < ids.length; i++) {
        html += '<span class="cat-id-chip">' + ShingoEdge.escapeHtml(ids[i]) +
            '<button type="button" class="cat-id-remove" onclick="removeCatIDChip(\'' + prefix + '\',' + i + ')">&times;</button></span>';
    }
    container.innerHTML = html;
}

function addCatIDChip(prefix) {
    var input = document.getElementById(prefix + '-catid-input');
    var val = input.value.trim();
    if (!val) return;
    if (!_catIDData[prefix]) _catIDData[prefix] = [];
    _catIDData[prefix].push(val);
    input.value = '';
    renderCatIDChips(prefix);
}

function removeCatIDChip(prefix, index) {
    _catIDData[prefix].splice(index, 1);
    renderCatIDChips(prefix);
}

function getCatIDs(prefix) {
    return _catIDData[prefix] || [];
}

function setCatIDs(prefix, ids) {
    _catIDData[prefix] = ids || [];
    renderCatIDChips(prefix);
}

// --- Production Lines ---
async function addProcess() {
    var d = ShingoEdge.getFormData('line-add-form');
    try {
        await ShingoEdge.api.post('/api/processes', d);
        ShingoEdge.toast('Process added', 'success');
        ShingoEdge.hideModal('line-add');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function openEditProcess(id, name, desc) {
    ShingoEdge.populateForm('line-edit-form', { id: id, name: name, description: desc });
    ShingoEdge.showModal('line-edit');
}

async function saveProcess() {
    var d = ShingoEdge.getFormData('line-edit-form');
    var id = d.id; delete d.id;
    try {
        await ShingoEdge.api.put('/api/processes/' + id, d);
        ShingoEdge.toast('Process updated', 'success');
        ShingoEdge.hideModal('line-edit');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteProcess(id) {
    var ok = await ShingoEdge.confirm('Delete this process and all its styles?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/processes/' + id);
        ShingoEdge.toast('Deleted', 'success');
        location.reload();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function setActiveStyle(processID, styleID) {
    try {
        await ShingoEdge.api.put('/api/processes/' + processID + '/active-style', {
            style_id: styleID ? parseInt(styleID) : null
        });
        ShingoEdge.toast('Active style updated', 'success');
        loadStyleChips(processID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Style Chips ---
async function loadStyleChips(processID) {
    var container = document.getElementById('style-chips-' + processID);
    if (!container) return;
    try {
        var styles = await ShingoEdge.api.get('/api/processes/' + processID + '/styles');
        var processes = await ShingoEdge.api.get('/api/processes');
        var activeStyleID = null;
        for (var i = 0; i < processes.length; i++) {
            if (processes[i].id === processID) { activeStyleID = processes[i].active_style_id; break; }
        }
        if (!styles || styles.length === 0) {
            container.innerHTML = '<span style="font-size:0.8rem;color:var(--text-muted);padding:0.25rem 0">No styles</span>';
            return;
        }
        var html = '';
        for (var i = 0; i < styles.length; i++) {
            var s = styles[i];
            var cls = s.id === activeStyleID ? 'style-chip style-chip-active' : 'style-chip style-chip-inactive';
            html += '<span class="' + cls + '" onclick="openEditStyle(' + s.id + ',' + processID + ',' + (s.id === activeStyleID ? 'true' : 'false') + ')" title="' + ShingoEdge.escapeHtml(s.description || '') + '">' + ShingoEdge.escapeHtml(s.name) + '</span>';
        }
        container.innerHTML = html;
    } catch (e) {
        container.innerHTML = '<span style="font-size:0.8rem;color:var(--text-muted)">Error loading styles</span>';
    }
}

function refreshAllStyleChips() {
    var cards = document.querySelectorAll('[data-process-id]');
    for (var i = 0; i < cards.length; i++) {
        loadStyleChips(parseInt(cards[i].getAttribute('data-process-id')));
    }
}

// Load all chips on page load
(function() { refreshAllStyleChips(); })();

// --- Styles ---
var _currentStyleProcessID = 0;
var _currentStyleIsActive = false;

function openAddStyle(processID) {
    _currentStyleProcessID = processID;
    var form = document.getElementById('js-add-form');
    form.querySelector('[name="line_id"]').value = processID;
    form.querySelector('[name="name"]').value = '';
    form.querySelector('[name="description"]').value = '';
    form.querySelector('[name="plc_name"]').value = '';
    form.querySelector('[name="tag_name"]').value = '';
    form.querySelector('[name="rp_enabled"]').checked = true;
    setCatIDs('js-add', []);
    ShingoEdge.showModal('js-add');
}

async function addStyle() {
    var d = ShingoEdge.getFormData('js-add-form');
    d.line_id = parseInt(d.line_id);
    d.cat_ids = getCatIDs('js-add');
    d.rp_plc_name = d.plc_name || '';
    d.rp_tag_name = d.tag_name || '';
    d.rp_enabled = !!d.rp_enabled;
    delete d.plc_name;
    delete d.tag_name;
    try {
        await ShingoEdge.api.post('/api/styles', d);
        ShingoEdge.toast('Style added', 'success');
        ShingoEdge.hideModal('js-add');
        loadStyleChips(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function openEditStyle(id, processID, isActive) {
    _currentStyleProcessID = processID;
    _currentStyleIsActive = isActive;

    // Fetch style data
    var styles = await ShingoEdge.api.get('/api/processes/' + processID + '/styles');
    var style = null;
    for (var i = 0; i < (styles || []).length; i++) {
        if (styles[i].id === id) { style = styles[i]; break; }
    }
    if (!style) { ShingoEdge.toast('Style not found', 'error'); return; }

    var form = document.getElementById('js-edit-form');
    form.querySelector('[name="id"]').value = id;
    form.querySelector('[name="line_id"]').value = lineID;
    form.querySelector('[name="name"]').value = style.name;
    form.querySelector('[name="description"]').value = style.description;
    setCatIDs('js-edit', style.cat_ids || []);

    // Fetch reporting point for this style
    var rp = null;
    try {
        rp = await ShingoEdge.api.get('/api/styles/' + id + '/reporting-point');
    } catch (e) {}

    form.querySelector('[name="plc_name"]').value = rp ? rp.plc_name : '';
    form.querySelector('[name="tag_name"]').value = rp ? rp.tag_name : '';
    form.querySelector('[name="rp_enabled"]').checked = rp ? rp.enabled : true;

    // Show/hide "Set as Active" button
    var setActiveBtn = document.getElementById('js-edit-set-active');
    setActiveBtn.style.display = isActive ? 'none' : '';

    ShingoEdge.showModal('js-edit');
}

async function saveStyle() {
    var d = ShingoEdge.getFormData('js-edit-form');
    var id = d.id; delete d.id;
    d.line_id = parseInt(d.line_id);
    d.cat_ids = getCatIDs('js-edit');
    d.rp_plc_name = d.plc_name || '';
    d.rp_tag_name = d.tag_name || '';
    d.rp_enabled = !!d.rp_enabled;
    delete d.plc_name;
    delete d.tag_name;
    try {
        await ShingoEdge.api.put('/api/styles/' + id, d);
        ShingoEdge.toast('Style updated', 'success');
        ShingoEdge.hideModal('js-edit');
        loadStyleChips(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function setActiveStyleFromModal() {
    var form = document.getElementById('js-edit-form');
    var styleID = parseInt(form.querySelector('[name="id"]').value);
    var processID = parseInt(form.querySelector('[name="line_id"]').value);
    await setActiveStyle(processID, styleID);
    ShingoEdge.hideModal('js-edit');
}

async function deleteStyle(id, processID) {
    var ok = await ShingoEdge.confirm('Delete this style?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/styles/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadStyleChips(processID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Material Slots ---
async function onSlotProcessChange() {
    var processID = document.getElementById('payload-line').value;
    var styleSel = document.getElementById('payload-job-style');
    styleSel.innerHTML = '<option value="">-- Select Style --</option>';
    if (!processID) { loadSlots(); return; }
    try {
        var styles = await ShingoEdge.api.get('/api/processes/' + processID + '/styles');
        for (var i = 0; i < (styles || []).length; i++) {
            var opt = document.createElement('option');
            opt.value = styles[i].id;
            opt.textContent = styles[i].name;
            styleSel.appendChild(opt);
        }
    } catch (e) {}
    loadSlots();
}

async function loadSlots() {
    var styleID = document.getElementById('payload-job-style').value;
    var body = document.getElementById('payload-body');
    var addBar = document.getElementById('payload-add-bar');
    if (!styleID) {
        body.innerHTML = '<tr><td colspan="9" class="empty-cell">Select a process and style to view material slots</td></tr>';
        addBar.style.display = 'none';
        return;
    }
    addBar.style.display = '';
    try {
        var slots = await ShingoEdge.api.get('/api/material-slots/style/' + styleID);
        if (!slots || slots.length === 0) {
            body.innerHTML = '<tr><td colspan="9" class="empty-cell">No material slots for this style</td></tr>';
            return;
        }
        var html = '';
        for (var i = 0; i < slots.length; i++) {
            var s = slots[i];
            var modeLabel = s.cycle_mode === 'two_robot' ? 'Two Robot' : s.cycle_mode === 'single_robot' ? 'Single Robot' : 'Sequential';
            html += '<tr id="slot-' + s.id + '">' +
                '<td class="mono">' + ShingoEdge.escapeHtml(s.location) + '</td>' +
                '<td class="mono">' + ShingoEdge.escapeHtml(s.payload_code) + '</td>' +
                '<td>' + ShingoEdge.escapeHtml(s.description) + '</td>' +
                '<td>' + s.role + '</td>' +
                '<td>' + s.remaining + ' / ' + s.reorder_point + '</td>' +
                '<td>' + s.reorder_point + '</td>' +
                '<td><span class="status-badge status-' + (s.cycle_mode === 'sequential' ? 'stored' : 'active') + '">' + modeLabel + '</span></td>' +
                '<td><span class="status-badge status-' + s.status + '">' + s.status + '</span></td>' +
                '<td class="actions">' +
                    '<button class="btn-icon" onclick=\'openEditSlot(' + JSON.stringify(s).replace(/'/g, "\\'") + ')\' title="Edit">&#9998;</button>' +
                    '<button class="btn-icon" onclick="resetSlot(' + s.id + ')" title="Reset to full">&#8634;</button>' +
                    '<button class="btn-icon btn-icon-danger" onclick="deleteSlot(' + s.id + ')" title="Delete">&#10005;</button>' +
                '</td></tr>';
        }
        body.innerHTML = html;
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// ── Cycle mode form helpers ──────────────────────────────────────────

function onCycleModeChange(formId) {
    var form = document.getElementById(formId);
    var mode = form.querySelector('[name="cycle_mode"]').value;
    var fields = form.querySelector('.hotswap-fields');
    var staging2 = form.querySelectorAll('.staging2-row');

    // Node selection fields apply to all cycle modes
    fields.style.display = '';

    staging2.forEach(function(el) {
        el.style.display = (mode === 'single_robot') ? '' : 'none';
        if (mode !== 'single_robot') {
            el.querySelectorAll('.node-value-group select').forEach(function(sel) {
                sel.innerHTML = '<option value="">-- Select --</option>';
            });
        }
    });
}

function onNodeTypeChange(sel) {
    var row = sel.closest('.form-row');
    var valueGroup = row.querySelector('.node-value-group');
    if (sel.value) {
        valueGroup.style.display = '';
        var valueSel = valueGroup.querySelector('select');
        if (valueSel) populateNodeSelect(valueSel, sel.value);
    } else {
        valueGroup.style.display = 'none';
        var valueSel = valueGroup.querySelector('select');
        if (valueSel) { valueSel.innerHTML = '<option value="">-- Select --</option>'; }
    }
}

// Populate a node value dropdown with nodes from Core, filtered by type.
// nodeType: "node" = physical nodes (non-NGRP), "group" = NGRP synthetic groups
function populateNodeSelect(sel, nodeType) {
    var currentVal = sel.value;
    loadCoreNodeListFull(function(nodes) {
        sel.innerHTML = '<option value="">-- Select --</option>';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var isGroup = (n.node_type === 'NGRP');
            if (nodeType === 'group' && !isGroup) continue;
            if (nodeType === 'node' && isGroup) continue;
            var opt = document.createElement('option');
            opt.value = n.name;
            opt.textContent = n.name;
            if (n.name === currentVal) opt.selected = true;
            sel.appendChild(opt);
        }
    });
}

// Cache full node objects (name + node_type) from Core
var _coreNodeListFull = null;

function loadCoreNodeListFull(cb) {
    if (_coreNodeListFull !== null) { cb(_coreNodeListFull); return; }
    ShingoEdge.api.get('/api/core-nodes').then(function(nodes) {
        _coreNodeListFull = (nodes || []).sort(function(a, b) {
            return a.name.localeCompare(b.name);
        });
        cb(_coreNodeListFull);
    }).catch(function() { cb([]); });
}

// Cache location nodes from Edge DB (node_id, description)
var _locationNodeList = null;

function loadLocationNodeList(cb) {
    if (_locationNodeList !== null) { cb(_locationNodeList); return; }
    ShingoEdge.api.get('/api/nodes').then(function(nodes) {
        _locationNodeList = (nodes || []).sort(function(a, b) {
            return a.node_id.localeCompare(b.node_id);
        });
        cb(_locationNodeList);
    }).catch(function() { cb([]); });
}

function populateLocationSelect(sel, currentVal, cb) {
    loadLocationNodeList(function(nodes) {
        sel.innerHTML = '<option value="">-- Select --</option>';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var opt = document.createElement('option');
            opt.value = n.node_id;
            opt.textContent = n.node_id + (n.description ? ' \u2013 ' + n.description : '');
            if (n.node_id === currentVal) opt.selected = true;
            sel.appendChild(opt);
        }
        if (cb) cb();
    });
}

function readCycleModeFields(formId) {
    var form = document.getElementById(formId);
    var result = { cycle_mode: 'sequential', staging_node: '', staging_node_group: '', staging_node_2: '', staging_node_2_group: '', full_pickup_node: '', full_pickup_node_group: '', outgoing_node: '', outgoing_node_group: '' };

    var mode = form.querySelector('[name="cycle_mode"]');
    if (mode) result.cycle_mode = mode.value || 'sequential';

    // Full Pickup
    var fpType = form.querySelector('[name="full_pickup_type"]');
    var fpVal = form.querySelector('[name="full_pickup_value"]');
    if (fpType && fpVal && fpType.value === 'node') result.full_pickup_node = fpVal.value;
    else if (fpType && fpVal && fpType.value === 'group') result.full_pickup_node_group = fpVal.value;

    // Staging 1
    var stType = form.querySelector('[name="staging_type"]');
    var stVal = form.querySelector('[name="staging_value"]');
    if (stType && stVal && stType.value === 'node') result.staging_node = stVal.value;
    else if (stType && stVal && stType.value === 'group') result.staging_node_group = stVal.value;

    // Staging 2
    var st2Type = form.querySelector('[name="staging2_type"]');
    var st2Val = form.querySelector('[name="staging2_value"]');
    if (st2Type && st2Val && st2Type.value === 'node') result.staging_node_2 = st2Val.value;
    else if (st2Type && st2Val && st2Type.value === 'group') result.staging_node_2_group = st2Val.value;

    // Empty Drop
    var edType = form.querySelector('[name="outgoing_type"]');
    var edVal = form.querySelector('[name="outgoing_value"]');
    if (edType && edVal && edType.value === 'node') result.outgoing_node = edVal.value;
    else if (edType && edVal && edType.value === 'group') result.outgoing_node_group = edVal.value;

    return result;
}

function populateCycleModeFields(formId, p) {
    var form = document.getElementById(formId);

    var modeSelect = form.querySelector('[name="cycle_mode"]');
    if (modeSelect) modeSelect.value = p.cycle_mode || 'sequential';

    function setNodeField(typeName, valueName, nodeVal, groupVal) {
        var typeEl = form.querySelector('[name="' + typeName + '"]');
        var valEl = form.querySelector('[name="' + valueName + '"]');
        if (!typeEl || !valEl) return;
        var targetVal = nodeVal || groupVal || '';
        if (nodeVal) { typeEl.value = 'node'; }
        else if (groupVal) { typeEl.value = 'group'; }
        else { typeEl.value = ''; }
        // Trigger node type change to show/populate the dropdown
        var row = typeEl.closest('.form-row');
        var valueGroup = row.querySelector('.node-value-group');
        if (typeEl.value) {
            valueGroup.style.display = '';
            populateNodeSelect(valEl, typeEl.value);
            // Set value after populate completes (async)
            loadCoreNodeListFull(function() { valEl.value = targetVal; });
        } else {
            valueGroup.style.display = 'none';
            valEl.innerHTML = '<option value="">-- Select --</option>';
        }
    }

    setNodeField('full_pickup_type', 'full_pickup_value', p.full_pickup_node, p.full_pickup_node_group);
    setNodeField('staging_type', 'staging_value', p.staging_node, p.staging_node_group);
    setNodeField('staging2_type', 'staging2_value', p.staging_node_2, p.staging_node_2_group);
    setNodeField('outgoing_type', 'outgoing_value', p.outgoing_node, p.outgoing_node_group);

    onCycleModeChange(formId);
}

// ── Slot CRUD ────────────────────────────────────────────────────────

async function addSlot() {
    var styleID = document.getElementById('payload-job-style').value;
    if (!styleID) { ShingoEdge.toast('Select a style first', 'warning'); return; }
    var d = ShingoEdge.getFormData('payload-add-form');
    d.style_id = parseInt(styleID);
    d.reorder_point = parseInt(d.reorder_point) || 0;

    // Merge cycle mode fields
    var cm = readCycleModeFields('payload-add-form');
    Object.assign(d, cm);

    // Clean up UI-only fields
    delete d.full_pickup_type; delete d.full_pickup_value;
    delete d.staging_type; delete d.staging_value;
    delete d.staging2_type; delete d.staging2_value;
    delete d.outgoing_type; delete d.outgoing_value;

    try {
        await ShingoEdge.api.post('/api/material-slots', d);
        ShingoEdge.toast('Slot added', 'success');
        ShingoEdge.hideModal('payload-add');
        loadSlots();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

function openAddSlot() {
    var sel = document.querySelector('#payload-add-form [name="location"]');
    populateLocationSelect(sel, '', function() {
        ShingoEdge.showModal('payload-add');
    });
}

function openEditSlot(s) {
    var sel = document.querySelector('#payload-edit-form [name="location"]');
    populateLocationSelect(sel, s.location, function() {
        ShingoEdge.populateForm('payload-edit-form', {
            id: s.id, location: s.location,
            description: s.description, payload_code: s.payload_code || '',
            role: s.role, auto_reorder: s.auto_reorder,
            reorder_point: s.reorder_point
        });
        populateCycleModeFields('payload-edit-form', s);
        ShingoEdge.showModal('payload-edit');
    });
}

async function saveSlot() {
    var d = ShingoEdge.getFormData('payload-edit-form');
    var id = d.id; delete d.id;
    d.reorder_point = parseInt(d.reorder_point) || 0;

    // Merge cycle mode fields
    var cm = readCycleModeFields('payload-edit-form');
    Object.assign(d, cm);

    // Clean up UI-only fields
    delete d.full_pickup_type; delete d.full_pickup_value;
    delete d.staging_type; delete d.staging_value;
    delete d.staging2_type; delete d.staging2_value;
    delete d.outgoing_type; delete d.outgoing_value;

    try {
        await ShingoEdge.api.put('/api/material-slots/' + id, d);
        ShingoEdge.toast('Slot updated', 'success');
        ShingoEdge.hideModal('payload-edit');
        loadSlots();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteSlot(id) {
    var ok = await ShingoEdge.confirm('Delete this material slot?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/material-slots/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadSlots();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function resetSlot(id) {
    var ok = await ShingoEdge.confirm('Reset this slot to full capacity?');
    if (!ok) return;
    try {
        await ShingoEdge.api.put('/api/material-slots/' + id + '/count', { piece_count: 0, reset: true });
        ShingoEdge.toast('Slot reset', 'success');
        loadSlots();
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Core Nodes ---
var _coreNodeSet = {};

async function fetchCoreNodes() {
    try {
        var nodes = await ShingoEdge.api.get('/api/core-nodes');
        _coreNodeSet = {};
        for (var i = 0; i < (nodes || []).length; i++) {
            _coreNodeSet[nodes[i].name] = true;
        }
    } catch (e) { /* ignore */ }
}

async function syncCoreNodes() {
    try {
        await ShingoEdge.api.post('/api/core-nodes/sync', {});
        ShingoEdge.toast('Syncing nodes...', 'success');
        // Invalidate both cached node lists so pickers re-fetch
        _coreNodeList = null;
        _coreNodeListFull = null;
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Nodes (nested under processes) ---
async function loadProcessNodes(processID) {
    var container = document.getElementById('node-list-' + processID);
    if (!container) return;
    try {
        var nodes = await ShingoEdge.api.get('/api/processes/' + processID + '/nodes');
        if (!nodes || nodes.length === 0) {
            container.innerHTML = '';
            return;
        }
        var html = '<div style="border-top:1px solid var(--border);padding-top:0.4rem;margin-top:0.25rem">' +
            '<label style="display:block;margin-bottom:0.25rem;font-weight:500;font-size:0.8rem;color:var(--text-muted)">Nodes</label>' +
            '<div style="display:flex;gap:0.4rem;flex-wrap:wrap">';
        for (var i = 0; i < nodes.length; i++) {
            var n = nodes[i];
            var confirmed = _coreNodeSet[n.node_id];
            var chipClass = confirmed ? 'style-chip style-chip-active' : 'style-chip style-chip-inactive';
            var tip = n.description ? ShingoEdge.escapeHtml(n.description) : '';
            if (!confirmed) tip = (tip ? tip + ' — ' : '') + 'Unconfirmed (not in core)';
            html += '<span class="' + chipClass + '" onclick="openEditNode(' + n.id + ',' + n.line_id + ')" title="' + tip + '" style="cursor:pointer">' + ShingoEdge.escapeHtml(n.node_id) + '</span>';
        }
        html += '</div></div>';
        container.innerHTML = html;
    } catch (e) {
        container.innerHTML = '';
    }
}

async function refreshAllProcessNodes() {
    await fetchCoreNodes();
    var cards = document.querySelectorAll('[data-process-id]');
    for (var i = 0; i < cards.length; i++) {
        loadProcessNodes(parseInt(cards[i].getAttribute('data-process-id')));
    }
}

// Load all nodes on page load
(function() { refreshAllProcessNodes(); })();

var _currentNodeProcessID = 0;

function openAddNode(processID) {
    _currentNodeProcessID = processID;
    var form = document.getElementById('node-add-form');
    form.querySelector('[name="line_id"]').value = processID;
    form.querySelector('[name="node_id"]').value = '';
    form.querySelector('[name="description"]').value = '';
    ShingoEdge.showModal('node-add');
}

async function addNode() {
    var d = ShingoEdge.getFormData('node-add-form');
    d.line_id = parseInt(d.line_id);
    try {
        await ShingoEdge.api.post('/api/nodes', d);
        ShingoEdge.toast('Node added', 'success');
        ShingoEdge.hideModal('node-add');
        loadProcessNodes(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function openEditNode(id, processID) {
    _currentNodeProcessID = processID;
    // Fetch current node data
    var nodes = await ShingoEdge.api.get('/api/processes/' + processID + '/nodes');
    var node = null;
    for (var i = 0; i < (nodes || []).length; i++) {
        if (nodes[i].id === id) { node = nodes[i]; break; }
    }
    if (!node) { ShingoEdge.toast('Node not found', 'error'); return; }
    ShingoEdge.populateForm('node-edit-form', { id: id, line_id: processID, node_id: node.node_id, description: node.description });
    ShingoEdge.showModal('node-edit');
}

async function saveNode() {
    var d = ShingoEdge.getFormData('node-edit-form');
    var id = d.id; delete d.id;
    d.line_id = parseInt(d.line_id);
    try {
        await ShingoEdge.api.put('/api/nodes/' + id, d);
        ShingoEdge.toast('Node updated', 'success');
        ShingoEdge.hideModal('node-edit');
        loadProcessNodes(d.line_id);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteNode(id, processID) {
    var ok = await ShingoEdge.confirm('Delete this location node?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/nodes/' + id);
        ShingoEdge.toast('Deleted', 'success');
        loadProcessNodes(processID || _currentNodeProcessID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

async function deleteNodeFromModal() {
    var form = document.getElementById('node-edit-form');
    var id = parseInt(form.querySelector('[name="id"]').value);
    var lineID = parseInt(form.querySelector('[name="line_id"]').value);
    var ok = await ShingoEdge.confirm('Delete this location node?');
    if (!ok) return;
    try {
        await ShingoEdge.api.del('/api/nodes/' + id);
        ShingoEdge.toast('Deleted', 'success');
        ShingoEdge.hideModal('node-edit');
        loadProcessNodes(lineID);
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Node Picker (core nodes dropdown) ---
var _coreNodeList = null;

function loadCoreNodeList(cb) {
    if (_coreNodeList !== null) { cb(_coreNodeList); return; }
    ShingoEdge.api.get('/api/core-nodes').then(function(names) {
        _coreNodeList = (names || []).sort();
        cb(_coreNodeList);
    }).catch(function() { cb([]); });
}

function openNodePicker(input) {
    loadCoreNodeList(function(nodes) {
        renderNodeDropdown(input, nodes, input.value);
    });
}

function filterNodePicker(input) {
    var nodes = _coreNodeList || [];
    renderNodeDropdown(input, nodes, input.value);
}

function renderNodeDropdown(input, nodes, filter) {
    var dropdown = input.parentElement.querySelector('.tag-picker-dropdown');
    if (!dropdown) return;
    var lc = (filter || '').toLowerCase();
    var filtered = nodes.filter(function(n) { return !lc || n.toLowerCase().indexOf(lc) !== -1; });
    if (filtered.length === 0) {
        dropdown.innerHTML = '<div class="tag-picker-empty">No matching nodes</div>';
    } else {
        var html = '';
        var limit = Math.min(filtered.length, 100);
        for (var i = 0; i < limit; i++) {
            html += '<div class="tag-picker-item" onmousedown="selectNodePickerItem(this, \'' + ShingoEdge.escapeHtml(filtered[i]).replace(/'/g, "\\'") + '\')">';
            html += '<span class="tag-picker-name">' + ShingoEdge.escapeHtml(filtered[i]) + '</span>';
            html += '</div>';
        }
        if (filtered.length > limit) {
            html += '<div class="tag-picker-empty">' + (filtered.length - limit) + ' more...</div>';
        }
        dropdown.innerHTML = html;
    }
    dropdown.style.display = '';
}

function selectNodePickerItem(el, nodeName) {
    var picker = el.closest('.tag-picker');
    var input = picker.querySelector('.tag-picker-input');
    input.value = nodeName;
    picker.querySelector('.tag-picker-dropdown').style.display = 'none';
}

// --- Messaging (Kafka) ---
function addBrokerRow() {
    var container = document.getElementById('broker-rows');
    var row = document.createElement('div');
    row.className = 'broker-row';
    row.innerHTML = '<input type="text" class="form-input broker-host" style="flex:1" placeholder="localhost">' +
        '<input type="number" class="form-input broker-port" style="width:80px" placeholder="9092">' +
        '<button class="btn btn-sm" onclick="testBroker(this)">Test</button>' +
        '<span class="broker-status"></span>' +
        '<button class="btn-icon btn-icon-danger" onclick="removeBrokerRow(this)" title="Remove">&#10005;</button>';
    container.appendChild(row);
}

function removeBrokerRow(btn) {
    var row = btn.closest('.broker-row');
    var container = document.getElementById('broker-rows');
    if (container.querySelectorAll('.broker-row').length > 1) {
        row.remove();
    } else {
        row.querySelector('.broker-host').value = '';
    }
}

function collectBrokers() {
    var rows = document.querySelectorAll('.broker-row');
    var brokers = [];
    for (var i = 0; i < rows.length; i++) {
        var host = rows[i].querySelector('.broker-host').value.trim();
        var port = rows[i].querySelector('.broker-port').value.trim();
        if (host) brokers.push(host + ':' + (port || '9092'));
    }
    return brokers;
}

function brokerAddr(row) {
    var host = row.querySelector('.broker-host').value.trim();
    var port = row.querySelector('.broker-port').value.trim();
    return host ? host + ':' + (port || '9092') : '';
}

async function testBroker(btn) {
    var row = btn.closest('.broker-row');
    var host = brokerAddr(row);
    var status = row.querySelector('.broker-status');
    if (!host) { status.textContent = ''; return; }
    status.textContent = 'Testing...';
    status.className = 'broker-status';
    try {
        var res = await ShingoEdge.api.post('/api/config/kafka/test', { broker: host });
        if (res.connected) {
            status.textContent = 'Connected';
            status.className = 'broker-status broker-status-ok';
        } else {
            status.textContent = res.error || 'Failed';
            status.className = 'broker-status broker-status-err';
        }
    } catch (e) {
        status.textContent = 'Error';
        status.className = 'broker-status broker-status-err';
    }
}

// --- Password ---
async function changePassword() {
    try {
        await ShingoEdge.api.post('/api/config/password', {
            old_password: document.getElementById('pw-old').value,
            new_password: document.getElementById('pw-new').value
        });
        ShingoEdge.toast('Password changed', 'success');
    } catch (e) { ShingoEdge.toast('Error: ' + e, 'error'); }
}

// --- Persistent Toast ---
function showPLCAlert(plcName, error) {
    // Dedup by PLC name
    var existing = document.querySelector('[data-plc-alert="' + plcName + '"]');
    if (existing) return;
    var container = document.querySelector('.toast-container');
    if (!container) {
        container = document.createElement('div');
        container.className = 'toast-container';
        document.body.appendChild(container);
    }
    var toast = document.createElement('div');
    toast.className = 'toast toast-persistent';
    toast.setAttribute('data-plc-alert', plcName);
    toast.innerHTML = '<span class="toast-msg">PLC ' + ShingoEdge.escapeHtml(plcName) + ' offline' + (error ? ': ' + ShingoEdge.escapeHtml(error) : '') + '</span>' +
        '<button class="toast-close" onclick="this.parentElement.remove()">&times;</button>';
    container.appendChild(toast);
}

function dismissPLCAlert(plcName) {
    var el = document.querySelector('[data-plc-alert="' + plcName + '"]');
    if (el) el.remove();
}

// --- SSE ---
ShingoEdge.createSSE('/events', {
    onPlcHealthAlert: function(data) {
        showPLCAlert(data.plc_name, data.error);
        var dot = document.getElementById('plc-health-' + data.plc_name);
        if (dot) { dot.className = 'plc-health-dot plc-health-offline'; }
    },
    onPlcHealthRecover: function(data) {
        dismissPLCAlert(data.plc_name);
        var dot = document.getElementById('plc-health-' + data.plc_name);
        if (dot) { dot.className = 'plc-health-dot plc-health-online'; }
    },
    onPlcStatus: function(data) {
        var el = document.getElementById('plc-status-' + data.plcName);
        if (el) {
            // Preserve the health dot
            var dot = el.querySelector('.plc-health-dot');
            el.className = 'plc-chip ' + (data.connected ? 'plc-chip-connected' : 'plc-chip-disconnected');
            if (dot && !el.contains(dot)) el.insertBefore(dot, el.firstChild);
        }
        if (data.connected) {
            dismissPLCAlert(data.plcName);
            var hdot = document.getElementById('plc-health-' + data.plcName);
            if (hdot) { hdot.className = 'plc-health-dot plc-health-online'; }
        }
    },
    onCoreNodes: function(data) {
        _coreNodeSet = {};
        var nodes = data.nodes || [];
        for (var i = 0; i < nodes.length; i++) {
            _coreNodeSet[nodes[i].name] = true;
        }
        _coreNodeList = nodes.map(function(n) { return n.name; }).sort();
        _coreNodeListFull = null; // Invalidate full list so cycle mode dropdowns re-fetch with types
        // Refresh node chips without re-fetching core nodes
        var cards = document.querySelectorAll('[data-process-id]');
        for (var i = 0; i < cards.length; i++) {
            loadProcessNodes(parseInt(cards[i].getAttribute('data-process-id')));
        }
        ShingoEdge.toast('Node list updated (' + nodes.length + ' nodes)', 'success');
    },
    onWarlinkStatus: function(data) {
        var badge = document.getElementById('warlink-status');
        if (badge) {
            badge.textContent = data.connected ? 'Connected' : 'Disconnected';
            badge.className = 'status-badge ' + (data.connected ? 'status-connected' : 'status-disconnected');
        }
        if (data.connected) {
            refreshPLCChips();
        } else {
            // Mark all chips disconnected
            var chips = document.querySelectorAll('.plc-chip');
            for (var i = 0; i < chips.length; i++) {
                chips[i].className = 'plc-chip plc-chip-disconnected';
            }
        }
    }
});
