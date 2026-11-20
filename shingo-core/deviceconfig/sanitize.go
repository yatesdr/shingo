package deviceconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// credentialKeys are the param keys whose VALUES are never stored or shown
// (brief, credential masking). Matched case-insensitively on the exact key —
// never by substring (the fixture's passive/passiveMotorName keys would
// trip). 密码 is measured live on both plants' payloads (empty today;
// plaintext the day it isn't — round-2, cobalt-heron M4 / umber-lynx).
var credentialKeys = map[string]bool{
	"password": true,
	"passwd":   true,
	"pwd":      true,
	"secret":   true,
	"token":    true,
	"密码":       true,
}

// CredentialKey reports whether a param key names a credential.
func CredentialKey(key string) bool {
	return credentialKeys[strings.ToLower(strings.TrimSpace(key))]
}

// Fingerprint is the deterministic one-way stand-in for a credential value:
// "sha256:" + the hex digest of the value. Same value ⇒ same fingerprint,
// so value equality — and therefore the matrix comparison and red — is
// unaffected while nothing holds a usable password. A hash of a low-entropy
// default is still guessable by someone with table access; the fingerprint
// stops casual reading, not targeted attack (the brief's honest caveat).
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Sanitize replaces every credential param's value in place with its
// fingerprint and returns the tree serialized. This runs at PULL time,
// before either JSONB column is written: the ESP32 mesh wifi password rides
// the live #11500 payload of every robot measured (evidence/README.md), so
// an unsanitized archive is a stored credential. The string the comparison
// sees is the fingerprint, never the value; there is no reveal because
// nothing real is kept.
//
// The in-place mutation is LOAD-BEARING, not an implementation detail: the
// pull handler flattens Leaves from the SAME tree after Sanitize returns,
// which is why the params column is clean without a second pass — a
// sanitize-that-copied would leave the flattened rows holding plaintext.
// Callers that need the original values must deep-copy before calling.
//
// The output is deterministic: typed structs marshal their fields in a
// fixed order, and only value fields change.
func Sanitize(m *ModelFile) ([]byte, error) {
	for i := range m.DeviceTypes {
		for j := range m.DeviceTypes[i].Devices {
			sanitizeParams(m.DeviceTypes[i].Devices[j].DeviceParams)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("deviceconfig sanitize: %w", err)
	}
	return out, nil
}

func sanitizeParams(ps []Param) {
	for i := range ps {
		p := &ps[i]
		if CredentialKey(p.Key) && p.StringValue != nil && *p.StringValue != "" {
			v := Fingerprint(*p.StringValue)
			p.StringValue = &v
		}
		if p.Combo != nil {
			for _, ch := range p.Combo.ChildParams {
				sanitizeParams(ch.Params)
			}
		}
		if p.Array != nil {
			sanitizeParams(p.Array.Params)
		}
	}
}
