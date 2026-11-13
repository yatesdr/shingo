## Buttons

Verified against: 74c3eb77

### Class taxonomy

```css
.btn          /* base — neutral background, border */
.btn-primary  /* primary action */
.btn-danger   /* destructive */
.btn-sm       /* size variant */
.btn-icon     /* icon-only square button */
.btn-block    /* full-width */
```

That's the entire taxonomy. Each app's sheet defines it (Core's `style.css`,
Edge's `shingoedge.css`); `.btn-icon` and `.btn-block` are Edge's only. Resist
adding `.btn-secondary`, `.btn-success` etc. — if you need a green button, it's
usually a primary action in a different context, not a new variant.

**A destructive verb repeated on every row of a table is `.btn .btn-sm`, not
`.btn-danger`.** A column of solid red buttons is structure shouting louder
than the data beside it (Orders' Cancel, Payloads' Delete). The confirm dialog
carries the warning and is unchanged. `.btn-danger` stays for the one
destructive action of a dialog or a detail view.

### Touch sizing

An Operator HMI touch target is never smaller than `--os-touch-min` (56 px).
Size it from the scope it sits in (`.modal--touch`, the one-tap panel's
`.os-co-picker--one-tap`), not with a parallel `.btn--touch` modifier.

### Labels

A one-tap panel's action on the Operator HMI is a short verb phrase that names
what happens to the thing in front of the operator, not the API verb behind it:
**BIN OFF**, **EMPTY ON**, **PARTIAL ON**. The HMI's action buttons are upper
case, like **CANCEL** beside them; a panel half in one case and half in the
other reads as two systems.

### What not to do

- Hardcoded `padding: 12px 24px` for touch buttons — use the scope
- Another parallel button taxonomy beside the HMI's existing `.os-action-btn`
  / `.os-header-btn`
- Tab buttons styled as primary buttons — tabs are not CTAs (see
  [Tabs](22-tabs.md))

