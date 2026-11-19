// Package deviceconfig parses the robot model file that RoboKit #1500
// returns (RoboShop's Device Config), flattens it into comparison rows, and
// computes the fleet matrix for the Robots page's Device Config tab.
//
// It mirrors scenemap: a top-level package for parsing a vendor robot
// artifact, owning the vendor shape so neither rds (the wire) nor www (the
// page) has to. www, engine and service import no shingocore/rds; only the
// seerrds adapter talks to it.
//
// The parse target is a real robot's file, measured 2026-11-19 against
// AMR/SJV-SW600.model (51 sections, 80 devices, 530 live leaves) and
// re-measured live the same day: the #1500 response is a BARE model —
// top-level keys exactly {model, deviceTypes}, no wrapper envelope
// (work/deviceconfig-tab-2026-11-19/evidence). Same shape, same ten param
// types and same value fields on Hopkinsville's W1500SL family.
package deviceconfig

import (
	"encoding/json"
	"fmt"
)

// ModelFile is the whole #1500 document.
type ModelFile struct {
	Model       string       `json:"model"`
	DeviceTypes []DeviceType `json:"deviceTypes"`
}

// DeviceType is a SECTION of the matrix.
type DeviceType struct {
	Name     string   `json:"name"`
	Desc     string   `json:"desc"`
	MaxCount int      `json:"maxCount"`
	Ord      int      `json:"ord"`
	Devices  []Device `json:"devices"`
}

// Device is a SUBSECTION of the matrix. IsDisplay is carried but NEVER
// filtered on: RoboShop shows CAN1/CAN2, which the fixture flags
// isDisplay:false, so the flag's meaning is not understood and filtering on
// it would hide rows the plant compares (brief, parser rules).
type Device struct {
	Name         string  `json:"name"`
	Desc         string  `json:"desc"`
	IsDisplay    bool    `json:"isDisplay"`
	IsEnabled    bool    `json:"isEnabled"`
	DeviceParams []Param `json:"deviceParams"`
}

// Param is one entry of a device's deviceParams — a leaf in the simple
// case, or a combo (value = the selected choice) or an array group (recurse)
// in the cases the census measured.
//
// Typed value fields are WIDTH-SPECIFIC (int32Value, not intValue) with
// bounds in the same spelling. Every field is optional and nil means "this
// encoding does not carry it"; which ones are set follows Type.
type Param struct {
	Key         string `json:"key"`
	Desc        string `json:"desc"`
	Type        string `json:"type"`
	Unit        string `json:"unit"`
	CloneEnable bool   `json:"cloneEnable"`

	Combo *ComboParam `json:"comboParam,omitempty"`
	Array *ArrayParam `json:"arrayParam,omitempty"`

	Int32Value     *int32   `json:"int32Value,omitempty"`
	Int32Maxvalue  *int32   `json:"int32Maxvalue,omitempty"`
	Int32Minvalue  *int32   `json:"int32Minvalue,omitempty"`
	Uint32Value    *uint32  `json:"uint32Value,omitempty"`
	Uint32Maxvalue *uint32  `json:"uint32Maxvalue,omitempty"`
	Uint32Minvalue *uint32  `json:"uint32Minvalue,omitempty"`
	DoubleValue    *float64 `json:"doubleValue,omitempty"`
	DoubleMaxvalue *float64 `json:"doubleMaxvalue,omitempty"`
	DoubleMinvalue *float64 `json:"doubleMinvalue,omitempty"`
	BoolValue      *bool    `json:"boolValue,omitempty"`
	StringValue    *string  `json:"stringValue,omitempty"`
}

// ComboParam is a choice param: ChildKey is the CURRENT SELECTION (the
// value the matrix compares), ChildParams the legal choices. A choice may
// carry its own subparams (Trigger's source=DI has a DI index; func=
// runScript has name+args) — only the SELECTED choice's are live rows; the
// rest are kept for the v4 edit UI, never rendered.
type ComboParam struct {
	ChildKey    string       `json:"childKey"`
	ChildParams []ChildParam `json:"childParams"`
}

// ChildParam is one legal choice of a combo.
type ChildParam struct {
	Key    string  `json:"key"`
	Desc   string  `json:"desc"`
	Params []Param `json:"params"`
}

// ArrayParam is a group level: its own key ("basic") folds into the row
// path ("basic: terminalResistance"). One group level seen in the census;
// nested levels recurse the same way.
type ArrayParam struct {
	Params []Param `json:"params"`
}

// Parse decodes a #1500 response body.
func Parse(raw []byte) (*ModelFile, error) {
	var m ModelFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("deviceconfig parse: %w", err)
	}
	if m.Model == "" || len(m.DeviceTypes) == 0 {
		return nil, fmt.Errorf("deviceconfig parse: not a model file (model=%q, %d deviceTypes)", m.Model, len(m.DeviceTypes))
	}
	return &m, nil
}

// Value is the param's canonical comparison string: the selected choice for
// a combo, the typed value otherwise, "" when the encoding carries no value
// (type "empty"). Equal strings mean equal values; the matrix never
// compares raw bytes.
func (p Param) Value() string {
	if p.Combo != nil {
		return p.Combo.ChildKey
	}
	if p.StringValue != nil {
		return *p.StringValue
	}
	if p.Int32Value != nil {
		return fmt.Sprintf("%d", *p.Int32Value)
	}
	if p.Uint32Value != nil {
		return fmt.Sprintf("%d", *p.Uint32Value)
	}
	if p.DoubleValue != nil {
		return fmt.Sprintf("%v", *p.DoubleValue)
	}
	if p.BoolValue != nil {
		return fmt.Sprintf("%t", *p.BoolValue)
	}
	return ""
}

// Range is the hover's bounds line, "" when the encoding carries none.
func (p Param) Range() string {
	lo, hi := "", ""
	if p.Int32Minvalue != nil {
		lo = fmt.Sprintf("%d", *p.Int32Minvalue)
	} else if p.Uint32Minvalue != nil {
		lo = fmt.Sprintf("%d", *p.Uint32Minvalue)
	} else if p.DoubleMinvalue != nil {
		lo = fmt.Sprintf("%v", *p.DoubleMinvalue)
	}
	if p.Int32Maxvalue != nil {
		hi = fmt.Sprintf("%d", *p.Int32Maxvalue)
	} else if p.Uint32Maxvalue != nil {
		hi = fmt.Sprintf("%d", *p.Uint32Maxvalue)
	} else if p.DoubleMaxvalue != nil {
		hi = fmt.Sprintf("%v", *p.DoubleMaxvalue)
	}
	if lo == "" && hi == "" {
		return ""
	}
	return lo + ".." + hi
}

// hasSubparams reports whether any choice of the combo carries params —
// the difference between a bare-name row (baudrate) and a `param: type`
// selection row with live subparam rows (mode, shape, source, func).
func (p Param) hasSubparams() bool {
	if p.Combo == nil {
		return false
	}
	for _, ch := range p.Combo.ChildParams {
		if len(ch.Params) > 0 {
			return true
		}
	}
	return false
}

// choiceParams returns the SELECTED choice's params — the live subparams.
// The choice is unmatched (childKey not in childParams) only in a corrupt
// file; the params then simply do not exist and the caller renders nothing,
// which is the honest cell for a file that does not describe itself.
func (p Param) choiceParams() []Param {
	if p.Combo == nil {
		return nil
	}
	for _, ch := range p.Combo.ChildParams {
		if ch.Key == p.Combo.ChildKey {
			return ch.Params
		}
	}
	return nil
}
