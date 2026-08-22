# Core → Edge outbox: what order things arrive in

Answered 2026-08-02, because the projection work needs it and nobody had
checked. The short version is at the bottom if you only need the rule.

## The guarantee

**Steady state it is FIFO, and stronger than per-station: it is global FIFO.**

- One outbox table, dequeued by `ORDER BY id` on a `BIGSERIAL`
  (`shingo-core/store/messaging/messaging.go`, `store/schema/postgres_ddl.go`).
  `station_id` and `msg_type` are stored but appear in no `ORDER BY`, no
  partition, and no priority.
- One drainer goroutine, publishing the batch in a single loop, called inline so
  cycles never overlap (`protocol/outbox/drainer.go`).
- One topic, created with one partition (`shingo-core/messaging/client.go`). With
  a single partition Kafka preserves total order regardless of key.
- Edge consumes on one read loop per topic and dispatches synchronously through
  the subject router — a map lookup and a middleware chain, not a queue per type.
  **Edge does not reorder.**

## The hole

**The drainer does not head-of-line block.** On a publish error it increments the
retry count and returns, and the caller moves to the next message. So message A
failing while message B succeeds in the same pass delivers B first and A on a
later cycle.

`IsConnected()` gates the whole cycle, so a clean disconnect is safe — nothing
publishes at all. ⚠️ On EDGE that gate does not fire: `IsConnected()` there is
`kafkaW != nil` and `Connect()` performs no I/O (`kafkago.TCP` resolves lazily),
so it is true from the first connect until shutdown regardless of the network.
It reports that a writer object exists, not that a broker is reachable — see
`kafka_last_publish_ok` on edge `/status` for the latter. It must stay that way:
`drain()` returns early on a false, so a "reachable now" answer would stop
retrying at exactly the moment retrying is the point. What reorders is a *per-message* failure: a context timeout, an
oversized payload, a broker error that recovers mid-pass.

Worse, and the part to design against: once a message exhausts `MaxRetries` the
`retries < N` filter drops it from the stream **permanently**, while everything
behind it keeps flowing. That is not a delay, it is a hole.

Three things changed around this on 2026-08-22, none of which close the hole:

- **The wake arm mutes on failure.** The drainer's doorbell fires a drain per
  enqueue, and a drain retries *every* pending row — so under a transport
  failure each enqueue spent one of the message's `MaxRetries`. A publish
  failure now stands the wake arm down until a ticker drain succeeds, putting
  the tolerance back at `MaxRetries × interval`.
- **Dead letters are kept seven days**, delivered rows one. They shared a 24-hour
  cutoff, which destroyed the evidence of a loss before anyone found it. The
  purge is now two statements in one transaction rather than one with an `OR`.
- **`MaxRetries` is still a COUNT**, so its real-world tolerance silently
  rescales whenever the drain cadence changes — which is exactly what the
  doorbell did to it. Expressing it as a duration is the open item.

## Two smaller caveats

**The partition key is empty on Core.** `PartitionKey` is only ever assigned on
Edge. Inert today because the topic has one partition — but raising the partition
count turns this into live reordering, silently. The client's own comment already
flags it.

**`ORDER BY id` is not commit order.** Two concurrent transactions can commit in
the opposite order to their assigned ids, and a drain landing in that window
publishes the higher id first. Largely defused here because `EnqueueOutbox` takes
`*sql.DB` and does a single autocommit INSERT — it never joins a caller's
transaction — so two enqueues issued in sequence from one goroutine are ordered.
It only bites when two things enqueue from different goroutines.

## The rule

If two messages about the same order are enqueued in sequence from one code path,
they arrive in that order. **Do not build on that alone.** A consumer must
tolerate arriving second-first, because one failed publish is all it takes.

For the order projection specifically: the reconcile is **day-one load-bearing,
not a backstop**. "A status arrived for an order I have never seen" is a case
that will happen, not an unreachable one, and it must be handled rather than
logged as impossible.
