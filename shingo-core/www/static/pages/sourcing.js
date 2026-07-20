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
  // A sourceability verdict is a function of (claims, available bin pool), so
  // the pool signals are what actually invalidate this page: bin-update and
  // inventory-update. There is no dedicated verdict event today — the monitor
  // publishes deltas to Edge over Kafka but does not broadcast to Core's SSE
  // hub — so this refreshes on the INPUTS rather than on the verdict itself.
  // That is a coarser trigger than ideal: it can reload when a bin moved in a
  // way that changed no verdict. A `sourcing-update` topic emitted where the
  // monitor already detects changed verdicts would be the precise signal.
  //
  // The page is server-rendered, so refreshing means re-requesting it. Reloads
  // are debounced because a burst of bin moves is one logical change to this
  // view, and the selected process is preserved above.
  let pending = null;
  function scheduleReload() {
    if (pending) return;
    pending = setTimeout(() => { window.location.reload(); }, 1500);
  }

  onSSE('connected', scheduleReload);
  onSSE('bin-update', scheduleReload);
  onSSE('inventory-update', scheduleReload);
}
