package domain

import "testing"

// The two states are not interchangeable, and collapsing them is the mistake
// this type exists to prevent: a known-empty carrier is an answer you can act
// on, an unknown one is not. Core's carrierPayloadFor and the node-bins
// tri-state each draw the same distinction for the same reason.
func TestLinesideCarrier_KnownEmptyIsNotUnknown(t *testing.T) {
	t.Parallel()

	empty := KnownCarrier("")
	if p, ok := empty.Payload(); !ok || p != "" {
		t.Errorf("KnownCarrier(\"\").Payload() = (%q, %v), want (\"\", true) — an empty carrier is "+
			"a real answer, not an absence", p, ok)
	}
	if !empty.IsEmpty() {
		t.Error("a known carrier with no payload must report IsEmpty")
	}

	unknown := UnknownCarrier()
	if p, ok := unknown.Payload(); ok || p != "" {
		t.Errorf("UnknownCarrier().Payload() = (%q, %v), want (\"\", false)", p, ok)
	}
	if unknown.IsEmpty() {
		t.Error("an unknown carrier must NOT report IsEmpty. \"I cannot tell\" is not \"it is " +
			"empty\", and treating it as such is how an unreadable node gets material ordered onto it")
	}
}

// The zero value is unusable on purpose: a LinesideCarrier nobody filled in
// cannot be mistaken for an answer.
func TestLinesideCarrier_ZeroValueIsUnknown(t *testing.T) {
	t.Parallel()
	var zero LinesideCarrier
	if zero.Known() {
		t.Fatal("the zero value reports Known. A struct nobody filled in must read as " +
			"\"cannot say\", or an unset field becomes a confident wrong answer.")
	}
	if p, ok := zero.Payload(); ok || p != "" {
		t.Errorf("zero.Payload() = (%q, %v), want (\"\", false)", p, ok)
	}
	if zero != UnknownCarrier() {
		t.Error("the zero value must equal UnknownCarrier so the two spellings cannot drift")
	}
}

func TestLinesideCarrier_CarriesItsPayload(t *testing.T) {
	t.Parallel()
	c := KnownCarrier("63145-6TA1B.10")
	p, ok := c.Payload()
	if !ok || p != "63145-6TA1B.10" {
		t.Errorf("Payload() = (%q, %v), want the payload and true", p, ok)
	}
	if c.IsEmpty() {
		t.Error("a carrier with a payload is not empty")
	}
}

// LinesidePayloadCode is a named type so a claim's payload — a plain string,
// the REQUESTED identity — cannot be assigned to the runtime row's resident
// field without somebody writing the conversion down. That cannot be asserted
// from inside a passing test, since the mismatched assignment does not compile;
// what is pinned here is that the type still carries its value intact, which is
// the part a careless conversion could break.
//
// Its former partner, RequestedPayloadCode, is gone. It had zero non-test
// callers, so there was no assignment anywhere for the compiler to refuse, and
// the header claiming otherwise was the guarantee's only evidence.
func TestLinesidePayloadCode_RoundTrip(t *testing.T) {
	t.Parallel()
	if got := LinesidePayloadCode("PART-B").String(); got != "PART-B" {
		t.Errorf("LinesidePayloadCode.String() = %q", got)
	}
}
