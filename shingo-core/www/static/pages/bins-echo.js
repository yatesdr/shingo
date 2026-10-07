// bins-echo.js — telling the echo of the Bins page's own action apart from an
// update made elsewhere. Pure: no DOM, no clock of its own (callers pass
// `now`); bins.row.test.js runs it under plain Node.
//
// The bin-update event carries ids only (www/sse.go), so the page cannot tell
// its own echo from someone else's change by looking at it. It can count: a
// successful verb makes Core emit a known number of bin-update events for that
// bin, and the page drops exactly that many. Anything beyond the count is a
// real update and costs the one history-free read, as every update did before.

// ECHOES is how many bin-update events a successful verb emits for its bin,
// read off the emit sites in www/bin_actions.go: every verb emits once
// (emitBinUpdate, or binMove's own Emit), except add_note, which goes through
// BinService.AddNote and emits nothing. A refused verb returns before its
// emit, so a refusal emits none. If a handler there gains or loses an emit,
// this table must follow it.
export var ECHOES = {
  activate: 1,
  flag: 1,
  maintenance: 1,
  retire: 1,
  release: 1,
  stage: 1,
  lock: 1,
  unlock: 1,
  load_payload: 1,
  clear: 1,
  confirm_manifest: 1,
  unconfirm_manifest: 1,
  move: 1,
  record_count: 1,
  add_note: 0,
  update: 1
};

// echoesFor is the count for one verb. A verb not in the table is counted as
// one: at worst that costs one extra read, never a missed update.
export function echoesFor(action) {
  return Object.prototype.hasOwnProperty.call(ECHOES, action) ? ECHOES[action] : 1;
}

// newEchoLedger keeps one entry per own action per bin.
//   expect(id, n)      at send: n echoes for bin id may arrive from now on
//                      (the echo can beat the answer: Core emits before it
//                      writes the response). Returns the entry.
//   settle(entry, ok, now)
//                      at the answer. ok: the entry lives `expiryMs` more, so
//                      a lost echo cannot swallow a later real update. Not ok
//                      (refused, or the post failed): the verb emitted
//                      nothing, so the entry goes, and the number of events it
//                      had already taken is returned: those were real updates
//                      from elsewhere, and the caller reads the bin.
//   take(id, now)      on a bin-update for bin id: true when it is an
//                      expected echo (consumed), false when it is a real update.
export function newEchoLedger(expiryMs) {
  var entries = [];

  function live(e, now) {
    return e.got < e.want && (e.expires === null || now < e.expires);
  }

  return {
    expect: function(id, n) {
      var e = { id: id, want: n, got: 0, expires: null };
      entries.push(e);
      return e;
    },
    settle: function(e, ok, now) {
      var i = entries.indexOf(e);
      if (i === -1) return 0;
      if (ok) {
        e.expires = now + expiryMs;
        if (e.got >= e.want) entries.splice(i, 1);
        return 0;
      }
      entries.splice(i, 1);
      return e.got;
    },
    take: function(id, now) {
      for (var i = 0; i < entries.length; i++) {
        var e = entries[i];
        if (e.id !== id) continue;
        if (!live(e, now)) continue;
        e.got++;
        if (e.got >= e.want && e.expires !== null) entries.splice(i, 1);
        return true;
      }
      // Drop what can no longer take anything.
      entries = entries.filter(function(e) { return e.expires === null || (now < e.expires && e.got < e.want); });
      return false;
    },
    size: function() { return entries.length; }
  };
}
