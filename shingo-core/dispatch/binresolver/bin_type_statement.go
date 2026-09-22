package binresolver

import "fmt"

// Carrier is what a store resolution is putting down.
//
// ── WHY THIS IS A TYPE AND NOT A *int64 ─────────────────────────────────────
//
// It was `binTypeID *int64`, and nil meant two different things: "this may go
// anywhere" and "I could not work out what this is". The per-node Allowed Bin
// Types fence reads nil as the first, and the intake resolver passed nil meaning
// the second — so a knockdown resolved into a tote-only position at Hopkinsville
// on 2026-09-22 with every gate behaving exactly as written.
//
// That is not a bug in the fence. It is one value carrying two facts, which is
// the same shape that left payloadAllowedAt spelled into two of four branches
// and needed ngrpAtDeclaredLevel copied into the capacity gate to stop the two
// disagreeing. A caller cannot omit this any more: it says what it is placing,
// or it says why it does not know, and "I forgot" is not spellable.
//
// ── UNKNOWN IS LOUD, NOT SILENT ─────────────────────────────────────────────
//
// An unknown bin type still resolves UNTYPED — refusing would turn a read fault
// into a parked robot, and untyped is what every order did before the fence
// existed. What changes is that the resolver SAYS SO, naming the reason its
// caller gave. There are eight resolution sites and reasoning about which of
// them can name a bin type is how this was got wrong twice; a line in the plant's
// own log answers it from real traffic in a shift.
type BinTypeStatement struct {
	typeID *int64
	// why is the caller's account of the gap, non-empty exactly when typeID is
	// nil and this is not NoBinType. It is written into the log line, so it has
	// to name the SITE and the reason — "intake: order not yet persisted", not
	// "unknown".
	why string
	// placing distinguishes a store (something is being put down, so the fence
	// applies) from a retrieve (nothing is, so it does not). Without it a
	// retrieve's NoBinType would log a gap on every call.
	placing bool
}

// KnownBinType names the bin type being placed.
func KnownBinType(binTypeID int64) BinTypeStatement {
	return BinTypeStatement{typeID: &binTypeID, placing: true}
}

// BinTypeFrom lifts an already-optional id, for the callers that hold one and
// have their own account of the nil case.
func BinTypeFrom(binTypeID *int64, why string) BinTypeStatement {
	if binTypeID != nil {
		return KnownBinType(*binTypeID)
	}
	return UnknownBinType(why)
}

// UnknownBinType records that a store is being resolved without knowing what it
// places, and why. The reason is operator- and engineer-facing; name the site.
func UnknownBinType(why string) BinTypeStatement {
	if why == "" {
		why = "no reason given — a caller passed an unexplained unknown bin type"
	}
	return BinTypeStatement{why: why, placing: true}
}

// NoBinType is a resolution that places nothing: a retrieve. It is not an
// unknown, and it never reports a gap.
var NoBinType = BinTypeStatement{}

// TypeID is the bin type, or nil when it could not be named. nil narrows
// nothing — see binTypeAllowed.
func (c BinTypeStatement) TypeID() *int64 { return c.typeID }

// gap reports whether this resolution is placing something it cannot name,
// which is the condition worth a log line.
func (c BinTypeStatement) gap() bool { return c.placing && c.typeID == nil }

func (c BinTypeStatement) String() string {
	if c.typeID != nil {
		return fmt.Sprintf("bin type %d", *c.typeID)
	}
	if !c.placing {
		return "nothing (retrieve)"
	}
	return "UNKNOWN (" + c.why + ")"
}
