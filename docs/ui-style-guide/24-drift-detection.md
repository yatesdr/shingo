## Drift detection

The codebase already has one drift test:
`shingo-edge/www/order_status_js_drift_test.go` pins the JS status arrays
in `operator-station/order-status.js` to the Go projectors in
`protocol/status.go`.

Extend this pattern to:

1. **CSS class coverage** — every `protocol.Status` value has a
   `.badge-<status>` class in `shared/status-classes.css`.
2. **Swap mode enum** — JS dropdown options match `protocol.SwapMode` values.
3. **Claim role enum** — same.
4. **Token name presence** — if a CSS file references `var(--foo)`, `--foo`
   exists in `tokens.css`. Extended to **templates** (shipped):
   `TestNoUndefinedCSSVarsInTemplates` in `shingo-core/www` fails when a
   template's `var(--foo)` resolves to no `--foo` in the shared/page CSS (the
   `--card-bg`-referenced-but-undefined class of bug), allowing inline
   template-local custom properties.
5. **No emoji** (shipped) — `TestNoEmojiInTemplatesAndPageJS` in both
   `www` packages fails on any emoji in a template or page-JS file, via
   `shared.IsEmoji`. See the Icons section.
6. **Swap-mode WORDS** (shipped, U10) — `TestSwapModeWordsMatchTheModel` in
   `shingo-edge/domain` pins Go's `SwapModeWord` to `composer-model.js`'s
   `MODES` block, which is the authority because both surfaces draw their
   chips and cards from it. The validator's refusals name a mode by the same
   word (`swapModeLabel` returns `SwapModeWord`; only loader and unloader
   claims, which have no chip, keep their own), so an operator who picked
   `2‑robot index` reads `2‑robot index` in the refusal. A suggested preset
   name is a control and uses it too.
7. **Shape-field words** (shipped, U10) — `TestShapeFieldWordsMatchTheModel`
   pins `domain.shapeFields` to `composer-model.js`'s `SHAPE_FIELDS`: the same
   eleven fields, the same words, the same order. The server words a drifted
   member's field list on a row and the desktop words an apply's diff in the
   modal one click away, and two spellings would read as two different fields
   describing one change. The words are D1's column headings, which is what
   makes them worth pinning — a column heading is a decision about what an
   engineer is looking at, and it is made here rather than in either file.

Each test is ~30-50 LOC of Go reading source files literally with a regex.
Don't introduce a code generator; the test pattern is sufficient for the
current scale.

