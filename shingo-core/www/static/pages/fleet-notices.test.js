// Unit tests for relevantNotices (fleet-notices.js). Run under plain Node via
// the Go wrapper fleet_notices_test.go. Exit 0 on pass, 1 on any failure.
//
// The fixture is SPR order 6903's live notice (2026-09-24): three other robots'
// roll-call reasons, shown while AMR-10 was mid-unload on the order.

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let failures = 0;
function check(name, cond, detail) {
  if (cond) {
    console.log('  ok  ' + name);
  } else {
    failures++;
    console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
  }
}

const ctx = {};
vm.createContext(ctx);
const src = fs.readFileSync(path.join(__dirname, 'fleet-notices.js'), 'utf8')
  .replace(/^export\s+/gm, '');
vm.runInContext(src + '\nthis.relevantNotices = relevantNotices;', ctx);
const relevantNotices = ctx.relevantNotices;

const n6903 = [{ code: 80004, desc: 'AMR-06:cannot append current task: not load task or container_count < 1, 0, 0; ' +
  'AMR-04:order is not complete; AMR-03:cannot append current task: not load task or container_count < 1, 0, 0' }];

// Assigned: nothing about other robots survives.
let got = relevantNotices(n6903, 'AMR-10');
check('assigned order drops other robots\' roll-call', got.length === 0, JSON.stringify(got));

// Assigned: other robots' non-roll-call reasons are not this order's either.
got = relevantNotices([{ code: 80004, desc: 'AMR-05:robot is not dispatchable; AMR-06:no joining order' }], 'AMR-10');
check('assigned order drops another robot\'s not-dispatchable', got.length === 0, JSON.stringify(got));

// Assigned: an entry about THIS robot that is not roll-call is kept.
got = relevantNotices([{ code: 80004, desc: 'AMR-10:robot is not dispatchable; AMR-06:no joining order' }], 'AMR-10');
check('assigned order keeps its own robot\'s real reason',
  got.length === 1 && got[0].desc === 'AMR-10 not dispatchable', JSON.stringify(got));

// Unassigned: one summary line, the exception named.
got = relevantNotices([{ code: 80004, desc: 'AMR-01:no joining order; AMR-02:no joining order; ' +
  'AMR-03:no free container current; AMR-05:robot is not dispatchable' }], '');
check('unassigned order summarises the roll-call',
  got.length === 1 && got[0].desc === 'No robot assigned yet — 3 robots declined; AMR-05 not dispatchable',
  JSON.stringify(got));

// A message naming no robot is about the order and passes through.
got = relevantNotices([{ code: 60009, desc: 'Cannot find path to [SMN_011]' }], 'AMR-10');
check('robot-less message is kept', got.length === 1 && got[0].desc === 'Cannot find path to [SMN_011]',
  JSON.stringify(got));

// Strings (older rows) pass through.
got = relevantNotices(['legacy text'], 'AMR-10');
check('string entries pass through', got.length === 1 && got[0] === 'legacy text', JSON.stringify(got));

if (failures > 0) {
  console.log(failures + ' failure(s)');
  process.exit(1);
}
console.log('all fleet-notice assertions passed');
