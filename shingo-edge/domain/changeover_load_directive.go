package domain

import (
	"sort"

	"shingo/protocol"
)

// ChangeoverLoadDirective is what a loader's card says during a changeover:
// LOAD THIS BIN TYPE, because these cells are waiting for it.
//
// ── WHY THE CARD NEEDS TELLING AT ALL ─────────────────────────────────────
//
// A loader's board normally offers every payload the loader serves and lets
// the operator pick. During a changeover that is the wrong question: the cells
// changing over are waiting for one specific empty carrier, and every other
// choice on the board loads a bin nobody is asking for — which then sits on the
// window and blocks the one that is needed.
//
// BIN TYPE, NOT PAYLOAD, is what the operator physically fetches. An empty
// carrier is dunnage; the payload is what goes in it afterwards. Payloads are
// carried alongside only so the card can say what the bins are FOR.
type ChangeoverLoadDirective struct {
	// BinTypeCodes are the empty carrier types to load, deduplicated and
	// ordered. Plural because one changeover can put two presses onto
	// different dunnage at once.
	BinTypeCodes []string `json:"bin_type_codes"`
	// PayloadCodes are what those carriers are for — context on the card, not
	// the instruction.
	PayloadCodes []string `json:"payload_codes"`
	// ForNodes are the cells waiting, so the operator can see who this is for.
	ForNodes []string `json:"for_nodes"`
	// ChangeoverID is the episode this load belongs to. Carried so an order
	// created off this directive can be ATTRIBUTED to the changeover instead
	// of arriving as an orphan in demand-origin reporting.
	ChangeoverID int64 `json:"changeover_id"`
}

// BuildChangeoverLoadDirective computes what a loader should be told to load.
//
// toClaims are the incoming style's claims; binTypeFor resolves a payload to
// its dunnage code. A payload whose bin type is unknown contributes NOTHING —
// naming a carrier we cannot identify is worse than saying nothing, because
// the operator would go and fetch a guess.
//
// Returns nil when there is nothing to say: no changeover, the flag off, no
// resolvable bin type. A card that always shows a directive is a card whose
// directive nobody reads.
// directiveOn is the STATION's setting, read from the Core-owned loader
// (bin_loaders.changeover_load_directive) rather than from the claim. It used to
// be a claim column, which made a per-station policy per-(style, station): a
// loader serving six styles carried six copies that had to agree.
//
// NO ROLE GUARD. This used to refuse anything that was not a produce claim, on
// the reasoning that an unloader's directive would be about fulls. It is not a
// different instruction — it is the same one: "for the payloads this station
// serves, here is the carrier the incoming style needs". Whether a given station
// wants to be told that is the question the setting answers, and the answer is
// now the operator's to give where they set the station up.
func BuildChangeoverLoadDirective(
	changeoverID int64,
	directiveOn bool,
	loaderClaim *NodeClaim,
	toClaims []NodeClaim,
	binTypeFor func(payloadCode string) string,
) *ChangeoverLoadDirective {
	if changeoverID == 0 || !directiveOn || loaderClaim == nil {
		return nil
	}

	serves := loaderServes(loaderClaim)
	seenBin, seenPayload := map[string]bool{}, map[string]bool{}
	var out ChangeoverLoadDirective
	out.ChangeoverID = changeoverID

	for i := range toClaims {
		c := &toClaims[i]
		if c.Role != protocol.ClaimRoleProduce || c.PayloadCode == "" {
			continue
		}
		// Only payloads THIS loader can serve. A line's other loader's work is
		// not this operator's instruction.
		if len(serves) > 0 && !serves[c.PayloadCode] {
			continue
		}
		binType := binTypeFor(c.PayloadCode)
		if binType == "" {
			continue
		}
		if !seenBin[binType] {
			seenBin[binType] = true
			out.BinTypeCodes = append(out.BinTypeCodes, binType)
		}
		if !seenPayload[c.PayloadCode] {
			seenPayload[c.PayloadCode] = true
			out.PayloadCodes = append(out.PayloadCodes, c.PayloadCode)
		}
		out.ForNodes = append(out.ForNodes, c.CoreNodeName)
	}
	if len(out.BinTypeCodes) == 0 {
		return nil
	}
	// Deterministic: the card must not reshuffle between SSE refreshes.
	sort.Strings(out.BinTypeCodes)
	sort.Strings(out.PayloadCodes)
	sort.Strings(out.ForNodes)
	return &out
}

// loaderServes is the payload set this loader can actually load, or an empty
// map meaning "no restriction recorded".
func loaderServes(c *NodeClaim) map[string]bool {
	out := map[string]bool{}
	for _, p := range c.AllowedPayloads() {
		out[p] = true
	}
	return out
}
