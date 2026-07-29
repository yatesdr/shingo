import { api, h, onSSE } from '/static/shared/utils.js';

// Sourcing page — direction B (two-pane): a process rail on the left, that
// process's changeover detail on the right.
//
// SSR renders every pane; this module switches which one is visible. Without JS
// the panes stack and the page still reads — the .src-js class is what opts into
// one-at-a-time, so nothing is hidden until something can unhide it.

const root = document.getElementById('src-root');
if (root) {
  const tabs = Array.from(root.querySelectorAll('.src-rrow'));
  const panes = Array.from(root.querySelectorAll('.src-pane'));

  // Only now is hiding safe.
  root.classList.add('src-js');

  const SEL_KEY = 'sourcing:selected-process';

  function select(processID, { remember = true } = {}) {
    let matched = false;
    for (const t of tabs) {
      const on = t.dataset.process === processID;
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      if (on) matched = true;
    }
    for (const p of panes) {
      p.hidden = p.dataset.process !== processID;
    }
    if (matched && remember) {
      try { sessionStorage.setItem(SEL_KEY, processID); } catch { /* private mode */ }
    }
    return matched;
  }

  for (const t of tabs) {
    t.addEventListener('click', () => select(t.dataset.process));
  }

  // Blocked-style links in the unlock-impact panel jump to the process's pane.
  // The target may live in the collapsed not-set-up tail, so open any <details>
  // ancestor of its rail row first, then select it. Delegated so it survives the
  // SSE reload (the whole page re-renders).
  document.addEventListener('click', (e) => {
    const link = e.target.closest('[data-goto-process]');
    if (!link) return;
    e.preventDefault();
    const proc = link.dataset.gotoProcess;
    const row = tabs.find((t) => t.dataset.process === proc);
    if (row && typeof row.closest === 'function') {
      const details = row.closest('details');
      if (details) details.open = true;
    }
    select(proc);
    if (root.scrollIntoView) root.scrollIntoView({ behavior: 'smooth', block: 'start' });
  });

  // role="tablist" implies arrow-key movement; a rail that traps keyboard users
  // on the first process is worse than no roles at all.
  root.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    const i = tabs.indexOf(document.activeElement);
    if (i < 0) return;
    e.preventDefault();
    const next = tabs[(i + (e.key === 'ArrowDown' ? 1 : tabs.length - 1)) % tabs.length];
    next.focus();
    select(next.dataset.process);
  });

  // Restore the operator's process across the SSE-driven reloads below, so a
  // pool change somewhere else on the plant does not yank them back to the
  // first process mid-decision.
  let restored = false;
  try {
    const saved = sessionStorage.getItem(SEL_KEY);
    if (saved) restored = select(saved, { remember: false });
  } catch { /* private mode */ }
  if (!restored && tabs.length) select(tabs[0].dataset.process, { remember: false });

  // ── Live updates ────────────────────────────────────────────────────────
  // The page is server-rendered, so refreshing means re-requesting it. Every
  // trigger below coalesces through one timer: whichever fires first wins, and
  // a second trigger inside the window is absorbed rather than queued.
  //
  // A reload is a full navigation, not JS work, so it cannot stall the main
  // thread — but reloading a HIDDEN tab is pure waste: a backgrounded sourcing
  // page reloading every 30s on bin churn burns server renders nobody is
  // looking at, and a background tab churning under an attached debugger is the
  // most likely thing behind the "renderer hangs during inspection" report.
  // So when a reload comes due on a hidden tab, defer it and fire once when the
  // operator returns — the page they come back to is current, and it never
  // reloads while unwatched.
  let pending = null;
  let deferredWhileHidden = false;
  function reloadNow() {
    if (document.hidden) {
      deferredWhileHidden = true;
      pending = null;
      return;
    }
    window.location.reload();
  }
  function scheduleReload(delayMs) {
    if (pending) return;
    pending = setTimeout(reloadNow, delayMs);
  }
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && deferredWhileHidden) {
      deferredWhileHidden = false;
      window.location.reload();
    }
  });

  // PRIMARY — sourcing-update fires only when a sourceability VERDICT moved.
  // That is precisely what this page displays, so it reloads promptly on it.
  const VERDICT_MS = 2000;
  onSSE('sourcing-update', () => scheduleReload(VERDICT_MS));

  // FALLBACK — bin/inventory movement, coalesced hard.
  //
  // These are kept, but slowly, and the reasoning matters: sourcing-update
  // covers every VERDICT change, but the page also renders per-claim Free/Held
  // counts and those move when a bin does WITHOUT changing any verdict (free
  // 5→4, still green). So they still earn their place — for number drift only,
  // which is not urgent.
  //
  // The window is 30s rather than the ~5s first considered. This is a
  // throttle, not a debounce: it fires at most once per window, so 5s would
  // still permit 12 reloads a minute on a plant where bins move constantly —
  // the strobing this is meant to end. Anything worth seeing sooner arrives on
  // sourcing-update.
  const DRIFT_MS = 30000;
  onSSE('bin-update', () => scheduleReload(DRIFT_MS));
  onSSE('inventory-update', () => scheduleReload(DRIFT_MS));

  // RECONNECT ONLY — never on first connect.
  //
  // This page shipped with onSSE('connected', reload), which is an infinite
  // loop: load → SSE connects → connected fires → reload → connects again.
  // The page pulsed forever on an idle plant (field-observed at Springfield).
  // A reload is only warranted after a connection was LOST, because events
  // missed while disconnected may have changed a verdict; the first connect of
  // a fresh page has missed nothing — the server just rendered it.
  let everConnected = false;
  let droppedSinceConnect = false;
  onSSE('disconnected', () => {
    if (everConnected) droppedSinceConnect = true;
  });
  onSSE('connected', () => {
    if (everConnected && droppedSinceConnect) {
      droppedSinceConnect = false;
      scheduleReload(VERDICT_MS);
      return;
    }
    everConnected = true;
  });
}

// ── Verdict history (7.2 / S4) ───────────────────────────────────────────────
//
// GET /api/sourceability/events shipped in 4975247c with, in the commit's own
// words, "No new page". Both recompute paths already write to it
// (SourceabilityMonitor.recomputeKeys and .recomputeAll both call
// persistChanges), so this is a read side over a live table and nothing on the
// write path changes.
//
// WHAT THE ROW ACTUALLY IS, because the shape does not match a casual reading of
// "filterable by since/process/payload/limit":
//
//   - ?payload= filters `missing_payload`, not `payload_code`.
//   - `payload_code` and `missing_payload` are written from the SAME variable
//     (store/sourceability/events.go RecordChange passes `missing` to both $3
//     and $7), so they are identical in every row. `payload_code` here does NOT
//     mean "the style's payload"; it means "the first missing payload".
//   - both are EMPTY on a recovery, because both derive from s.Missing and a
//     green verdict is missing nothing. So ?payload=X returns the failures and
//     never the recoveries, and the endpoint's own promise — "went unsourceable
//     at 09:14 missing X, recovered 09:41" — cannot be answered by a
//     payload-filtered query. It needs the rows for one (process, style) in
//     order, which is what this panel fetches.
//   - only the FIRST missing payload gets a column. The full list is in
//     `reason` ("missing A, B") and in metadata.missing.
//   - `metadata` arrives as a JSON STRING (the query does
//     COALESCE(metadata::text,'')) and can be ''. It is not an object.
//   - op / source / actor are the same three constants on every row.
//
// One request grouped client-side, rather than one per process: the table is
// edge-triggered so steady-state volume is near zero, and N panes would mean N
// round trips for a panel most readers glance at once.
const HISTORY_LIMIT = 400;

// fmtHeld formats a duration the way the number doctrine asks: compound, never
// decimal, and a measured zero is a number. Returns null when the inputs cannot
// support a duration, so the caller renders an absence instead of a value.
//
// NOT shared/utils.js formatDuration, deliberately. That helper opens with
// `if (!ms || ms <= 0) return '-'`, which renders a real measured zero as the
// absence glyph — two verdict changes inside the same second is a genuine 0 s,
// and the doctrine's load-bearing rule is that zero, no-data and n/a must not
// look alike. Fixing it in shared/ would move every duration on every surface;
// this panel formats its own and the defect is recorded here.
function fmtHeld(ms) {
    if (!(ms >= 0)) return null;
    const s = Math.round(ms / 1000);
    if (s < 60) return s + ' s';
    const m = Math.floor(s / 60);
    if (m < 60) return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
    const hh = Math.floor(m / 60);
    return hh + 'h ' + String(m % 60).padStart(2, '0') + 'm';
}

// verdictPill reuses this page's own pill classes, so the history and the live
// verdict above it read as one vocabulary rather than two. An unrecognised
// status renders AS ITSELF: the vocabulary has four values today, and a default
// arm that rendered blank would turn the fifth into a silent data-loss bug.
function verdictPill(status) {
    const known = {
        green: ['src-v-green', '● Sourcing'],
        yellow: ['src-v-yellow', '▲ At risk'],
        red: ['src-v-red', '✕ Blocked'],
        not_configured: ['src-v-unset', '○ Not set up'],
    };
    const hit = known[status];
    if (hit) return h`<span class="badge badge-sm src-v ${hit[0]}">${hit[1]}</span>`;
    return h`<span class="badge badge-sm src-v src-v-nodata" title="Status is not in this page's vocabulary — shown as the server sent it">${status || '(blank)'}</span>`;
}

// renderHistory fills one pane. `rows` are that process's events, newest first.
function renderHistory(host, rows, since, truncated) {
    if (!rows.length) {
        // An empty state says what is missing and over what window. A drawn
        // table frame with no rows reads as "measured, nothing happened", which
        // is a different claim from "no verdict has moved".
        host.innerHTML = h`<p class="src-none">No verdict change recorded for this process since ${since}. A row is written only when a verdict actually moves, so a quiet process is the normal case.</p>`;
        return;
    }

    const body = rows.map((r, i) => {
        // The state THIS row established held until the next NEWER change.
        // i === 0 is the newest, so that state is the one in force now.
        const newer = rows[i - 1];
        let held;
        if (!newer) {
            const d = fmtHeld(Date.now() - new Date(r.observed_at).getTime());
            held = d === null
                ? h`<span class="src-hist-nodata" title="This row is stamped in the future relative to this browser — a clock difference between Core and this machine, not a measurement">&mdash;</span>`
                : h`<span class="src-hist-now">current</span>` + h` <span class="src-hist-nodata">(${d})</span>`;
        } else {
            const d = fmtHeld(new Date(newer.observed_at).getTime() - new Date(r.observed_at).getTime());
            held = d === null
                ? h`<span class="src-hist-nodata" title="The next change is stamped earlier than this one — rows out of order, not a duration">&mdash;</span>`
                : h`${d}`;
        }

        // Missing payload is absent on a recovery BY CONSTRUCTION, not because
        // anything failed to arrive — so it reads n/a, not the em dash that
        // means "we have not heard".
        const miss = r.missing_payload
            ? h`${r.missing_payload}`
            : h`<span class="src-hist-nodata" title="A recovered or unconfigured verdict is missing nothing, so this column does not apply to the row">n/a</span>`;

        // `reason` carries the FULL missing list; the column carries only the
        // first. Say so rather than truncating silently.
        const more = (r.reason && r.missing_payload && r.reason !== 'missing ' + r.missing_payload)
            ? h`<div class="src-feeds" title="${r.reason}">and more</div>`
            : '';

        return h`<tr>
          <td class="src-hist-when"><time data-utc="${r.observed_at}">${r.observed_at}</time></td>
          <td>${r.style_id || '(none)'}</td>
          <td>${{ __html: true, value: verdictPill(r.status) }}</td>
          <td>${{ __html: true, value: miss + more }}</td>
          <td class="src-hist-held">${{ __html: true, value: held }}</td>
        </tr>`;
    }).join('');

    // The count travels WITH its window, always. A bare "12 changes" is the
    // 1,779 failure in miniature: a true count that becomes an estimate the
    // moment it is read without the window it was taken over.
    const note = truncated
        ? h`${rows.length} changes shown since ${since} — the response hit its ${HISTORY_LIMIT}-row limit, so older changes inside this window are not listed.`
        : h`${rows.length} change${rows.length === 1 ? '' : 's'} since ${since}. A row exists only where a verdict moved.`;

    host.innerHTML = h`<p class="src-hist-note">${note}</p>` + h`<table class="src-tbl">
      <thead><tr><th>When</th><th>Style</th><th>Verdict</th><th>Missing</th><th>Held</th></tr></thead>
      <tbody>${{ __html: true, value: body }}</tbody>
    </table>`;
}

async function loadVerdictHistory() {
    const hosts = Array.from(document.querySelectorAll('.src-history'));
    if (!hosts.length) return;

    let payload;
    try {
        payload = await api.get('/api/sourceability/events?limit=' + HISTORY_LIMIT);
    } catch (err) {
        // A failed fetch is NOT an empty history. Saying "no changes" here would
        // be the A1 defect in miniature: reading an absent answer as a positive
        // finding.
        for (const host of hosts) {
            host.innerHTML = h`<p class="src-none">Verdict history unavailable: ${String(err)}</p>`;
        }
        return;
    }

    const events = (payload && payload.events) || [];
    // The server echoes the window it actually used, and that is the number to
    // print — not the one this client asked for. The handler silently ignores a
    // malformed ?since= and falls back to seven days, so the request is not
    // evidence of the window.
    const since = (payload && payload.since) ? new Date(payload.since).toLocaleString() : 'the default 7-day window';
    const truncated = events.length >= HISTORY_LIMIT;

    const byProcess = new Map();
    for (const e of events) {
        if (!byProcess.has(e.process_key)) byProcess.set(e.process_key, []);
        byProcess.get(e.process_key).push(e);
    }

    for (const host of hosts) {
        renderHistory(host, byProcess.get(host.dataset.process) || [], since, truncated);
    }
    // These <time data-utc> cells were just inserted, and Core runs no htmx
    // swap hook, so convert them here.
    for (const elem of document.querySelectorAll('.src-history time[data-utc]')) {
        const d = new Date(elem.getAttribute('data-utc'));
        if (!isNaN(d.getTime())) elem.textContent = d.toLocaleString();
    }
}

loadVerdictHistory();
