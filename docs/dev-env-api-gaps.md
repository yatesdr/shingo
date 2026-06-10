# Dev-Env Engine-API Gaps

Places where the local-dev-env sim/seed code was forced into **raw SQL** or
touching a **private field** because no store accessor / engine method existed.
This is the engine-API backlog: each entry is a candidate for a proper accessor.

Per `implementation-brief.md` §0.7. Created on branch `local-dev-env`.

**Status (through Phase 3 / T3.1):** no gaps yet. The sim code so far goes through
existing APIs — the fleet simulator/driver use the `fleet`/`simulator` methods,
the fake WarLink implements the public `plc.WarlinkClient` interface, and config
uses the typed config structs. Raw-SQL needs are most likely to appear in
Phase 4 (the seed tool writing topology), per the brief.

| # | Where (sim/seed file) | What was missing | Workaround used | Suggested accessor |
|---|---|---|---|---|
| _ | _(none recorded yet)_ | | | |
