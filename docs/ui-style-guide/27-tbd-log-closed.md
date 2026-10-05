## TBD log (closed)

Every TBD entry from the working draft has been resolved. The
decisions are referenced in the relevant sections above; the summary
below exists as a paper trail for anyone reading the doc and wondering
what was contested at the start.

- **ES modules in shared/, shared/ placement, modal backdrop default,
  form-state convention, per-page imports + delegateActions** —
  see Code organization, Module shape, Modals, Forms, and Event
  handling sections.
- **shingoedge.js / app.js interior cleanup** — both files are now
  flat top-level `export function` / `export const` declarations.
  `window.ShingoEdge` is retained at the bottom of `shingoedge.js`
  only for the two remaining non-module consumers (`traffic.html`
  inline `<script>` and `operator-station/operator.js`); when those
  migrate to module imports the bridge can go.
- **HTMX swap targets re-running `convertTimestamps`** — resolved as
  automatic. `shared/utils.js` exports
  `installHtmxTimestampConversion()`, which wires a single
  `document.body` listener for `htmx:afterSwap` that calls
  `convertTimestamps(event.detail.target)` against the swapped-in
  subtree. Edge's `shingoedge.js` calls it once at module load
  alongside `installBackdropClose()`. Under the plant-local convention
  (see Timestamps) templates emit plant-local text and the shim is a
  rollover guard — it rewrites only pre-convention ` UTC` paints, so a
  swapped-in subtree can never reintroduce the flicker. Core admin
  doesn't use HTMX so the listener never fires there; the API is
  available if a future surface adopts HTMX.
- **Operator HMI `.os-modal*` rename** — the operator surface now
  uses `.modal-overlay.modal--touch` for the backdrop and
  `.modal--touch .modal-*` for the inner pieces, per the Modal
  section's canonical naming.

