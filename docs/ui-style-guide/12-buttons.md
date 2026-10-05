## Buttons

### Class taxonomy

```css
.btn          /* base — neutral background, border */
.btn-primary  /* primary action */
.btn-danger   /* destructive */
.btn-sm       /* size variant */
.btn-icon     /* icon-only square button */
.btn-block    /* full-width */
```

That's the entire taxonomy. Resist adding `.btn-secondary`, `.btn-success`
etc. — if you need a green button, it's usually a primary action in a
different context, not a new variant.

### Touch sizing

Operator HMI buttons get `min-height: var(--os-touch-min)` via the
`.modal--touch` scope (or equivalent). Don't introduce a parallel
`.btn--touch` modifier; the scope handles it.

### Labels

A one-tap panel's action on the Operator HMI is a short verb phrase in
sentence case: **Full off**, **Send on**. It names what happens to the thing in
front of the operator, not the API verb behind it. CANCEL and the legacy picker
labels stay upper case until the HMI migration changes them together; a panel
half in one case and half in the other reads as two systems.

### What not to do

- ❌ Hardcoded `padding: 12px 24px` for touch buttons — use the scope
- ❌ `.os-action-btn`, `.os-header-btn` parallel taxonomies — fold into `.btn`
- ❌ Tab buttons styled as primary buttons (the `.process-tab.btn-primary`
  pattern on Edge) — tabs are not CTAs

