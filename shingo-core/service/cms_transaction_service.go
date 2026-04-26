package service

import (
	"shingocore/store"
	"shingocore/store/cms"
)

// CMSTransactionService exposes read-only CMS transaction listings.
// Handlers call CMSTransactionService instead of reaching through
// engine passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type CMSTransactionService struct {
	db *store.DB
}

func NewCMSTransactionService(db *store.DB) *CMSTransactionService {
	return &CMSTransactionService{db: db}
}

// ListByNode returns the most recent CMS transactions filed against a
// single node, paginated by limit/offset.
func (s *CMSTransactionService) ListByNode(nodeID int64, limit, offset int) ([]*cms.Transaction, error) {
	return s.db.ListCMSTransactions(nodeID, limit, offset)
}

// ListAll returns the most recent CMS transactions across every
// node, paginated by limit/offset.
func (s *CMSTransactionService) ListAll(limit, offset int) ([]*cms.Transaction, error) {
	return s.db.ListAllCMSTransactions(limit, offset)
}
