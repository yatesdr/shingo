## Mission detail — stages

**Mission detail shows an order's life as stages.** The source is
`order_history`, with one row per status span (stage, where, started, took,
share). The spans tile the order's life exactly, from the first history row to
the terminal row, or to the server's now while the order is in flight. Nothing
is "unaccounted": a figure that does not sum is a bug, not a segment.

One bar draws the same spans on a true time scale (width proportional to time,
no minimum width). It has four classes: *waiting to dispatch* (pending,
sourcing, queued, submitted, reshuffling), *moving* (dispatched, acknowledged,
in transit) and *held* (staged, an unowned fault) from the data palette, and
*waiting for confirm* (delivered) drawn neutral (`--viz-secondary`). Held means
a robot is parked on the order; after delivery no robot is, so the wait for the
station's confirmation is not held. The four still sum to the order's life.

The summary names its two totals apart. *Duration* is
`mission_telemetry.duration_ms`, the same number under the same name as the
Missions list's column (created to the fleet's terminal report). In flight it
is the list's in-flight figure too, ticking from the order's creation and
saying "so far"; a finished order the fleet never summarised gets a titled em
dash. *Order life* is created to the
terminal history row, the sum of the stages; the two differ by the wait for
confirmation.

*Run time* is a third measure, named apart from both: robot assigned (the
first acknowledged or in-transit row) to load down (delivered, or the order's
failure), with fault time excluded. It measures the work, not the robot's
availability. The Overview's average tile, its P50/P95 trend and the drill
they open call it run time and carry the title "Robot assigned to load down;
fault time excluded"; none of them calls it duration.

A faulted row folds into the span it interrupted and is stated there as a
count and the time lost; the recovery continuation folds in with it. Started
times show seconds and carry the full plant-local datetime in the title.
Durations use the shared ladder, and an open span ticks and says "so far".

Robot actions (the fleet's per-block legs and state reports) render once,
inside one closed disclosure, grouped by the stage they happened in, with no
per-row chips. A leg's duration is a measurement, `unknown` or `not run`,
never a dash. The class map is keyed on protocol statuses and pinned whole by
`mission_state_vocabulary_drift_test.go`.
