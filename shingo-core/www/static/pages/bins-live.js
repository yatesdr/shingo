// bins-live.js — keeping the Bins table's rows current while the page is open.
//
// The server draws the table once. Core then names what changed: a bin
// (bin-update, bin-row) or an order whose claim on its carts moved
// (order-update). The page hands each name to this feed, and the feed reads
// their rows in ONE request a moment later (GET /api/bins/rows), so a burst —
// a robot's drop emits several events, a running line a count every few
// seconds — costs one read. Names that arrive while a read is out wait for the
// next. A hidden tab collects names and reads once it is shown. After the live
// link dropped, events were missed and nothing says which, so the next read is
// every row.
//
// SPR 2026-11-19: a Bins page opened before a robot's 7:55 drop still showed
// the cart at its source half an hour later, and a search for the node it was
// dropped at found nothing.
//
// Pure: no DOM, no clock of its own (the caller passes timers);
// bins.row.test.js runs it under plain Node.

// MAX_NAMED is the most bins, or orders, one read names; more than that and
// the read is every row, which costs the same as drawing the page.
export var MAX_NAMED = 200;

// rowsQuery is the query string for one read: '' (every row) or
// "ids=1,2&orders=3".
export function rowsQuery(binIDs, orderIDs) {
  var parts = [];
  if (binIDs.length) parts.push('ids=' + binIDs.join(','));
  if (orderIDs.length) parts.push('orders=' + orderIDs.join(','));
  return parts.join('&');
}

// newRowsFeed builds the feed.
//   opts.read(query)          GET /api/bins/rows?<query>; resolves {rows, gone}
//   opts.apply(answer, every) paints an answer; every: it holds every row
//   opts.hidden()             true while the tab is hidden
//   opts.later(fn, ms), opts.cancel(handle)   timers
//   opts.quietMs              how long a burst must go quiet before it is read
//   opts.maxWaitMs            the longest a name waits while names keep coming
// It returns:
//   bin(id)    this bin's row changed
//   order(id)  this order's carts' rows changed
//   every()    the next read is every row
//   shown()    the tab was shown: read what waited while it was hidden
export function newRowsFeed(opts) {
  var bins = {};
  var orders = {};
  var wantEvery = false;
  var reading = false;
  var quiet = null;
  var cap = null;

  function waiting() {
    return wantEvery || Object.keys(bins).length > 0 || Object.keys(orders).length > 0;
  }

  function disarm() {
    if (quiet !== null) { opts.cancel(quiet); quiet = null; }
    if (cap !== null) { opts.cancel(cap); cap = null; }
  }

  // arm (re)starts the quiet timer and, for the first name of a burst, the cap.
  // A hidden tab arms nothing: shown() does.
  function arm() {
    if (opts.hidden()) return;
    if (quiet !== null) opts.cancel(quiet);
    quiet = opts.later(read, opts.quietMs);
    if (cap === null) cap = opts.later(read, opts.maxWaitMs);
  }

  function read() {
    disarm();
    if (reading || !waiting() || opts.hidden()) return;
    var ids = Object.keys(bins);
    var ords = Object.keys(orders);
    var every = wantEvery || ids.length > MAX_NAMED || ords.length > MAX_NAMED;
    var query = every ? '' : rowsQuery(ids, ords);
    bins = {};
    orders = {};
    wantEvery = false;
    reading = true;
    function done() {
      reading = false;
      if (waiting()) arm();
    }
    opts.read(query).then(
      function(answer) {
        try { opts.apply(answer, every); } finally { done(); }
      },
      // A failed read loses its names, so the next read is every row. Nothing
      // retries on its own: the next name arms it, and a Core that is down
      // drops the live link, whose return asks for every row anyway.
      function() { wantEvery = true; reading = false; });
  }

  function want(set, id) {
    var n = parseInt(id, 10);
    if (!(n > 0)) return;
    set[n] = true;
    arm();
  }

  return {
    bin: function(id) { want(bins, id); },
    order: function(id) { want(orders, id); },
    every: function() { wantEvery = true; arm(); },
    shown: function() { if (waiting()) arm(); }
  };
}
