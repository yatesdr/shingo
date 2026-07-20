import { onSSE } from '/static/shared/utils.js';

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
