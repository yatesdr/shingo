// cases.js — the helper pins' case lists for modal, confirm / prompt, toast
// and api (shared/testdata/helper-pins-harness/pins.js), run on the shared
// components (layer.js, notice.js, http.js) through surface.js. Run by
// shared/components_test.go (TestSharedComponentPins); prints one JSON object
// {golden file name: text} on stdout, compared with golden/.
//
// THE SAME CASES, so the goldens diff line for line with the hubs'
// (testdata/helper-pins/<x>.<surface>.txt): when wave C swaps a hub's names
// onto the components, that hub's golden moves to read as these do, header
// line aside.

'use strict';

const pins = require('../helper-pins-harness/pins.js');

const COMPONENT = {
    name: 'component', root: 'edge', entry: 'shared/testdata/components/surface.js',
    sharedEntry: 'shared/testdata/components/surface.js',
    file: 'shared/layer.js, notice.js, http.js',
    pick: (m) => ({ showModal: m.showModal, hideModal: m.hideModal, confirm: m.confirm, prompt: m.prompt, toast: m.toast }),
    words: { confirm: 'confirm', prompt: 'prompt' },
    sel: { dialog: '.layer-scrim', toastBox: '.notice-stack', toast: '.notice-toast' },
    doors: (probe) => ({
        shape: () => 'http is an ' + typeof probe.m.http + ' {' + Object.keys(probe.m.http).join(', ') + '}; send is a '
            + typeof probe.m.send + ' (method, url, body, opts); every door resolves, none throws',
        requests: [
            ['http.get(url)', (p) => p.m.http.get('/api/x')],
            ['http.post(url, {a:1})', (p) => p.m.http.post('/api/x', { a: 1 })],
            ['http.post(url)', (p) => p.m.http.post('/api/x')],
            ['http.post(url, null)', (p) => p.m.http.post('/api/x', null)],
            ['http.put(url, {a:1})', (p) => p.m.http.put('/api/x', { a: 1 })],
            ['http.patch(url, {a:1})', (p) => p.m.http.patch('/api/x', { a: 1 })],
            ['http.del(url)', (p) => p.m.http.del('/api/x')],
            ['send("POST", url, null)', (p) => p.m.send('POST', '/api/x', null)],
        ],
        throwing: [],
        safe: [
            ['http.get', (p) => p.m.http.get('/api/x')],
            ['http.post', (p) => p.m.http.post('/api/x', { a: 1 })],
            ['http.post {notice:false}', (p) => p.m.http.post('/api/x', { a: 1 }, { notice: false })],
        ],
    }),
};

(async function main() {
    const goldens = {};
    const golden = (name, lines) => { goldens[name] = lines.join('\n').replace(/[ \t]+$/gm, '') + '\n'; };
    golden('modal.component.txt', pins.modalPins(COMPONENT));
    golden('dialog.component.txt', await pins.dialogPins(COMPONENT));
    golden('toast.component.txt', await pins.toastPins(COMPONENT));
    golden('api.component.txt', await pins.apiPins(COMPONENT));
    process.stdout.write(JSON.stringify(goldens));
})().catch((e) => {
    console.error('component case lists crashed:', e && e.stack ? e.stack : e);
    process.exit(1);
});
