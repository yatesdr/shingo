package www

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"shingo/protocol"

	"shingocore/asks"
	"shingocore/service"
)

// partCreate is AskPartCreate's body: the floor create
// (service.PayloadService.CreateFromFloor) for the asking station, answered with
// the asking process's id and the part's catalog row and carrier rows as the
// catalog feed and the node list build them (PayloadService.CatalogRow), so the
// Edge applies the answer with no memory of the ask. A repeat of the same
// submit is answered the same way, marked a repeat. A request with no process
// is a bad body (400); every refusal is AnswerRefused with the sentence a person
// reads (partCreateRefusal).
func (d *Doors) partCreate(station string, req protocol.PartCreateRequest) asks.Result {
	if req.ProcessID < 1 {
		return badRequest("process_id is required")
	}
	parts := d.engine.PayloadService()
	p, repeat, err := parts.CreateFromFloor(station, req)
	if err != nil {
		return partCreateRefusal(req, err)
	}
	row, binTypes, err := parts.CatalogRow(p.Code)
	if err != nil {
		return refused(http.StatusInternalServerError,
			// OWNER-WORDS
			fmt.Sprintf("Core made part %s but could not read it back (%v). Send it again.", p.Code, err))
	}
	return accepted(protocol.PartCreateResponse{ProcessID: req.ProcessID, Payload: row, BinTypes: binTypes, Repeat: repeat})
}

// partCreateRefusal is the sentence the operator reads for each refusal of a
// part made at the floor, at AnswerRefused. The checks are the service's, the
// desktop's among them; the words are the floor's. Anything the service did
// not refuse by name is Core's failure (AnswerFailed), its own text.
func partCreateRefusal(req protocol.PartCreateRequest, err error) asks.Result {
	number := strings.TrimSpace(req.PartNumber)
	carrier := strings.TrimSpace(req.BinTypeCode)
	var exists *service.PartExistsError
	var taken *service.CATIDTakenError
	var conflict *service.CATIDConflict
	var sentence string
	switch {
	case errors.Is(err, service.ErrPayloadCode):
		// OWNER-WORDS
		sentence = "The part number is blank."
	case errors.Is(err, service.ErrPayloadCapacity):
		// OWNER-WORDS
		sentence = fmt.Sprintf("The standard pack must be 1 or more (got %d).", req.StandardPack)
	case errors.Is(err, service.ErrPayloadToteNumber):
		// OWNER-WORDS
		sentence = "Tote layers and UoP per tote must be 1 or more, or left blank."
	case errors.Is(err, service.ErrUnknownCarrierType):
		// OWNER-WORDS
		sentence = fmt.Sprintf("Core has no carrier type %q.", carrier)
	case errors.Is(err, service.ErrBareTypeInPayloadRule):
		// OWNER-WORDS
		sentence = fmt.Sprintf("%s is a bare carrier type. Pick the carrier the part rides in.", carrier)
	case errors.As(err, &exists) && exists.Near:
		// OWNER-WORDS
		sentence = fmt.Sprintf("This part already exists on Core as %s.", exists.Code)
	case errors.As(err, &exists):
		// OWNER-WORDS
		sentence = fmt.Sprintf("Part %s already exists on Core. An engineer adds it to this process.", exists.Code)
	case errors.As(err, &taken):
		// OWNER-WORDS
		sentence = fmt.Sprintf("CATID %s is already on part %s.", taken.CATID, taken.PartNumber)
	case errors.As(err, &conflict):
		// OWNER-WORDS
		sentence = fmt.Sprintf("Core knows part %s with CATID %s. Leave CATID blank to keep it, or ask an engineer.",
			number, conflict.Existing)
	default:
		return refused(protocol.AnswerFailed, err.Error())
	}
	return refused(protocol.AnswerRefused, sentence)
}
