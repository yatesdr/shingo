// Unit tests for sendBackCard (operator-modal.js): the tote press's SEND BACK
// line as the board renders the server's decision. It decides nothing: every
// state, word and flag comes from the view's send_back object.
//
// Runs under plain Node (no npm), extracting the SHIPPING functions out of
// operator-modal.js in a vm. Exit 0 = pass. Run via operator_modal_send_back_test.go.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function ok(cond, label) {
    if (cond) { passed++; return; }
    failed++;
    console.error('FAIL: ' + label);
}

function extractFn(src, name) {
    const start = src.indexOf('function ' + name + '(');
    if (start < 0) throw new Error('function ' + name + ' not found');
    const open = src.indexOf('{', start);
    let depth = 0;
    for (let j = open; j < src.length; j++) {
        if (src[j] === '{') depth++;
        else if (src[j] === '}') {
            depth--;
            if (depth === 0) return src.slice(start, j + 1);
        }
    }
    throw new Error('unbalanced braces in ' + name);
}

const modalSrc = fs.readFileSync(path.join(__dirname, 'operator-modal.js'), 'utf8');
const ctx = vm.createContext({ posting: new Set(), esc: (s) => String(s) });
for (const fn of ['actionBtn', 'sendBackCard']) {
    vm.runInContext(extractFn(modalSrc, fn), ctx);
}
vm.runInContext('this.sendBackCard = sendBackCard;', ctx);
const card = ctx.sendBackCard;

const enabled = card({ state: 'enabled', outside: 'P7-OUT', bin: 'CART-12', outbound: 'UNLOAD-A' }, 41);
ok(enabled.includes('Outside bin · P7-OUT · CART-12'), 'enabled names the outside and the cart');
ok(enabled.includes('Sends it to UNLOAD-A and brings tops &amp; bottoms'), 'enabled says where it goes');
ok(enabled.includes('data-action="/api/process-nodes/41/send-back"') && !enabled.includes(' disabled'), 'enabled acts on the route');

const disabled = card({ state: 'disabled', outside: 'P7-OUT', bin: 'CART-12', outbound: 'UNLOAD-A' }, 41);
ok(disabled.includes(' disabled'), 'disabled greys the button');

const waiting = card({ state: 'waiting', outside: 'P7-OUT', can_cancel: true,
    queue_reason: 'Waiting for tops & bottoms in SUPER-A' }, 41);
ok(waiting.includes('Waiting for tops & bottoms in SUPER-A'), "waiting shows the pull's own sentence");
ok(waiting.includes('data-action="/api/process-nodes/41/send-back/cancel"'), 'waiting offers CANCEL while it can act');
ok(!waiting.includes('/send-back"'), 'waiting offers no second SEND BACK');

const moving = card({ state: 'waiting', outside: 'P7-OUT', can_cancel: false }, 41);
ok(!moving.includes('/cancel'), 'no CANCEL when nothing unmoved is left');

console.log(passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
