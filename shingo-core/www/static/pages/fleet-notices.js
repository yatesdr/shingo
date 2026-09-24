// fleet-notices.js — keep only the fleet notices that are about THIS order.
//
// RDS notices are mostly a roll-call: one entry per robot, "AMR-06:<reason>",
// saying why each robot did not take the job. SPR's stored notices, 14 days to
// 2026-09-24, carry five reasons: "no joining order" (6674 entries), "order is
// not complete" (517), "no free container current" (292), "cannot append
// current task: not load task or container_count < ..." (157) and "robot is not
// dispatchable" (188). All five ride orders that finished normally.
//
// SO THE QUESTION DECIDES WHAT IS SHOWN, and it depends on whether the order
// has a robot:
//
//   - ASSIGNED: why the other robots passed is not a question anyone has. SPR
//     6903 showed AMR-06, AMR-04 and AMR-03's reasons while AMR-10 was
//     mid-unload. Only entries naming this order's robot, and entries naming no
//     robot, are kept; the roll-call reasons are dropped even for this robot.
//   - NOT ASSIGNED: the roll-call is the answer to "why is nobody taking it",
//     and it is summarised into one line: "No robot assigned yet — 9 robots
//     declined; AMR-05 not dispatchable". A reason that is not roll-call is
//     named per robot, because it is one somebody may need to act on.
//
// Shared by the order modal (orders.js) and the mission page (mission-detail.js)
// so the two never say different things about the same notice.

// The per-robot reasons that only mean "this robot did not take it".
const ROLL_CALL = [
  /^no joining order$/i,
  /^order is not complete$/i,
  /^no free container current$/i,
  /^cannot append current task: not load task or container_count/i,
];

// A robot entry is "<vehicle>:<reason>" with no space in the vehicle name, which
// is what tells it apart from a reason that itself contains a colon.
const ROBOT_ENTRY = /^([A-Za-z0-9_.-]+):\s*(.+)$/;

function isRollCall(reason) {
  return ROLL_CALL.some(function(re) { return re.test(reason.trim()); });
}

// Plain wording for the reasons worth a person's attention.
function plainReason(robot, reason) {
  if (/^robot is not dispatchable$/i.test(reason.trim())) return robot + ' not dispatchable';
  return robot + ': ' + reason.trim();
}

// relevantNotices filters one class of fleet message for an order. robotID is
// the order's robot, or '' when none is assigned. Strings and entries without a
// desc pass through untouched; a message left with nothing to say is dropped.
export function relevantNotices(items, robotID) {
  if (!items || !items.length) return [];
  return items.map(function(m) {
    if (!m || typeof m !== 'object' || !m.desc) return m;
    const kept = [];
    const others = [];
    let declined = 0;
    String(m.desc).split(';').forEach(function(raw) {
      const part = raw.trim();
      if (part === '') return;
      const entry = ROBOT_ENTRY.exec(part);
      if (!entry) { kept.push(part); return; }
      const robot = entry[1];
      const reason = entry[2];
      if (robotID) {
        if (robot === robotID && !isRollCall(reason)) kept.push(plainReason(robot, reason));
        return;
      }
      if (isRollCall(reason)) declined++;
      else others.push(plainReason(robot, reason));
    });
    if (!robotID && (declined > 0 || others.length > 0)) {
      let line = 'No robot assigned yet';
      const bits = [];
      if (declined > 0) bits.push(declined + (declined === 1 ? ' robot declined' : ' robots declined'));
      line += ' — ' + bits.concat(others).join('; ');
      kept.push(line);
    }
    if (kept.length === 0) return null;
    return { code: m.code, desc: kept.join('; '), times: m.times, timestamp: m.timestamp };
  }).filter(function(m) { return m !== null; });
}
