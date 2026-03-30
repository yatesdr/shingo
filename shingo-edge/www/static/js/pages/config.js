function collectBrokers() {
    return Array.from(document.querySelectorAll('.broker-row')).map(function(row) {
        const host = row.querySelector('.broker-host').value.trim();
        const port = row.querySelector('.broker-port').value.trim();
        if (!host) return '';
        return port ? host + ':' + port : host;
    }).filter(Boolean);
}

function addBrokerRow() {
    const row = document.createElement('div');
    row.className = 'broker-row';
    row.innerHTML = '' +
        '<input type="text" class="form-input broker-host" style="flex:1" placeholder="localhost">' +
        '<input type="number" class="form-input broker-port" style="width:7rem" placeholder="9092">' +
        '<button class="btn btn-sm" onclick="testBroker(this)">Test</button>' +
        '<span class="broker-status"></span>' +
        '<button class="btn-icon btn-icon-danger" onclick="removeBrokerRow(this)" title="Remove">&#10005;</button>';
    document.getElementById('broker-rows').appendChild(row);
}

function removeBrokerRow(button) {
    const rows = document.querySelectorAll('.broker-row');
    if (rows.length <= 1) {
        rows[0].querySelector('.broker-host').value = '';
        rows[0].querySelector('.broker-port').value = '';
        rows[0].querySelector('.broker-status').textContent = '';
        return;
    }
    button.closest('.broker-row').remove();
}

async function testBroker(button) {
    const row = button.closest('.broker-row');
    const host = row.querySelector('.broker-host').value.trim();
    const port = row.querySelector('.broker-port').value.trim();
    const status = row.querySelector('.broker-status');
    if (!host || !port) {
        status.textContent = 'Enter host and port';
        return;
    }
    status.textContent = 'Testing...';
    try {
        const res = await ShingoEdge.api.post('/api/config/kafka/test', { broker: host + ':' + port });
        status.textContent = res.connected ? 'Connected' : (res.error || 'Failed');
    } catch (e) {
        status.textContent = String(e);
    }
}

async function saveIdentity() {
    try {
        await ShingoEdge.api.put('/api/config/station-id', {
            station_id: document.getElementById('station-id-input').value.trim()
        });
        ShingoEdge.toast('Station identity saved', 'success');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function saveWarLink() {
    try {
        const form = document.getElementById('warlink-form');
        await ShingoEdge.api.put('/api/config/warlink', {
            host: form.querySelector('[name="host"]').value.trim(),
            port: parseInt(form.querySelector('[name="port"]').value || '0', 10),
            poll_rate: form.querySelector('[name="poll_rate"]').value.trim(),
            mode: form.querySelector('[name="mode"]').value,
            enabled: form.querySelector('[name="enabled"]').checked
        });
        ShingoEdge.toast('WarLink config saved', 'success');
        location.reload();
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function saveMessaging() {
    try {
        await Promise.all([
            ShingoEdge.api.put('/api/config/messaging', { kafka_brokers: collectBrokers() }),
            ShingoEdge.api.put('/api/config/auto-confirm', { auto_confirm: document.getElementById('auto-confirm').checked })
        ]);
        ShingoEdge.toast('Messaging config saved', 'success');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

function backupFormData() {
    return ShingoEdge.getFormData('backup-form');
}

function backupFingerprint() {
    const data = backupFormData();
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

let testedBackupFingerprint = '';
const stationID = document.getElementById('page-data').dataset.stationId || '';

function setBackupConnectionStatus(message, ok) {
    const el = document.getElementById('backup-connection-status');
    el.innerHTML = (ok === true ? '<span class="status-badge status-connected" style="margin-right:0.5rem">Connected</span>' :
        ok === false ? '<span class="status-badge status-disconnected" style="margin-right:0.5rem">Failed</span>' : '') + message;
}

function setBackupOperationStatus(message, kind) {
    const el = document.getElementById('backup-operation-status');
    el.innerHTML = (kind === 'ok' ? '<span class="status-badge status-connected" style="margin-right:0.5rem">Ready</span>' :
        kind === 'busy' ? '<span class="status-badge" style="margin-right:0.5rem">Working</span>' :
        kind === 'error' ? '<span class="status-badge status-disconnected" style="margin-right:0.5rem">Error</span>' : '') + message;
}

async function testBackupConfig() {
    setBackupConnectionStatus('Testing backup storage connection...', null);
    try {
        await ShingoEdge.api.post('/api/backups/test', backupFormData());
        testedBackupFingerprint = backupFingerprint();
        setBackupConnectionStatus('Connection test succeeded.', true);
        ShingoEdge.toast('Backup connection succeeded', 'success');
    } catch (e) {
        setBackupConnectionStatus('Connection test failed: ' + e, false);
        ShingoEdge.toast('Backup test failed: ' + e, 'error');
    }
}

async function saveBackupConfig() {
    try {
        const data = backupFormData();
        if (data.enabled && testedBackupFingerprint !== backupFingerprint()) {
            throw 'run Test Connection after changing storage settings before enabling backups';
        }
        await ShingoEdge.api.put('/api/backups/config', data);
        setBackupOperationStatus('Backup settings saved.', 'ok');
        ShingoEdge.toast('Backup settings saved', 'success');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperationStatus('Failed to save backup settings: ' + e, 'error');
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function runBackupNow() {
    try {
        setBackupOperationStatus('Manual backup in progress...', 'busy');
        await ShingoEdge.api.post('/api/backups/run', {});
        setBackupOperationStatus('Manual backup completed successfully.', 'ok');
        ShingoEdge.toast('Backup completed', 'success');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperationStatus('Manual backup failed: ' + e, 'error');
        ShingoEdge.toast('Backup failed: ' + e, 'error');
    }
}

function formatMaybeDate(value) {
    if (!value) return '';
    const date = new Date(value);
    return isNaN(date) ? String(value) : date.toLocaleString();
}

function formatBytes(bytes) {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = bytes || 0;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return (unit === 0 ? String(value) : value.toFixed(1)) + ' ' + units[unit];
}

async function loadBackupStatus() {
    try {
        const status = await ShingoEdge.api.get('/api/backups/status');
        const lines = [];
        lines.push('<div><strong>Automatic Backups:</strong> ' + (status.enabled ? 'Enabled' : 'Disabled') + '</div>');
        lines.push('<div><strong>Scheduler:</strong> ' + (status.running ? 'Backup currently running' : 'Idle') + '</div>');
        if (status.last_success_at) lines.push('<div><strong>Last Success:</strong> ' + formatMaybeDate(status.last_success_at) + '</div>');
        if (status.last_failure_at) lines.push('<div><strong>Last Failure:</strong> ' + formatMaybeDate(status.last_failure_at) + '</div>');
        if (status.next_scheduled_at) lines.push('<div><strong>Next Scheduled Run:</strong> ' + formatMaybeDate(status.next_scheduled_at) + '</div>');
        document.getElementById('backup-status').innerHTML = lines.join('');
    } catch (e) {
        document.getElementById('backup-status').textContent = 'Backup status unavailable: ' + e;
    }
}

async function stageRestore(key) {
    const typed = window.prompt('Type the station ID to confirm restore.', stationID);
    if (typed !== stationID) {
        ShingoEdge.toast('Restore cancelled: station ID mismatch.', 'warning');
        return;
    }
    try {
        setBackupOperationStatus('Downloading and staging restore archive...', 'busy');
        await ShingoEdge.api.post('/api/backups/restore', { key: key });
        setBackupOperationStatus('Restore staged successfully. Restart shingo-edge to apply it.', 'ok');
        ShingoEdge.toast('Restore staged. Restart shingo-edge to apply it.', 'warning');
        await loadBackupStatus();
        await loadBackups();
    } catch (e) {
        setBackupOperationStatus('Restore staging failed: ' + e, 'error');
        ShingoEdge.toast('Restore staging failed: ' + e, 'error');
    }
}

async function loadBackups() {
    const body = document.getElementById('backup-body');
    body.innerHTML = '<tr><td colspan="4" class="empty-cell">Loading backups...</td></tr>';
    try {
        const items = await ShingoEdge.api.get('/api/backups');
        if (!items || !items.length) {
            body.innerHTML = '<tr><td colspan="4" class="empty-cell">No backups found for this station</td></tr>';
            return;
        }
        body.innerHTML = items.map(function(item) {
            const action = item.restore_pending
                ? '<span class="status-badge status-connected">Pending Restart</span>'
                : '<button class="btn btn-sm btn-danger" onclick="stageRestore(' + JSON.stringify(item.key).replace(/"/g, '&quot;') + ')">Restore On Restart</button>';
            return '<tr>' +
                '<td>' + ShingoEdge.escapeHtml(formatMaybeDate(item.created_at || item.last_modified || '')) + '</td>' +
                '<td>' + ShingoEdge.escapeHtml(formatBytes(item.size || 0)) + '</td>' +
                '<td><code>' + ShingoEdge.escapeHtml(item.key) + '</code></td>' +
                '<td>' + action + '</td>' +
                '</tr>';
        }).join('');
    } catch (e) {
        body.innerHTML = '<tr><td colspan="4" class="empty-cell">Failed to load backups: ' + ShingoEdge.escapeHtml(String(e)) + '</td></tr>';
    }
}

async function changePassword() {
    const oldPassword = document.getElementById('pw-old').value;
    const newPassword = document.getElementById('pw-new').value;
    const confirm = document.getElementById('pw-confirm').value;
    if (!newPassword) {
        ShingoEdge.toast('Enter a new password', 'warning');
        return;
    }
    if (newPassword !== confirm) {
        ShingoEdge.toast('New password confirmation does not match', 'warning');
        return;
    }
    try {
        await ShingoEdge.api.post('/api/config/password', {
            old_password: oldPassword,
            new_password: newPassword
        });
        document.getElementById('pw-old').value = '';
        document.getElementById('pw-new').value = '';
        document.getElementById('pw-confirm').value = '';
        ShingoEdge.toast('Password changed', 'success');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

// --- Core API ---

async function saveCoreAPI() {
    try {
        await ShingoEdge.api.put('/api/config/core-api', {
            core_api: document.getElementById('core-api-url').value.trim()
        });
        ShingoEdge.toast('Core API URL saved', 'success');
    } catch (e) {
        ShingoEdge.toast('Error: ' + e, 'error');
    }
}

async function testCoreAPI() {
    var status = document.getElementById('core-api-status');
    var url = document.getElementById('core-api-url').value.trim();
    if (!url) { status.textContent = 'Enter a URL'; return; }
    status.textContent = 'Testing...';
    try {
        var res = await ShingoEdge.api.post('/api/config/core-api/test', { core_api: url });
        status.textContent = res.connected ? 'Connected' : (res.error || 'Failed');
        status.style.color = res.connected ? 'var(--success, green)' : 'var(--danger, red)';
    } catch (e) {
        status.textContent = String(e);
        status.style.color = 'var(--danger, red)';
    }
}

loadBackupStatus();
loadBackups();
