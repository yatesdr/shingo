// Package wire turns cms_transactions rows into the JSON array the middleware
// accepts.
//
// PURE, AND THAT IS THE DESIGN. No database, no clock, no config globals, no
// closures over a lookup — everything a row needs is already on the row,
// because the builder stamped it there. An earlier shape had the translator
// hold a stockLocationLookup closure that reached back into the node tree for
// each row's storeroom; that made the package untestable as a table of inputs
// and put a query inside a loop over a POST body. cms_transactions.storeroom
// exists so this file does not need one.
//
// The package is named wire rather than cms because store/cms already holds
// the persistence types, and two packages called cms would have to be aliased
// at every import.
package wire

import (
	"sort"

	"shingocore/store/cms"
)

// MiddlewareTx is one line of the middleware's inventory_transactions array.
//
// The field names and JSON keys follow the vendor's sample. THEY ARE NOT
// SHINGO'S VOCABULARY and should not be renamed to match it: StockLocation is
// what CMS calls a storeroom, Resource is what it calls the thing that did the
// moving, and Bin is its own bin concept rather than shingo's carrier. Keeping
// their names lets a person compare this struct against the vendor document
// line by line, which is the only check available until a real POST is
// answered.
type MiddlewareTx struct {
	// TicketNumber is 1 for everything, per the vendor's sample. It is not an
	// identifier we control and must not be used as one — it does not
	// distinguish two postings, so it cannot serve as an idempotency key.
	TicketNumber int `json:"TicketNumber"`
	// EntryNumber is 1-based within THIS array, assigned over a stable sort.
	EntryNumber   int    `json:"EntryNumber"`
	PartNumber    string `json:"PartNumber"`
	StockLocation string `json:"StockLocation"`
	Bin           string `json:"Bin"`
	// Quantity is UNSIGNED. Direction lives in TransactionType, which is the
	// vendor's model: a decrease is a positive quantity of a decrease, not a
	// negative quantity. Shipping a signed value here would double the sign.
	Quantity        int64  `json:"Quantity"`
	TransactionType string `json:"TransactionType"`
	Resource        string `json:"Resource"`
	ReasonCode      string `json:"ReasonCode"`
	UnitOfMeasure   string `json:"UnitOfMeasure"`
	UserID          string `json:"UserId"`
	Department      string `json:"Department"`
	Operation       string `json:"Operation"`
}

// Config is the vocabulary CMS expects, passed in rather than read from a
// package-level variable so a test can state it and a second plant could
// differ without this package knowing there is more than one.
type Config struct {
	// Department and Operation are blank in the vendor's sample and blank in
	// the shipped defaults. They are FIELDS rather than the hardcoded ""s they
	// were, because the followup list's promise is that all CMS vocabulary is a
	// yaml edit — and two of the thirteen being a code change and a release
	// contradicted that on the day SCO comes back with values for them.
	Department    string
	Operation     string
	ReasonCode    string
	IncreaseType  string
	DecreaseType  string
	UnitOfMeasure string
	UserID        string
}

// Build converts transaction rows into the middleware array.
//
// ORDERING IS BY ROW ID, NOT BY SLICE POSITION. EntryNumber is assigned over
// that order, so the same set of rows always serialises identically — which is
// what makes body_sha a usable dedup key and what makes a retry re-send the
// same body rather than a permutation of it. Sorting a copy leaves the
// caller's slice alone.
//
// Zero-delta rows are dropped. A transfer of nothing is noise in a ledger, and
// it has no direction to put in TransactionType — sign(0) would have to pick
// one and would be wrong half the time by construction.
func Build(txns []*cms.Transaction, cfg Config) []MiddlewareTx {
	ordered := make([]*cms.Transaction, 0, len(txns))
	for _, t := range txns {
		if t == nil || t.Delta == 0 {
			continue
		}
		ordered = append(ordered, t)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	out := make([]MiddlewareTx, 0, len(ordered))
	for i, t := range ordered {
		qty := t.Delta
		txnType := cfg.IncreaseType
		if qty < 0 {
			qty = -qty
			txnType = cfg.DecreaseType
		}
		out = append(out, MiddlewareTx{
			// TICKET NUMBER IS A CONSTANT 1 AND NOBODY HAS CONFIRMED WHAT IT
			// MEANS. The field was matched to a single vendor sample; whether
			// CMS assigns the ticket, expects the caller to, or wants one per
			// movement is an open question with IT and this is the site it
			// lands on. It has never been sent — cms_postings has never existed
			// at a plant — so it is a question, not an incident.
			TicketNumber: 1,
			EntryNumber:  i + 1,
			// THE LINE'S PART NUMBER. A transaction is one manifest line's
			// movement, and after the identity correction a line names the part
			// CMS books against — which for a bin of ONE part is the payload
			// code and for a kit is each component's own number.
			//
			// IT CARRIED t.PayloadCode FOR ONE COMMIT, and that was a workaround
			// for uncorrected data rather than a reading of the contract. It is
			// right for a single-part payload and it DOUBLE-BOOKS a kit: one
			// transaction per line, every one of them naming the payload, is a
			// payload-15 bin of 1000 posting 1000 twice. What makes this line
			// safe is not the binding, it is the guard on the other side —
			// material.BuildMovementTransactions refuses to build a movement
			// whose lines do not resolve to parts, so a value that reaches here
			// has been through the correction.
			PartNumber:      t.CatID,
			StockLocation:   t.Storeroom,
			Bin:             t.BinLabel,
			Quantity:        qty,
			TransactionType: txnType,
			// Blank for an operator drag: no robot moved it, and an invented
			// resource would be a claim about the plant that is not true.
			Resource:      t.RobotID,
			ReasonCode:    cfg.ReasonCode,
			UnitOfMeasure: cfg.UnitOfMeasure,
			UserID:        cfg.UserID,
			// Empty by default, per the vendor's sample, and declared rather
			// than omitted so the body's shape does not change when SCO fills
			// them in — which is now a yaml edit.
			Department: cfg.Department,
			Operation:  cfg.Operation,
		})
	}
	return out
}
