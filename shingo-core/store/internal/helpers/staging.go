package helpers

// staging.go — THE STAGING STATE, one spelling.
//
// A bin's staging state is three columns written together: status,
// staged_at and staged_expires_at. Three writers set it — bins.Stage,
// bins.ReleaseStaged and PlaceBinTx — plus the
// by-hand move's clearStaging arm, and each used to spell the triple in its own
// statement. They compose these fragments instead.
//
// PLACEHOLDERS ARE FIXED: $1 is the write time (clock.Now().UTC(), one clock
// for the whole triple and updated_at), $2 is the expiry for the staged form.
// A statement that composes a fragment numbers its own parameters after them.

// StagedSetSQL is the SET list that stages a bin. $1 = now, $2 = expiry (NULL
// means staged with no countdown).
const StagedSetSQL = `status='staged', staged_at=$1, staged_expires_at=$2, updated_at=$1`

// AvailableSetSQL is the SET list that makes a bin available and clears its
// staging timestamps. $1 = now.
const AvailableSetSQL = `status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=$1`

// StagingOwnsStatusSQL is the guard every status RE-DERIVATION composes: the
// staging writers own `available` and `staged` and nothing else. A flagged,
// maintenance or retired bin — or one in a value outside the enum — was put
// there by a person or a door on purpose, and an arrival or a move must not
// flatten it back to available.
//
// The same two values as SourceableStatusSQL, and a different fact: that one
// says which bins a robot may take, this one says which statuses the staging
// machine may overwrite. They are not composed from each other on purpose — a
// change to what is sourceable is not a change to what staging owns.
const StagingOwnsStatusSQL = `status IN ('available','staged')`

// StagedOnlySQL is the release guard: only a staged bin is released.
const StagedOnlySQL = `status='staged'`
