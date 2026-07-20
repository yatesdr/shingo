# Reservations and the claim discipline

This document has moved. The canonical description of reservations — the
soft-until-complete rule, the reserve/confirm split, slots-before-bins, where
each path lives in the code, the claim seatbelt, the compound-child exception,
and the invariant sweep that fences it — is the repo-root document:

**→ [`docs/reservations.md`](../../docs/reservations.md)**

It lives at the repo root rather than here because it is referenced across the
root documentation set (`[[bin-transit-state]]`, `[[storage-protections]]`,
`[[order-builder-dispatch]]`, `[[terminology]]`, `[[data-model]]`,
`[[queued-order-fulfillment]]`, `[[order-state-machine/transitions]]`) and
those cross-links resolve relative to that directory.

Nothing was dropped in the move: the content that used to live in this file was
folded into the root document, which now carries both the substrate reference
(store API, schema, allocator, reaping) and the claim discipline.
