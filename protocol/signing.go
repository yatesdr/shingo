package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// ErrInvalidSignature is returned when HMAC verification fails.
var ErrInvalidSignature = errors.New("protocol: invalid message signature")

// signedWire is the wire format when signing is enabled.
type signedWire struct {
	Envelope json.RawMessage `json:"env"`
	Sig      string          `json:"sig"`
}

// Sign wraps encoded envelope bytes with an HMAC-SHA256 signature.
// Returns the signed wire format: {"env": <original>, "sig": "<hex hmac>"}.
func Sign(envelopeData []byte, key []byte) ([]byte, error) {
	mac := hmac.New(sha256.New, key)
	mac.Write(envelopeData)
	sig := hex.EncodeToString(mac.Sum(nil))

	return json.Marshal(signedWire{
		Envelope: envelopeData,
		Sig:      sig,
	})
}

// VerifyAndUnwrap checks the HMAC signature and returns the inner envelope bytes.
// If signing key is nil/empty, returns data unchanged (signing disabled).
func VerifyAndUnwrap(data []byte, key []byte) ([]byte, error) {
	if len(key) == 0 {
		return data, nil
	}

	var sw signedWire
	if err := json.Unmarshal(data, &sw); err != nil {
		return nil, ErrInvalidSignature
	}
	if sw.Sig == "" || len(sw.Envelope) == 0 {
		return nil, ErrInvalidSignature
	}

	expectedSig, err := hex.DecodeString(sw.Sig)
	if err != nil {
		return nil, ErrInvalidSignature
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(sw.Envelope))
	if !hmac.Equal(mac.Sum(nil), expectedSig) {
		return nil, ErrInvalidSignature
	}

	return sw.Envelope, nil
}
