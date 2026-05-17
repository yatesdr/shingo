package service

import (
	"database/sql"

	"shingocore/store"
	"shingocore/store/payloads"
)

// BinManifestStore is the narrow DB surface BinManifestService depends on.
//
// Declaring it consumer-side does two things:
//
//  1. *store.DB satisfies it for free (Go interface satisfaction is
//     structural), so engine wiring does not change.
//  2. Step 7 (the WWW handler fake-engine work) can drop a hand-rolled
//     fake into BinManifestService.db for tests that exercise the
//     handler's interaction with the service without needing a real DB.
//
// Method count is deliberately small. BinManifestService is dominated
// by transactional SQL (Begin → Exec → Commit) with audit-row inserts
// in the same tx; most BinManifestService tests need a real database
// and stay docker-backed. The interface exists to make the dependency
// explicit, not to enable wholesale test conversion at this layer —
// that happens at the service-interface layer in step 7.
//
// The Exec method satisfies audit.BinUOPExecer at the call site
// (AuditReleaseOverride → audit.AppendBinUOPOverride) without
// requiring an additional wrapper on *store.DB.
type BinManifestStore interface {
	// Transaction primitive — used by the 5 transactional methods on
	// BinManifestService (ClearForReuse, SetForProduction, ClearAndClaim,
	// SyncUOPAndClaim, syncOrClearForReleased). Returns concrete *sql.Tx
	// because the bodies use raw tx.Exec / tx.QueryRow SQL; tests that
	// need to drive these methods need a real Postgres.
	Begin() (*sql.Tx, error)

	// Exec lets BinManifestStore satisfy audit.BinUOPExecer when
	// AuditReleaseOverride passes the store to audit.AppendBinUOPOverride.
	Exec(query string, args ...any) (sql.Result, error)

	// Bin manifest mutations.
	ConfirmBinManifest(binID int64, producedAt string) error
	UnconfirmBinManifest(binID int64) error
	ClaimBin(binID, orderID int64) error

	// High-level audit-row append (legacy audit table, not bin_uop_audit
	// — that one is reached via Exec / the BinUOPExecer interface).
	AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error

	// Payload-template lookups for SetFromTemplate. *store.DB has typed
	// wrappers for the payloads-package free functions; using them here
	// keeps the interface method-only (no embedded-field access).
	GetPayloadByCode(code string) (*payloads.Payload, error)
	ListPayloadManifest(payloadID int64) ([]*payloads.ManifestItem, error)
}

// Compile-time check that *store.DB satisfies BinManifestStore. If the
// store package drops or renames any of the methods above, this
// assertion catches it before the build fails somewhere further
// downstream.
var _ BinManifestStore = (*store.DB)(nil)
