# Shingo → CMS Integration

Living document, owned by the shingo team. It describes what is on
`feat/cms-middleware` as of 2026-09-05 and is meant to grow as the follow-up
items land.

It is the **reference**: what the integration does, where each piece lives, and
which rules are load-bearing. It is not the design record (that is the round-2
synthesis) and not the specification (that is the implementation plan). Where a
claim here is subtle, the file and symbol that decide it are named, because the
code is the thing that is true.

---

## What this is

When an AMR carries a bin across a boundary between two CMS storerooms, shingo
records a pair of inventory transactions locally and POSTs them to the CMS
middleware, which batches them into CMS.

**And when the unloader CLEARS a bin at a tagged node, shingo books the
departure.** That is the second event, and it is one-sided: clearing is how
material leaves the AMR supermarket for a CMS zone shingo cannot see, so there is
a decrement and no matching credit. See [the clear](#the-clear-a-one-sided-departure).

**Two switches, both per-site, and neither is a plant name.** A node is a
boundary because it carries a `cms_storeroom` property. The subsystem runs at
all because the site's yaml carries a `cms:` block. A site with neither is
unaffected by everything below; a site with both participates. Which sites have
them is deployment state and is deliberately not recorded here — this is built
for the fleet, not for one plant, and a doc that names today's participants
starts lying on the day the next one is switched on.

**AMR dispatch never waits on this.** If the middleware is unreachable for a
shift, bins keep moving and rows accumulate. The backlog is a number on the
diagnostics page, not a brake on the plant.

---

## Wire contract

`POST <base_url>`, `Content-Type: application/json`, body a JSON **array** of
transaction objects.

Headers:

| Header | Value |
|---|---|
| `x-access-key` | `cms.access_key` |
| `x-secret-key` | `cms.secret_key` |
| `x-body-sha256` | sha256 of the exact body, hex. Offered as a dedup hint; harmless if ignored. See [Known limitations](#known-limitations-v1). |

### The thirteen fields

Spellings follow the vendor's sample and are **not** shingo's vocabulary — they
are kept so a person can compare `cms/wire/wire.go`'s `MiddlewareTx` against the
vendor document line by line. **That comparison happened 2026-09-07** and
`MiddlewareTx` matches the sample exactly (F4 closed). The line above used to say
it was still owed.

| Field | Where it comes from | Notes |
|---|---|---|
| `TicketNumber` | constant `1` | Per the sample. Not an identifier we control and **not usable as an idempotency key** — it does not distinguish two postings. `EntryNumber` is what distinguishes them. |
| `EntryNumber` | `cms_transactions.id` | The ROW ID, globally unique. It was a 1-based index, which with `TicketNumber` pinned to `1` made every single-row post send the pair `(1, 1)` — and nobody has confirmed what that pair means to CMS. The array is sorted by this same id, so a given set of rows still serialises identically, which is what makes `body_sha` meaningful. |
| `PartNumber` | `cms_transactions.cat_id` | A `payload_manifest.part_number`. **Not** a payload code — see [Quantity derivation](#quantity-derivation). |
| `StockLocation` | `cms_transactions.storeroom` | The boundary's `cms_storeroom` value, stamped at build time. |
| `Bin` | `cms_transactions.bin_label` | CMS's own bin concept; shingo's carrier label is what fills it. |
| `Quantity` | `abs(cms_transactions.delta)` | **Unsigned.** Direction lives in `TransactionType`. |
| `TransactionType` | `cms.increase_type` / `cms.decrease_type` | Chosen by the sign of `delta`. |
| `Resource` | `cms_transactions.robot_id` | Blank for an operator drag — no robot moved it, and an invented resource would be a claim about the plant that is not true. |
| `ReasonCode` | `cms.reason_code` | |
| `UnitOfMeasure` | `cms.unit_of_measure` | |
| `UserId` | `cms.user_id` | |
| `Department` | `cms.department` | Blank by default, per the sample. |
| `Operation` | `cms.operation` | Blank by default, per the sample. |

Rows with `delta = 0` are dropped: a transfer of nothing is noise in a ledger,
and `sign(0)` would have to pick a `TransactionType` and would be wrong half the
time by construction.

### Response

A 2xx is expected to carry a transaction id. The client accepts it under any of
`TransactionId`, `TransactionID`, `transaction_id`, `transactionId`, `id`, as a
string or a JSON number, and keeps the literal text — ids are opaque tokens
here, and decoding a large one through a float64 would round it.

A 2xx with no id we can parse is **not** a success: it is `inflight`. Something
was accepted and we do not know what it is called.

### Status GET

`GET <base_url>?TransactionId=<id>`, same auth headers. Three-valued and the
caller must treat it that way:

- 404, or 2xx with an empty result — **not held**.
- 2xx with a non-empty result — **held**.
- anything else, or a transport failure — **could not ask**, which is not an
  answer. Collapsing it into "not held" re-sends a transaction that landed.

---

## Signal flow

There are TWO entry points, and they differ in more than which rows they build.

```
bin moves
  └─ engine emits BinUpdatedEvent{Action:"moved"}
       └─ RecordMovementTransactions            engine/cms_transactions.go
            ├─ ev.Replay → return               (a replay must not re-book)
            └─ material.BuildMovementTransactions
                 ├─ FindCMSBoundary(from), FindCMSBoundary(to)
                 ├─ same boundary → nothing
                 ├─ rows: source −count, dest +count
                 └─ uncounted manifest lines → reported back
            ├─ error or uncounted lines → cmsBuildFailures++ and a log line
            ├─ db.CreateCMSTransactions(rows)
            └─ Events.Emit(EventCMSTransaction)

the unloader clears a bin       POST /api/telemetry/bin-clear
  └─ ClearForReuseAndBookDeparture          engine/cms_transactions.go
       ├─ buildClearDeparture
       │    └─ material.BuildClearTransactions
       │         ├─ FindCMSBoundary(node)  ← BEFORE the bin is read
       │         ├─ no boundary → nothing
       │         └─ rows: −count at that one boundary
       │    └─ error or uncounted lines → cmsBuildFailures++ and a log line
       │       naming the payload and uop_remaining, which the clear destroys
       ├─ no rows → binManifest.ClearForReuse (its own transaction, as before)
       └─ rows → ONE transaction:
            ├─ cms.CreateInTx(tx, rows)     ← fails ⇒ the clear is REFUSED
            ├─ binManifest.ClearForReuseTx(tx, …)
            ├─ commit
            └─ Events.Emit(EventCMSTransaction)   ← after the commit, never before

both then reach
  └─ subscriber (engine/wiring.go)
       ├─ cms.Postable(source_type)          movement, clear — not correction
       └─ poster.Enqueue → cms_postings row + AttachPosting
                           └─ Ring() — the doorbell
                                └─ poster.DrainOnce
                                     ├─ wire.Build → JSON
                                     ├─ MarkInflight            ← BEFORE the send
                                     ├─ client.Post
                                     └─ applyResult → durable state
                                          └─ poster.ReconcileOnce settles inflight
```

Two backstops run on the poster's own tickers, because a doorbell that is missed
once would otherwise strand work forever:

- **`SweepOrphansOnce`** re-enqueues `cms_transactions` rows that are older than
  one poll interval and that no posting ever claimed. `Enqueue` runs on the
  event subscriber's goroutine, and when it fails the only thing that would have
  retried is the notification that already failed.
- **`ReconcileOnce`** is the only path out of `inflight`.

---

## Boundary model

`cms_storeroom` on a node makes it a CMS boundary, and the property's **value**
is the storeroom code CMS knows it by (`SM01`, `MAN`, `DOCK`).

**One property, two answers, no default.** Presence means "this is a boundary";
absence means "not a boundary". A node is never a boundary because of where it
sits in the tree.

`material.FindCMSBoundary` walks up the parent chain from a node and returns the
nearest **synthetic** ancestor (or the node itself) that carries the property.
It is **fail-closed at every depth**, and that is a correction rather than a
preference: the predicate it replaced defaulted parentless synthetic nodes ON,
so `_TRANSIT`, every node group and every per-robot carrier node was a boundary.
A bin picked up by a robot "crossed" from its storeroom into the robot and
shingo booked the transfer.

Three distinct answers, and callers must not collapse them:

| Return | Meaning |
|---|---|
| `(node, code, nil)` | this is the boundary |
| `(nil, "", nil)` | the walk reached a root; no boundary above this node |
| `(nil, "", err)` | the lookup FAILED, or the parent chain has a cycle |

The third is not the second. A transient database failure that reads as "no
boundary here" emits zero transactions for a real physical move, with only a log
line as evidence.

**Same-boundary moves emit nothing.** Cross-boundary moves emit a paired
negative at the source and positive at the destination. A move with only one
tagged endpoint emits only that side. A **clear** resolves one node and emits only
a negative at whatever boundary it found — see [the clear](#the-clear-a-one-sided-departure).

### Tagging a node

There is no UI for this in v1; the generic node-property UI exists, and at
cutover the codes go in by hand:

```sql
INSERT INTO node_properties (node_id, key, value)
VALUES (<node id>, 'cms_storeroom', '<SCO's code>');
```

---

## The clear: a one-sided departure

`POST /api/telemetry/bin-clear` is **Edge's door and the unloader's**. At
Hopkinsville the unloader takes bins out of the AMR supermarket by clearing them
and moving the material elsewhere, so the clear IS the departure. Before this it
emitted an SSE broadcast and never reached the event bus: the arrival posted, the
departure did not, and the storeroom climbed forever.

`engine.ClearForReuseAndBookDeparture` owns it.

**Deliberately one-sided.** shingo does not know which CMS zone the material went
to, and inventing one would be a claim about the plant that is not true. CMS
credits the destination by its own logic, as it credits MAN at label print. On the
wire that is one row, `TransactionType` `D`, `Quantity` equal to the bin's
contents at the moment of the clear.

**`Resource` is blank.** No robot cleared the bin; a person did.

### One transaction, and the ordering is the point

The clear destroys `uop_remaining` and the manifest — the two values the quantity
is derived from. So:

- rows written **after** the clear cannot be reconstructed if their write fails;
- rows committed **before** it become a departure the plant never made if the
  clear then fails.

One transaction is the only arrangement with neither failure, and it is reachable
because `BinManifestService.ClearForReuseTx` takes a caller's `*sql.Tx` and
`cms.CreateInTx` was split out of `cms.Create` to match. If the row write fails
the clear is **refused** — the unloader presses the button again, which is
recoverable; a clear that silently drops inventory is not. Same family as
`MarkInflight` committing before the POST.

The event is emitted **after** the commit. A posting for rows a rollback removed
would send CMS a departure that did not happen.

Pinned by `TestCMSClearEndToEnd_AFailedBookingRefusesTheClear`, which installs a
CHECK constraint that refuses the row and asserts the bin still holds its
contents. It goes red against a two-transaction implementation.

### A build failure does not block the door

An unparseable manifest, a template line naming no part, a cycle in the node tree
— each of those would make the bin **permanently unclearable** if the door refused
on them, which takes the operator's repair path away. That is the HK 2026-07-28
trap: the instinctive fix being the one action that guarantees no recovery. A
broken inventory feed must not become a brake on the plant.

So the loss is counted into `cmsBuildFailures`, which **ranks first** on the
health verdict, and the log line names the payload and `uop_remaining` **while
they still exist** — the movement path does not need to, because there the bin
still stands and can be re-counted.

### The boundary is resolved before the bin is read

That ordering is what keeps an untagged site away from this code entirely: nothing
below the boundary check runs for a plant that has tagged nothing, so no template
defect anywhere can surface at its clear door. Pinned by
`TestBuildClear_UntaggedNodeProducesNothing`, whose fixture carries a template
that WOULD refuse — move the check below the bin read and it goes red.

### Not the UI's clear

`binClear` (`www/bin_actions.go:182`) is the admin door, and it stays silent. It
is what an operator reaches for to REPAIR a wrong record, and booking a repair as
an inventory movement writes fiction into a ledger. The same gesture serving both
"this record was wrong" and "I took this material away" is the real problem, and
the fix is to let the person say which — not to make clear book unconditionally.
Recorded as deferred in `NEXT-STEPS-cms.md`; the split is pinned by
`TestCMSClearEndToEnd_TheServiceClearStaysSilent` so it cannot become incidental.

---

## Quantity derivation

```
quantity = |bin.uop_remaining × payload_manifest.parts_per_cycle|
```

Computed at emission time in `material.BuildMovementTransactions`, never stored.
The bin manifest lists **which parts are in the carrier and nothing else** — it
used to carry a `qty` that no writer agreed on and nothing rewrote as production
drew the bin down, so whatever it held went stale on the first consumed part.
Historical `qty` values in production JSON are ignored.

**`manifest.items[].catid` IS A PART NUMBER.** It is matched against
`payload_manifest.part_number` by everything that counts a bin — this
derivation, and the inventory page's per-part rows. Writing a payload code there
matches nothing, so the bin resolves to a ratio of zero, books nothing, and
looks exactly like a bin that crossed no boundary. That was a live defect in
`syncOrClearForReleased` (every partial-release bin) and it is the reason for
the counter below.

### When a line cannot be counted

`BuildMovementTransactions` returns the manifest lines it could not turn into a
count, and the engine counts them into `cmsBuildFailures` and names them in the
log. A line nobody can count is inventory that physically moved and will never
be booked; left to `continue` silently, it is invisible.

A **drained** bin is not an uncountable one: `uop_remaining ≤ 0` makes every
count zero for a reason the template answered perfectly well, and reporting it
would put a finding on every empty carrier in the plant.

A bin with **no template at all** — an unknown `payload_code`, or none — emits
nothing and reports nothing. There is no ratio, so no count can be derived, and
a movement row with a guessed quantity is worse than no row: once it reaches CMS
it is indistinguishable from a measured one.

---

## Inflight state machine

The API has **no idempotency key**. The only question that decides whether a
retry is safe is whether any bytes could have reached the middleware — not how
severe the failure was. A DNS failure and a read timeout are both "the POST
failed" and they are opposite answers to that question.

### Client classes

| Class | Cause | Could the middleware have it? |
|---|---|---|
| `ClassPosted` | 2xx with an id | yes, and we know its name |
| `ClassInflight` | 2xx we could not parse an id out of | yes, name unknown |
| `ClassRejected` | 4xx | no, and it will be refused again |
| `ClassAuthFault` | 401/403, or a request we could not build | no |
| `ClassRetryableBeforeSend` | DNS, TLS, refused connection | **no** — nothing left |
| `ClassRetryableAfterSend` | 5xx, timeout, anything unrecognised | **maybe** |

The zero value is `ClassRetryableAfterSend`, deliberately: a bare `PostResult{}`
has to mean something, and the safe meaning is "this may have landed".

### Posting statuses

The statuses are **not a progress bar**. They record what is known about whether
the middleware has the data.

| Status | Meaning | Exit |
|---|---|---|
| `pending` | nothing sent. Safe to send. | the drain |
| `inflight` | bytes went out, answer unknown. **Not safe to send again.** | the reconciler, only |
| `posted` | acknowledged, id known | terminal |
| `rejected` | refused | terminal |
| `failed` | out of attempts, credential fault, or a bounded loop gave up | needs a person |

### Two-phase send

`MarkInflight` **commits before the POST**. A crash from that point leaves a row
saying "we tried, we do not know" — which the reconciler can work with. A row
still saying `pending` while a POST was on the wire would be eligible for
re-send with nothing recording that a copy had gone.

`applyResult` writes the transaction id **before** the status, for the same
family of reason: `MarkPosted` carries both, so a failure of that one statement
used to lose the id and leave an inflight row with nothing to ask about.

### What the reconciler can and cannot resolve

It works by asking the middleware whether it holds an id, so it can only resolve
rows that **have** one. In v1 exactly one after-send failure produces one: a 2xx
that named an id whose settling write then failed.

Every other route into `inflight` leaves no id — a 2xx with an unparseable body,
a 5xx, a timeout mid-response, a crash between the send and the acknowledgement.
Those are **not** auto-resolved and are not meant to be: re-sending may
double-book and marking them posted would invent a success. They age, count as
`UnresolvableInflight`, and a person checks the middleware and settles them by
hand.

That is a deliberate v1 boundary, not an unfinished path. It moves if the
middleware ever dedupes on `x-body-sha256`.

### Bounds

| Bound | Config | Counts |
|---|---|---|
| attempts | `cms.max_attempts` (12) | sends, incremented once per `MarkInflight` |
| requeues | `cms.max_requeues` (3) | the reconciler returning a row to the queue |

They are separate because they count different events, and the second is not
optional: a requeue deliberately **preserves** the attempt budget, so without
its own ceiling that cycle has none. A row that exhausts either becomes
`failed`, with `last_error` saying which.

Backoff is exponential on the poll interval, capped at 15 minutes — the cap is
what makes `max_attempts` a bounded amount of TIME rather than an unbounded one.

**Nothing is ever discarded.** The Kafka outbox drops a message after ten
attempts, which `docs/outbox-ordering.md` calls "not a delay, a hole". An
inventory ledger cannot have a hole.

---

## Fail-loud mechanisms

**Health is positive evidence, never inferred silence.** "Zero pending" is
equally the signature of a muted poster, an untagged plant, a failing subscriber
and a feed nobody is handing work to. `healthy` requires a successful post and
at least one tagged boundary; a feed that has never posted reports "configured
but unproven".

`GET /api/cms-health` returns the counts, the verdict and its sentence. The
verdict is computed **server-side** so one definition serves every reader.

### The ranking, worst first

`service/cms_posting_service.go`'s `verdict()`, pinned by
`TestVerdict_RankingIsPinnedInOrder`:

1. **`BuildFailures`** — a move that reached no table at all. It writes no
   transaction, so it writes no posting, and every other count would report a
   plant that simply did not move anything. Nothing may rank above it.
2. **muted** — the feed is sending nothing.
3. **no tagged storerooms** — the feed *can* never send anything.
4. **`UnresolvableInflight`** — inventory in an ambiguous state. Worse than any
   known outcome, because only a person can settle it.
5. **`Unposted`** — recorded and never queued. An ongoing leak, unlike the
   parked rows below.
6. **`FailedRecent`**, then **`RejectedRecent`** — both definitely-not-booked;
   a failure can be requeued, a rejection needs the body fixed first.
7. age findings, then "nothing posted yet".

`rejected` and `failed` are read through **windowed** counts
(`cms.health_window`, 24h). They are permanent marks on a row with no
acknowledge path, and ranked on lifetime totals one refusal would hold the card
red forever — a health surface that cannot go green stops being read. Older
rows stay in the counts and are named in the healthy sentence; they stop being
the verdict, not stop existing.

`cmsBuildFailures` is in-memory and resets on restart. That is honest: it counts
what **this** process dropped.

The diagnostics card is the loud channel for v1. There is no paging and no
email.

### Muting

A credential fault mutes the poster: with a bad key every queued posting fails
identically, and letting them all exhaust their attempts turns a five-minute
config fix into a table of dead rows. The faulting posting goes back to
`pending` — a 401 is evidence the middleware did **not** book it.

**A mute is cleared by restarting core.** There is no unmute verb, deliberately:
the fault it fires on is a credential, and a credential is fixed in the yaml
that is read at boot.

---

## Config surface

The `cms:` block. **Validation is FATAL**, unlike the neighbouring `rds:` block
which repairs a bad value and carries on. The distinction is the point: RDS's
validated field decides what wording an operator sees, and this one decides
whether an inventory ledger receives what shingo believes about the plant's
stock. A core that starts with a URL and no keys posts nothing, records the
failures in a table nobody is watching, and looks healthy.

| Key | Default | Rule |
|---|---|---|
| `base_url` | `""` | **Empty disables the whole subsystem.** Must parse with a scheme and a host. |
| `access_key`, `secret_key` | `""` | Site-local yaml, never the repo. With `base_url` set, either being empty **refuses to boot**. |
| `timeout` | `30s` | HTTP round trip. |
| `poll_interval` | `30s` | Drain ticker; also the backoff base and the orphan sweep's grace period. |
| `max_attempts` | `12` | Sends before a posting fails. |
| `settle_window` | `5m` | How long an inflight row is left before the reconciler asks. Too short gets "no such transaction" about something about to exist. |
| `max_requeues` | `3` | Reconciler requeues before it gives up. |
| `health_window` | `24h` | How far back a terminal posting counts toward the verdict. |
| `reason_code` | `TEST-AMR` | Vocabulary — placeholder until SCO confirms. |
| `increase_type` | `I` | Vocabulary. |
| `decrease_type` | `D` | Vocabulary. |
| `unit_of_measure` | `EA` | Vocabulary. |
| `user_id` | `SHINGO` | Vocabulary. |
| `department` | `""` | Vocabulary; blank in the vendor's sample. |
| `operation` | `""` | Vocabulary; blank in the vendor's sample. |

Every vocabulary value is a yaml edit rather than a release. That is a promise
made to SCO and it is why `department` and `operation` are config rather than
the constants they started as.

`config.Load` starts from `Defaults()` and unmarshals the yaml over it, so an
omitted key keeps its default. `Validate` fills durations and counts written
explicitly as zero; it leaves the vocabulary strings alone, because an empty
one can be exactly right.

A commented example block lives in `shingocore.dev.yaml`.

---

## Known limitations (v1)

- **No middleware-side dedup on `x-body-sha256`** — **answered 2026-09-07**
  (**F5**): there is none, and each POST regenerates a transaction id, so a
  re-send DOUBLE-BOOKS. The `inflight` class stays manually resolvable as
  designed, and no posting is ever retried by hand.
- **The status GET ignores `TransactionId`** — "next release" per the middleware
  owner, 2026-09-07. With the param ignored the endpoint returns ALL
  transactions, so the reconciler's "2xx with a non-empty result" reads as *held*
  for any id it asks about. Narrow (it needs an inflight row carrying an id) and
  silent. **Do not trust an automatic inflight resolution until the param
  ships.**
- **After-send failures without a returned id resolve manually.** See
  [what the reconciler can and cannot resolve](#what-the-reconciler-can-and-cannot-resolve).
  A person checks the middleware and marks the row posted or requeues it.
- ~~The thirteen field spellings are inferred from one sample~~ (**F4**)
  **closed 2026-09-07** against the vendor's own sample; `MiddlewareTx` matches
  exactly. A test pins them and fails on an extra key as well as a missing one.
- **A UI clear books nothing** (`www/bin_actions.go:182`), so material pulled
  through that door is invisible to CMS. Deliberate — see
  [not the UI's clear](#not-the-uis-clear) — and the standing item in
  `NEXT-STEPS-cms.md`.
- **Consumption in place is invisible.** A bin drawn to zero where it stands and
  then removed as an empty posts nothing: the quantity derives from
  `uop_remaining` at emission and a drained bin counts zero. The material left by
  being consumed, not carried, and shingo has no consumption transaction. Only
  bites where a tagged storeroom is consumed from directly.
- **No template versioning.** SCO editing a payload template retroactively
  changes what in-flight bins ship to CMS as counts, because the quantity is
  derived at emission rather than captured at load. A separate project if the
  behaviour ever bites.
- ~~Four SPR `parts_per_cycle = 0` rows~~ **resolved 2026-09-05.** The owner
  declared all four entry oversights rather than declarations, and they were
  corrected at the plant, pinned to row ids. No migration backfilled them: a
  blanket rewrite of this column would have invented inventory, and it is not
  a law that a zero means one — other rows legitimately hold 2 and 24. A
  missing or non-positive ratio is now refused at every entry point, so the
  question is closed going forward rather than only backwards.
- **Two flagged template rows await SCO** (**F7**): HK's `Test-Payload` at 24,
  and SPR `76292-6TA0C.06 / 33258` at 2.
- **Historical `source_type='correction'` rows persist** and remain filterable.
  The code path that emitted them is deleted, and the `corrections` table was
  dropped (v104) once both plants confirmed zero rows in it and zero
  transactions carrying that source type.
- **`GET /api/cms-health` is unauthenticated**, in the same route block as
  `/api/cms-transactions` and `/outbox/deadletters`. Its `muted_reason` can
  embed an excerpt of the middleware's refusal body. The excerpt is bounded
  (500 bytes, cut on a rune boundary) and credential-redacted, so no key leaks —
  but arbitrary middleware error text can reach an unauthenticated reader on
  the plant network. Small surface; documented rather than changed.
- ~~19 migration verify predicates~~ **fixed.** Eleven assertions across nine
  verify predicates negated a helper that returns false on a query error, which
  reads a failed check as a satisfied post-condition; they now use the absence
  helpers (`schema.ColumnAbsent` / `TableAbsent` / `IndexAbsent` /
  `NodePropertyKeyAbsent`). The other eight matches of that spelling are inside
  migration bodies, where the direction is already the safe one, and were left
  alone. A lint keys on the parameter type so the distinction survives.

---

## Where things live

| | |
|---|---|
| boundary walk, row builders (move and clear) | `material/material.go` |
| persistence and emission | `engine/cms_transactions.go`, `engine/wiring.go` |
| the unloader's door | `www/handlers_telemetry.go` `apiBinClear` |
| the source-type vocabulary and `Postable` | `store/cms/cms.go` |
| transactions, postings | `store/cms/` |
| migrations | `store/migrations.go` (v99–v104) |
| translator (pure) | `cms/wire/wire.go` |
| HTTP client | `cms/client/client.go` |
| drain and reconcile | `cms/poster/poster.go` |
| health verdict | `service/cms_posting_service.go` |
| endpoint and card | `www/handlers_cms_health.go`, `www/static/pages/diagnostics.js` |
| end-to-end proof | `engine/cms_end_to_end_docker_test.go`, `engine/cms_clear_end_to_end_docker_test.go` |

## The design record

The argument behind this subsystem — the review rounds, the implementation plan,
the deviation log with every judgment call and its reasoning — was written
outside this repository and is **not distributed with the checkout**. It lives in
the working area alongside it on the machine where the work was done.

That is worth knowing for one reason: several rules above look arbitrary and are
not. The boundary property having no default, the count being derived at
emission instead of stored, absence rather than zero on the bin-load wire, the
refusal to guess a quantity — each of those is a decision someone argued
against and lost, and the record of why is in that log rather than here.

If you are about to change one of them and cannot find the reasoning, ask before
assuming there was none. The code comments carry the short version at each site;
this doc carries the shape; the argument is elsewhere.
