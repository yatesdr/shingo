// The Core Configuration page (docs/ui-style-guide/33-settings-pages.md).
//
// One draft, one Save: settingsPage tracks each section, sends the dirty ones
// in ONE PUT /api/config, and draws the server's field errors under their
// rows. What is not a setting is its own button and never touches the draft:
// Test connection (POST /api/config/test-database), Send a test (the two
// /config/test-* doors, with the draft's mail settings), Change password
// (POST /config/password).
import {
    apiResult, collectList, delegateActions, durationFromText, durationToText,
    escapeHtml, hideModal, settingsPage, showModal,
} from '/static/shared/utils.js';

const root = document.getElementById('config-page');
const $ = (sel) => root.querySelector(sel);
const field = (key) => root.querySelector('[data-field="' + key + '"]');
const val = (key) => { const f = field(key); return f ? f.value.trim() : ''; };
const sw = (key) => { const b = root.querySelector('[data-switch="' + key + '"]'); return !!b && b.classList.contains('on'); };

// --- controls ---------------------------------------------------------------

function setSwitch(key, on) {
    const b = root.querySelector('[data-switch="' + key + '"]');
    if (!b) return;
    b.classList.toggle('on', !!on);
    b.setAttribute('aria-checked', on ? 'true' : 'false');
}

function segValue(key) {
    const on = root.querySelector('[data-seg="' + key + '"] button.on');
    return on ? on.dataset.val : '';
}

function setSeg(key, value) {
    root.querySelectorAll('[data-seg="' + key + '"] button').forEach((b) => {
        const on = b.dataset.val === value;
        b.classList.toggle('on', on);
        b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
}

// A row that is off hides what depends on it (rule 7), computed from state.
function refreshShown() {
    root.querySelectorAll('[data-shown-by]').forEach((row) => {
        row.hidden = !sw(row.dataset.shownBy);
    });
}

// --- lists (rule 9; C1: read from the rows on the page, never by index) ------

function listRow(kind, value) {
    const row = document.createElement('div');
    row.className = 'v';
    row.dataset.row = kind;
    const placeholder = kind === 'broker' ? 'kafka:9092' : 'name@company.com';
    const label = kind === 'broker' ? 'Broker' : 'Recipient';
    row.innerHTML = '<input class="set-inp mid" type="text" spellcheck="false" autocomplete="off" placeholder="' +
        placeholder + '" aria-label="' + label + '" value="' + escapeHtml(value || '') + '">' +
        '<button type="button" class="set-btn quiet" data-action="removeRow">Remove</button>';
    return row;
}

function readList(containerID, kind, errPrefix) {
    const box = document.getElementById(containerID);
    let i = 0;
    return collectList(box, '[data-row="' + kind + '"]', (row) => {
        const inp = row.querySelector('input');
        const v = inp.value.trim();
        // The server keys a bad row's error by its place in the list sent.
        if (v && errPrefix) inp.dataset.field = errPrefix + (i++);
        else delete inp.dataset.field;
        return v;
    });
}

function writeList(containerID, kind, values) {
    const box = document.getElementById(containerID);
    box.querySelectorAll('[data-row="' + kind + '"], [data-empty]').forEach((r) => r.remove());
    const addRow = box.lastElementChild;
    (values || []).forEach((v) => box.insertBefore(listRow(kind, v), addRow));
    showEmpty(box, kind);
}

// An empty recipient list says so (as the server draws it).
function showEmpty(box, kind) {
    if (kind !== 'recipient' || box.querySelector('[data-row], [data-empty]')) return;
    const empty = document.createElement('div');
    empty.className = 'v';
    empty.dataset.empty = kind;
    empty.innerHTML = '<span class="set-dim">nobody yet</span>';
    box.insertBefore(empty, box.lastElementChild);
}

function addRow(containerID, kind) {
    const box = document.getElementById(containerID);
    const empty = box.querySelector('[data-empty]');
    if (empty) empty.remove();
    const row = listRow(kind, '');
    box.insertBefore(row, box.lastElementChild);
    row.querySelector('input').focus();
}

// --- secrets (rule 8; C4) ---------------------------------------------------

// A password box is never filled: blank keeps the saved one. Remove marks it
// to be cleared on Save; typing a new one undoes the mark.
function secretState(name) {
    const inp = root.querySelector('[data-secret="' + name + '"]');
    return { value: inp ? inp.value : '', clear: !!inp && inp.dataset.clear === '1' };
}

function writeSecret(name, st) {
    const inp = root.querySelector('[data-secret="' + name + '"]');
    if (!inp) return;
    inp.value = st.value || '';
    if (st.clear) inp.dataset.clear = '1'; else delete inp.dataset.clear;
    const saved = inp.dataset.saved === '1';
    inp.placeholder = st.clear ? 'removed on save' : (saved ? 'saved' : 'none saved');
    const btn = root.querySelector('[data-secret-for="' + name + '"]');
    if (btn) btn.hidden = st.clear;
}

// --- durations (rule 5) -------------------------------------------------------

function durErr(errors, key, text, required) {
    const d = durationFromText(text);
    if (d === null || (required && d === '')) errors[key] = 'Not a duration. Type it with its unit, like 45 min or 10 s.';
    return d;
}

function wholeMinutes(text) {
    const d = durationFromText(text);
    if (!d) return null;
    const m = /^(?:(\d+)h)?(?:(\d+)m)?0s$/.exec(d);
    if (!m) return null;
    return (parseInt(m[1] || '0', 10) * 60) + parseInt(m[2] || '0', 10);
}

// --- sections ---------------------------------------------------------------

const sections = {
    plant: {
        read: () => ({ timezone: val('timezone') }),
        write: (d) => { field('timezone').value = d.timezone; },
        body: (d) => ({ timezone: d.timezone }),
    },
    fleet: {
        read: () => ({
            base_url: val('fleet_base_url'), timeout: val('fleet_timeout'),
            poll_interval: val('fleet_poll_interval'), fault_grace: val('fleet_fault_grace'),
        }),
        write: (d) => {
            field('fleet_base_url').value = d.base_url;
            field('fleet_timeout').value = d.timeout;
            field('fleet_poll_interval').value = d.poll_interval;
            field('fleet_fault_grace').value = d.fault_grace;
        },
        validate: (d) => {
            const e = {};
            durErr(e, 'fleet_timeout', d.timeout, true);
            durErr(e, 'fleet_poll_interval', d.poll_interval, true);
            durErr(e, 'fleet_fault_grace', d.fault_grace, true);
            return Object.keys(e).length ? e : null;
        },
        body: (d) => ({
            base_url: d.base_url, timeout: durationFromText(d.timeout),
            poll_interval: durationFromText(d.poll_interval), fault_grace: durationFromText(d.fault_grace),
        }),
    },
    messaging: {
        read: () => ({
            brokers: readList('cfg-brokers', 'broker', 'kafka_broker_'),
            group_id: val('group_id'), orders_topic: val('orders_topic'), dispatch_topic: val('dispatch_topic'),
        }),
        write: (d) => {
            writeList('cfg-brokers', 'broker', d.brokers);
            field('group_id').value = d.group_id;
            field('orders_topic').value = d.orders_topic;
            field('dispatch_topic').value = d.dispatch_topic;
        },
    },
    notifications: {
        read: () => ({
            enabled: sw('notif_enabled'), smtp_host: val('notif_smtp_host'), smtp_port: val('notif_smtp_port'),
            smtp_tls: sw('notif_smtp_tls'), smtp_user: val('notif_smtp_user'), secret: secretState('smtp'),
            from_address: val('notif_from_address'), recipients: readList('cfg-recipients', 'recipient', ''),
            throttle: val('notif_throttle_minutes'),
        }),
        write: (d) => {
            setSwitch('notif_enabled', d.enabled);
            field('notif_smtp_host').value = d.smtp_host;
            field('notif_smtp_port').value = d.smtp_port;
            setSwitch('notif_smtp_tls', d.smtp_tls);
            field('notif_smtp_user').value = d.smtp_user;
            writeSecret('smtp', d.secret);
            field('notif_from_address').value = d.from_address;
            writeList('cfg-recipients', 'recipient', d.recipients);
            field('notif_throttle_minutes').value = d.throttle;
            refreshShown();
        },
        validate: (d) => (wholeMinutes(d.throttle) === null
            ? { notif_throttle_minutes: 'Type a whole number of minutes, like 15 min.' } : null),
        body: notificationsBody,
    },
    fire_alarm: {
        read: () => ({ enabled: sw('fa_enabled'), auto_resume_default: sw('fa_auto_resume') }),
        write: (d) => {
            setSwitch('fa_enabled', d.enabled);
            setSwitch('fa_auto_resume', d.auto_resume_default);
            refreshShown();
        },
    },
    database: {
        read: readDatabase,
        write: (d) => {
            field('pg_host').value = d.host;
            field('pg_port').value = d.port;
            field('pg_database').value = d.database;
            field('pg_user').value = d.user;
            writeSecret('pg', d.secret);
            setSeg('pg_sslmode', d.sslmode);
            field('pg_max_open_conns').value = d.max_open_conns;
            field('pg_max_idle_conns').value = d.max_idle_conns;
            field('pg_conn_max_lifetime').value = d.lifetime;
        },
        validate: (d) => {
            const e = {};
            durErr(e, 'pg_conn_max_lifetime', d.lifetime, true);
            return Object.keys(e).length ? e : null;
        },
        body: databaseBody,
    },
};

function notificationsBody(d) {
    return {
        enabled: d.enabled, smtp_host: d.smtp_host, smtp_port: d.smtp_port, smtp_tls: d.smtp_tls,
        smtp_user: d.smtp_user, smtp_password: d.secret.value, clear_smtp_password: d.secret.clear && !d.secret.value,
        from_address: d.from_address, recipients: d.recipients, throttle_minutes: String(wholeMinutes(d.throttle)),
    };
}

function readDatabase() {
    return {
        host: val('pg_host'), port: val('pg_port'), database: val('pg_database'), user: val('pg_user'),
        secret: secretState('pg'), sslmode: segValue('pg_sslmode'),
        max_open_conns: val('pg_max_open_conns'), max_idle_conns: val('pg_max_idle_conns'),
        lifetime: val('pg_conn_max_lifetime'),
    };
}

function databaseBody(d) {
    return {
        host: d.host, port: d.port, database: d.database, user: d.user,
        password: d.secret.value, clear_password: d.secret.clear && !d.secret.value, sslmode: d.sslmode,
        max_open_conns: d.max_open_conns, max_idle_conns: d.max_idle_conns,
        conn_max_lifetime: durationFromText(d.lifetime) || d.lifetime,
    };
}

// The duration boxes show what Go wrote the way a person types it, so a value
// the server rendered and one the page wrote compare equal.
root.querySelectorAll('[data-field="fleet_timeout"], [data-field="fleet_poll_interval"], [data-field="fleet_fault_grace"], [data-field="pg_conn_max_lifetime"]').forEach((inp) => {
    const t = durationToText(durationFromText(inp.value) || inp.value);
    if (t) inp.value = t;
});

let restart = [];
try { restart = JSON.parse(root.dataset.restart || '[]'); } catch (e) { restart = []; }

const page = settingsPage(root, {
    url: '/api/config',
    restart,
    sections,
    onSaved: (body, saved) => afterSave(saved),
});

// After a save: the saved secrets are now "saved" or gone, and the summaries
// and live words that are computed from the page follow what was saved.
function afterSave(saved) {
    ['smtp', 'pg'].forEach((name) => {
        const inp = root.querySelector('[data-secret="' + name + '"]');
        if (!inp) return;
        const sectionSaved = saved.indexOf(name === 'pg' ? 'database' : 'notifications') >= 0;
        if (!sectionSaved) return;
        if (inp.value) inp.dataset.saved = '1';
        else if (inp.dataset.clear === '1') inp.dataset.saved = '';
        writeSecret(name, { value: '', clear: false });
    });
    $('#cfg-topics-now').textContent = [val('group_id'), val('orders_topic'), val('dispatch_topic')].join(' · ');
    $('#cfg-pool-now').textContent = val('pg_max_open_conns') + ' open · ' + val('pg_max_idle_conns') +
        ' idle · each replaced after ' + val('pg_conn_max_lifetime');
    liveWords('cfg-notif-words', sw('notif_enabled'), 'email alerts on');
    liveWords('cfg-fa-words', sw('fa_enabled'), 'on');
    // The secrets' boxes changed under settingsPage: take the page as saved.
    saved.forEach((n) => { page.state.baseline[n] = JSON.stringify(sections[n].read()); });
    page.update();
}

function liveWords(id, on, words) {
    const el = document.getElementById(id);
    if (!el) return;
    el.classList.toggle('ok', on);
    el.lastChild.textContent = on ? words : 'off';
}

// --- actions ----------------------------------------------------------------

// resultLine puts one answer under a row, as the page draws a field error.
function resultLine(row, ok, text) {
    let line = row.nextElementSibling;
    if (!line || !line.dataset.result) {
        line = document.createElement('div');
        line.dataset.result = '1';
        row.parentNode.insertBefore(line, row.nextSibling);
    }
    line.className = ok ? 'set-note' : 'set-err';
    line.setAttribute('role', ok ? 'status' : 'alert');
    line.textContent = text;
}

async function testDatabase(btn) {
    const d = readDatabase();
    btn.disabled = true;
    btn.textContent = 'Testing…';
    const res = await apiResult('POST', '/api/config/test-database', databaseBody(d));
    btn.disabled = false;
    btn.textContent = 'Test connection';
    const b = res.body || {};
    const ok = res.ok && b.ok === true;
    const why = b.errors && Object.keys(b.errors).length ? Object.values(b.errors).join(' ') : (b.error || b.message);
    resultLine(field('pg_host').closest('.set-fld'), ok,
        ok ? (b.message || 'Connected.') : 'Did not connect: ' + (why || 'the server answered ' + res.status + '.'));
}

async function sendTest(btn) {
    const kind = btn.dataset.kind;
    const url = kind === 'email' ? '/config/test-email' : '/config/test-alert?type=' + encodeURIComponent(kind);
    btn.disabled = true;
    btn.textContent = 'Sending…';
    const res = await apiResult('POST', url, notificationsBody(sections.notifications.read()));
    btn.disabled = false;
    btn.textContent = 'Send';
    const b = res.body || {};
    const ok = res.ok && b.ok === true;
    resultLine(btn.closest('.set-fld'), ok, (b && (b.message || b.error)) || ('The server answered ' + res.status + '.'));
}

async function changePassword() {
    const modal = document.getElementById('cfg-password-modal');
    const get = (k) => modal.querySelector('[data-pw="' + k + '"]').value;
    const row = (k) => modal.querySelector('[data-pw="' + k + '"]').closest('.set-fld');
    modal.querySelectorAll('[data-result]').forEach((l) => l.remove());
    if (!get('new')) { resultLine(row('new'), false, 'Type a new password.'); return; }
    if (get('new') !== get('again')) { resultLine(row('again'), false, 'The two new passwords are not the same.'); return; }
    const res = await apiResult('POST', '/config/password', { old_password: get('old'), new_password: get('new') });
    const b = res.body || {};
    if (res.ok && b.ok) {
        hideModal('cfg-password-modal');
        resultLine(root.querySelector('[data-action="openPasswordModal"]').closest('.set-fld'), true, 'Password changed.');
        return;
    }
    const msg = b.message || b.error || ('The server answered ' + res.status + '.');
    resultLine(row(/current/.test(msg) ? 'old' : 'new'), false, msg.charAt(0).toUpperCase() + msg.slice(1) + '.');
}

function toggleMore(btn) {
    const open = btn.getAttribute('aria-expanded') !== 'true';
    btn.setAttribute('aria-expanded', open ? 'true' : 'false');
    btn.textContent = open ? 'Hide' : 'Change…';
    root.querySelectorAll('[data-more-of="' + btn.dataset.more + '"]').forEach((r) => { r.hidden = !open; });
}

function clearModalResults(id) {
    document.getElementById(id).querySelectorAll('[data-result]').forEach((l) => l.remove());
}

delegateActions(document.body, {
    addBroker: () => addRow('cfg-brokers', 'broker'),
    addRecipient: () => addRow('cfg-recipients', 'recipient'),
    removeRow: (btn) => {
        const row = btn.closest('[data-row]');
        const box = row.parentElement;
        row.remove();
        showEmpty(box, row.dataset.row);
        page.update();
    },
    removeSecret: (btn) => {
        writeSecret(btn.dataset.secretFor, { value: '', clear: true });
        page.update();
    },
    toggleMore,
    testDatabase,
    openTestModal: () => { clearModalResults('cfg-test-modal'); showModal('cfg-test-modal', { closeOnBackdrop: true }); },
    openPasswordModal: () => { clearModalResults('cfg-password-modal'); showModal('cfg-password-modal'); },
    closeModal: (btn) => hideModal(btn.dataset.modal),
    sendTest,
    changePassword,
});

// Switches and segments are buttons: one click flips them, then the rows that
// depend on them follow (settingsPage re-checks dirty on the same click).
root.addEventListener('click', (e) => {
    const s = e.target.closest('[data-switch]');
    if (s) {
        setSwitch(s.dataset.switch, !s.classList.contains('on'));
        refreshShown();
        return;
    }
    const seg = e.target.closest('[data-seg] button');
    if (seg) setSeg(seg.parentElement.dataset.seg, seg.dataset.val);
});
// Typing a new password undoes a Remove.
root.addEventListener('input', (e) => {
    const inp = e.target.closest('[data-secret]');
    if (inp && inp.value && inp.dataset.clear === '1') writeSecret(inp.dataset.secret, { value: inp.value, clear: false });
});
