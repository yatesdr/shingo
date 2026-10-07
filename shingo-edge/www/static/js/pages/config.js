// config.js — the Edge Configuration page (U3).
//
// One page, one Save (docs/ui-style-guide/33-settings-pages.md). The settings
// in shingoedge.yaml go through ONE request, PUT /api/config, for the dirty
// sections only; the shifts live in the database and keep PUT /api/shifts,
// sent only when the shift rows are dirty (an own-save section). Actions that
// are not settings — Test connection, Refresh, Test brokers, Back up now,
// Show backups, Restore, Change password — are their own buttons and never
// touch the draft.
//
// A load makes no request: the live words, the restart notice and the backup
// status come in the render. The backup list is fetched only when storage is
// configured and someone opens it (E6).

import { api, delegateActions, prompt, showModal, hideModal } from '/static/js/shingoedge.js';
import {
    settingsPage, apiResult, durationFromText, collectList, el, formatClock, formatTime, toast,
} from '/static/shared/utils.js';
import { backupRow, backupWords, makeStageRestore, stationBody, stationErrors } from '/static/js/pages/config-backups.js';

const root = document.getElementById('config');
const $ = (sel) => root.querySelector(sel);
const $$ = (sel) => Array.from(root.querySelectorAll(sel));
const field = (name) => root.querySelector('[data-field="' + name + '"]');
const val = (name) => { const f = field(name); return f ? f.value.trim() : ''; };
const sw = (name) => { const b = root.querySelector('[data-sw="' + name + '"]'); return !!(b && b.classList.contains('on')); };
const setSw = (name, on) => {
    const b = root.querySelector('[data-sw="' + name + '"]');
    if (!b) return;
    b.classList.toggle('on', !!on);
    b.setAttribute('aria-checked', on ? 'true' : 'false');
};
const setVal = (name, v) => { const f = field(name); if (f) f.value = v === undefined || v === null ? '' : String(v); };

function readJSON(attr, fallback) {
    try { return JSON.parse(root.getAttribute(attr) || ''); } catch (e) { return fallback; }
}

const stationID = root.dataset.stationId || '';
// Set only on a legacy-only Edge (no UID): carried unchanged in the station body.
const legacyStationID = root.dataset.legacyStationId || '';
let backupStatus = readJSON('data-backup-status', { available: false });
let storageOpen = false;
let secretRemoved = false;

// ─── Rule 7: a row that is off hides what depends on it ──────────────────

function mode() {
    const on = root.querySelector('[data-field="mode"] button.on');
    return on ? on.dataset.mode : 'sse';
}

function storageConfigured() {
    return !!(backupStatus && backupStatus.configured);
}

function syncVisibility() {
    const plcOn = sw('plc_enabled');
    $$('[data-dep="plc_enabled"]').forEach((n) => { n.hidden = !plcOn; });
    $$('[data-poll]').forEach((n) => { n.hidden = mode() !== 'poll'; });
    const autoOn = sw('backup_enabled');
    $$('[data-dep="backup_enabled"]').forEach((n) => { n.hidden = !autoOn; });

    const configured = storageConfigured();
    const showStorage = configured || storageOpen;
    $$('[data-storage]').forEach((n) => { n.hidden = !showStorage; });
    $$('[data-needs-storage]').forEach((b) => { b.hidden = !showStorage; });
    $$('[data-restore]').forEach((n) => { n.hidden = !configured; });

    const toggle = $('#storage-toggle');
    const sub = $('#storage-sub');
    if (configured) {
        const where = [val('bucket'), val('endpoint')].filter(Boolean).join(' at ');
        sub.textContent = 'S3 storage for copies of this Edge\'s database and config · ' + (where || 'set up');
        toggle.hidden = true;
    } else {
        sub.textContent = 'S3 storage for copies of this Edge\'s database and config · none set up';
        toggle.hidden = false;
        toggle.textContent = storageOpen ? 'Hide' : 'Set up storage…';
    }
    $('#secret-remove').hidden = secretRemoved || field('secret_key').placeholder !== 'saved';
}

function drawBackupLive() {
    const w = backupWords(backupStatus, formatClock);
    const live = $('#backup-live');
    live.className = 'set-cnt' + (w.cls ? ' ' + w.cls : '');
    live.lastElementChild.textContent = w.text;
}

function drawShiftLive() {
    const n = $$('[data-shift-row]').length;
    $('#shift-live').lastElementChild.textContent = n ? n + ' of 3' : 'none set';
    $('#shift-add-btn').disabled = n >= 3;
    $('#shift-add-hint').textContent = n ? '' : 'No shifts yet';
    $$('[data-shift-row]').forEach((row, i) => {
        row.querySelector('label').firstChild.textContent = 'Shift ' + (i + 1);
    });
}

// ─── Sections ────────────────────────────────────────────────────────────

const intOrText = (s) => (/^\d+$/.test(s) ? parseInt(s, 10) : s);

const sections = {
    station: {
        read: () => ({
            station_uid: val('station_uid'),
            timezone: val('timezone'),
            auto_confirm: sw('auto_confirm'),
        }),
        write: (v) => {
            setVal('station_uid', v.station_uid);
            setVal('timezone', v.timezone);
            setSw('auto_confirm', v.auto_confirm);
        },
        validate: (d) => stationErrors(d, legacyStationID),
        body: (d) => stationBody(d, legacyStationID),
    },
    core: {
        read: () => ({ core_api: val('core_api') }),
        write: (v) => setVal('core_api', v.core_api),
    },
    plc: {
        read: () => ({
            enabled: sw('plc_enabled'),
            host: val('host'),
            port: val('port'),
            mode: mode(),
            poll_rate: val('poll_rate'),
        }),
        write: (v) => {
            setSw('plc_enabled', v.enabled);
            setVal('host', v.host);
            setVal('port', v.port);
            pickMode(v.mode);
            setVal('poll_rate', v.poll_rate);
        },
        validate: (d) => {
            const e = {};
            if (d.port !== '' && !/^\d+$/.test(d.port)) e.port = 'A port is a whole number.';
            if (d.mode === 'poll' && durationFromText(d.poll_rate) === null) e.poll_rate = 'Not a duration, e.g. 2 s.';
            return Object.keys(e).length ? e : null;
        },
        body: (d) => ({
            enabled: d.enabled,
            host: d.host,
            port: d.port === '' ? 0 : parseInt(d.port, 10),
            mode: d.mode,
            poll_rate: durationFromText(d.poll_rate) || '',
        }),
    },
    messaging: {
        read: () => ({
            brokers: collectList($('#broker-list'), '[data-broker-row]', (row) => row.querySelector('[data-broker]').value.trim()),
        }),
        write: (v) => {
            $$('[data-broker-row]').forEach((r) => r.remove());
            (v.brokers || []).forEach((b) => addBroker(b));
        },
    },
    backups: {
        read: () => ({
            enabled: sw('backup_enabled'),
            schedule_interval: val('schedule_interval'),
            keep_hourly: val('keep_hourly'),
            keep_daily: val('keep_daily'),
            keep_weekly: val('keep_weekly'),
            keep_monthly: val('keep_monthly'),
            endpoint: val('endpoint'),
            bucket: val('bucket'),
            region: val('region'),
            access_key: val('access_key'),
            secret_key: val('secret_key'),
            remove_secret: secretRemoved,
            use_path_style: sw('use_path_style'),
            insecure_skip_tls_verify: sw('insecure_skip_tls_verify'),
        }),
        write: (v) => {
            setSw('backup_enabled', v.enabled);
            ['schedule_interval', 'keep_hourly', 'keep_daily', 'keep_weekly', 'keep_monthly',
                'endpoint', 'bucket', 'region', 'access_key', 'secret_key'].forEach((k) => setVal(k, v[k]));
            secretRemoved = !!v.remove_secret;
            setSw('use_path_style', v.use_path_style);
            setSw('insecure_skip_tls_verify', v.insecure_skip_tls_verify);
        },
        validate: (d) => {
            const e = {};
            if (d.enabled && durationFromText(d.schedule_interval) === null) e.schedule_interval = 'Not a duration, e.g. 1 h.';
            ['keep_hourly', 'keep_daily', 'keep_weekly', 'keep_monthly'].forEach((k) => {
                if (d[k] !== '' && !/^\d+$/.test(d[k])) e[k] = 'How many to keep is a whole number.';
            });
            return Object.keys(e).length ? e : null;
        },
        body: (d) => ({
            enabled: d.enabled,
            schedule_interval: durationFromText(d.schedule_interval) || '',
            keep_hourly: intOrText(d.keep_hourly || '0'),
            keep_daily: intOrText(d.keep_daily || '0'),
            keep_weekly: intOrText(d.keep_weekly || '0'),
            keep_monthly: intOrText(d.keep_monthly || '0'),
            endpoint: d.endpoint,
            bucket: d.bucket,
            region: d.region,
            access_key: d.access_key,
            secret_key: d.secret_key,
            remove_secret: d.remove_secret,
            use_path_style: d.use_path_style,
            insecure_skip_tls_verify: d.insecure_skip_tls_verify,
        }),
    },
    shifts: {
        label: 'shifts',
        read: () => ({
            rows: $$('[data-shift-row]').map((row) => ({
                name: row.querySelector('[data-sh="name"]').value.trim(),
                start: row.querySelector('[data-sh="start"]').value,
                end: row.querySelector('[data-sh="end"]').value,
            })),
        }),
        write: (v) => {
            $$('[data-shift-row]').forEach((r) => r.remove());
            (v.rows || []).forEach((s) => addShift(s));
        },
        // The full desired set, in order: a removed row is simply absent
        // (apiSaveShifts deletes what is not sent).
        save: async (d) => {
            const shifts = [];
            d.rows.forEach((s, i) => {
                if (!s.name && !s.start && !s.end) return;
                shifts.push({ shift_number: i + 1, name: s.name, start_time: s.start, end_time: s.end });
            });
            const r = await apiResult('PUT', '/api/shifts', shifts);
            const b = (r.body && typeof r.body === 'object') ? r.body : {};
            return Object.assign({ ok: r.ok }, b, { ok: r.ok && b.ok !== false });
        },
    },
};

// ─── Draft controls (they change the draft, so they update the page) ─────

let page = null;
const changed = () => { syncVisibility(); if (page) page.update(); };

function flip(...args) {
    const btn = args[args.length - 2];
    const on = !btn.classList.contains('on');
    btn.classList.toggle('on', on);
    btn.setAttribute('aria-checked', on ? 'true' : 'false');
    changed();
}

function pickMode(m) {
    root.querySelectorAll('[data-field="mode"] button').forEach((b) => {
        const on = b.dataset.mode === (m === 'poll' ? 'poll' : 'sse');
        b.classList.toggle('on', on);
        b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
    changed();
}

function addBroker(value) {
    const row = el('div', { className: 'v', dataset: { brokerRow: '1' } }, [
        el('input', {
            className: 'set-inp mid', type: 'text', 'aria-label': 'Broker', spellcheck: 'false',
            autocomplete: 'off', placeholder: 'host:9092', dataset: { broker: '1' },
        }),
        el('button', { type: 'button', className: 'set-btn quiet', dataset: { action: 'removeBroker' } }, 'Remove'),
        el('span', { className: 'set-dim', role: 'status', dataset: { brokerStatus: '1' } }),
    ]);
    row.querySelector('input').value = typeof value === 'string' ? value : '';
    $('#broker-list').insertBefore(row, $('#broker-add'));
    if (typeof value !== 'string') row.querySelector('input').focus();
    changed();
}

function removeBroker(...args) {
    args[args.length - 2].closest('[data-broker-row]').remove();
    changed();
}

function addShift(values) {
    if ($$('[data-shift-row]').length >= 3) return;
    const v = (values && typeof values === 'object' && !values.nodeType) ? values : {};
    const row = el('div', { className: 'set-fld', dataset: { shiftRow: '1' } }, [
        el('label', null, ['Shift', el('small', null, 'name, start and end in plant time')]),
        el('div', { className: 'v' }, [
            el('input', { className: 'set-inp', type: 'text', placeholder: 'Name', 'aria-label': 'Shift name', dataset: { sh: 'name' } }),
            el('input', { className: 'set-inp', type: 'time', 'aria-label': 'Start', dataset: { sh: 'start' } }),
            el('span', { className: 'set-dim' }, 'to'),
            el('input', { className: 'set-inp', type: 'time', 'aria-label': 'End', dataset: { sh: 'end' } }),
            el('button', { type: 'button', className: 'set-btn quiet', dataset: { action: 'removeShift' } }, 'Remove'),
        ]),
    ]);
    row.querySelector('[data-sh="name"]').value = v.name || '';
    row.querySelector('[data-sh="start"]').value = v.start || '';
    row.querySelector('[data-sh="end"]').value = v.end || '';
    $('#shift-list').appendChild(row);
    drawShiftLive();
    changed();
}

function removeShift(...args) {
    args[args.length - 2].closest('[data-shift-row]').remove();
    drawShiftLive();
    changed();
}

function toggleStorage() {
    storageOpen = !storageOpen;
    syncVisibility();
}

function removeSecret() {
    secretRemoved = true;
    setVal('secret_key', '');
    field('secret_key').placeholder = 'none saved';
    changed();
}

// ─── Actions that are not settings ───────────────────────────────────────

function note(msg, ok) {
    const n = $('#backup-op');
    n.hidden = !msg;
    n.textContent = msg || '';
    n.className = ok === false ? 'set-err' : 'set-note';
}

async function testCore() {
    const out = $('#core-test');
    const url = val('core_api');
    if (!url) { out.textContent = 'Enter an address first.'; return; }
    out.textContent = 'Testing…';
    try {
        const res = await api.post('/api/config/core-api/test', { core_api: url });
        out.textContent = res.connected ? 'Core answered.' : ('No answer: ' + (res.error || 'failed') + '.');
    } catch (e) {
        out.textContent = String(e);
    }
}

async function syncWith(btn, url, what) {
    const out = $('#core-sync');
    btn.disabled = true;
    out.textContent = 'Asking Core for the ' + what + '…';
    try {
        await api.post(url);
        out.textContent = 'Asked; the ' + what + ' arrives with Core\'s next answer.';
    } catch (e) {
        out.textContent = 'Not sent: ' + e;
    }
    setTimeout(() => { btn.disabled = false; }, 1500);
}

function syncCoreNodes() { return syncWith($('#sync-nodes'), '/api/core-nodes/sync', 'node list'); }
function syncPayloadCatalog() { return syncWith($('#sync-catalog'), '/api/payload-catalog/sync', 'payload catalog'); }

async function testBrokers() {
    const rows = $$('[data-broker-row]');
    for (const row of rows) {
        const addr = row.querySelector('[data-broker]').value.trim();
        const out = row.querySelector('[data-broker-status]');
        if (!addr) { out.textContent = ''; continue; }
        out.textContent = 'Testing…';
        try {
            const res = await api.post('/api/config/kafka/test', { broker: addr });
            out.textContent = res.connected ? 'reachable' : ('not reachable: ' + (res.error || 'failed'));
        } catch (e) {
            out.textContent = String(e);
        }
    }
}

function storageDraft() {
    const d = sections.backups.read();
    return {
        endpoint: d.endpoint, bucket: d.bucket, region: d.region, access_key: d.access_key,
        secret_key: d.remove_secret ? '' : d.secret_key,
        use_path_style: d.use_path_style, insecure_skip_tls_verify: d.insecure_skip_tls_verify,
    };
}

async function testBackup() {
    note('Testing the storage…');
    const r = await apiResult('POST', '/api/backups/test', storageDraft());
    note(r.ok ? 'The storage answered.' : 'Storage test failed: ' + ((r.body && r.body.error) || 'no answer.'), r.ok);
}

async function refreshBackupStatus() {
    const r = await apiResult('GET', '/api/backups/status');
    if (r.ok && r.body && typeof r.body === 'object') {
        backupStatus = Object.assign({ available: true }, r.body);
    }
    drawBackupLive();
    syncVisibility();
}

async function runBackup() {
    note('Backing up…');
    const r = await apiResult('POST', '/api/backups/run', {});
    note(r.ok ? 'Backed up.' : 'Backup failed: ' + ((r.body && r.body.error) || 'no answer.'), r.ok);
    await refreshBackupStatus();
}

async function listBackups() {
    const list = $('#backup-list');
    const status = $('#backup-list-status');
    status.textContent = 'Loading…';
    const r = await apiResult('GET', '/api/backups');
    list.replaceChildren();
    if (!r.ok) {
        status.textContent = 'Could not list backups: ' + ((r.body && r.body.error) || 'no answer.');
        return;
    }
    const items = Array.isArray(r.body) ? r.body : [];
    status.textContent = items.length ? items.length + ' in storage' : 'none in storage for this station';
    items.forEach((item) => list.appendChild(backupRow(item, el, formatTime)));
}

const stageRestore = makeStageRestore({
    stationID,
    prompt,
    post: (url, body) => apiResult('POST', url, body),
    done: (msg, ok) => {
        $('#backup-list-status').textContent = msg;
        toast(msg, ok ? 'warning' : 'error');
        if (ok) listBackups();
    },
});

// ─── Account ─────────────────────────────────────────────────────────────

const pw = (k) => document.querySelector('#password-modal [data-pw="' + k + '"]');

function pwRefuse(msg) {
    const box = document.getElementById('pw-refusal');
    box.hidden = !msg;
    box.textContent = msg || '';
}

function openPassword() {
    pwRefuse('');
    showModal('password-modal');
    setTimeout(() => pw('old').focus(), 0);
}

function closePassword() {
    pwRefuse('');
    hideModal('password-modal');
}

async function changePassword() {
    const next = pw('new').value;
    if (!next) { pwRefuse('Enter a new password.'); return; }
    if (next !== pw('again').value) { pwRefuse('The two new passwords are not the same.'); return; }
    const r = await apiResult('POST', '/api/config/password', { old_password: pw('old').value, new_password: next });
    if (!r.ok) {
        pwRefuse('Not changed: ' + ((r.body && r.body.error) || 'no answer.'));
        return;
    }
    closePassword();
    toast('Password changed', 'success');
}

// ─── The page ────────────────────────────────────────────────────────────

drawShiftLive();
drawBackupLive();
syncVisibility();

page = settingsPage(root, {
    url: '/api/config',
    restart: readJSON('data-restart', []),
    sections,
    onSaved: (body, saved) => {
        // E3: no reload. E4: in sim the PLC apply is skipped on purpose.
        if (Array.isArray(body.simulated) && body.simulated.indexOf('PLC link') >= 0) {
            const live = $('#plc-live');
            live.className = 'set-cnt';
            live.lastElementChild.textContent = 'simulated';
        }
        if (saved.indexOf('backups') >= 0) {
            // The secret is never rendered: what was typed is now saved.
            const d = sections.backups.read();
            const box = field('secret_key');
            if (d.remove_secret) box.placeholder = 'none saved';
            else if (d.secret_key) box.placeholder = 'saved';
            box.value = '';
            secretRemoved = false;
            page.state.baseline.backups = JSON.stringify(sections.backups.read());
            refreshBackupStatus();
        }
        syncVisibility();
        page.update();
    },
});

// Discard writes the saved values back through each section's write(); the
// rows that depend on a switch follow after any click.
root.addEventListener('click', () => { Promise.resolve().then(() => { syncVisibility(); drawShiftLive(); }); });

delegateActions(document.body, {
    flip, pickMode, addBroker, removeBroker, addShift, removeShift, toggleStorage, removeSecret,
    testCore, syncCoreNodes, syncPayloadCatalog, testBrokers,
    testBackup, runBackup, listBackups, stageRestore,
    openPassword, closePassword, changePassword,
});
