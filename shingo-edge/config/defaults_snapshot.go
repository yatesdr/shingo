package config

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// defaults_snapshot.go — the shipped defaults, rendered so a change to one is a
// DIFF.
//
// PHASE 1 APPLIED TO BEHAVIOUR INSTEAD OF SCHEMA, and the parallel is exact.
// The defaults ARE the schema of how this system behaves when nobody has said
// otherwise: there is no file you can open to see them all, they are scattered
// across struct tags and accessor fallbacks, and changing one is a
// one-character edit that is invisible in review. That is the same shape as the
// baseline-DDL problem, and it fails the same way — silently, and only on the
// machines that matter.
//
// A test cannot close this on its own. The hysteresis band test used to pin
// 10%; at margin 0 it would still have been GREEN while asserting nothing,
// because the band it checked would not have existed. A test that leans on a
// default goes vacuous the day someone retunes it, and it keeps looking like
// coverage — which is worse than deleting it. The band test is now written as a
// property over a range so it cannot go vacuous; this file is the systemic
// backstop for every default that has no such test.
//
// WHAT IT PINS AND WHAT IT DOES NOT. It renders the SHIPPED CODE DEFAULT — what
// every installation gets when its yaml says nothing. A plant overriding
// hysteresis_percent in its own config does not touch this file and does not
// break anything, which is correct: a per-plant tuning decision is local. What
// requires acknowledgement is changing what EVERYONE gets, and that now shows
// up as a reviewable line rather than a silent edit — "what depended on 10%?"
// is a question somebody can ask.
//
// Regenerate with `make defaults-snapshot`. TestDefaultsSnapshotIsCurrent fails
// with that command in its message.

// DefaultsSnapshotPath is the committed rendering, relative to the module root.
const DefaultsSnapshotPath = "config/defaults.snapshot.txt"

// DefaultsRegenCommand is the exact command the staleness test names.
const DefaultsRegenCommand = "make defaults-snapshot"

// RenderDefaults produces the stable text rendering of the shipped defaults.
//
// Two sections, because a raw field dump is not the whole story:
//
//   - FIELDS — the zero-value Config's fields, which is what an installation
//     with an empty yaml actually holds.
//   - RESOLVED — values computed by accessors that apply a fallback. These are
//     the ones that matter most and the ones a field dump misses entirely: a
//     nil *float64 tells a reviewer nothing, while "margin at reorder_point 50
//     = 5" is the behaviour.
func RenderDefaults() string {
	var b strings.Builder
	b.WriteString("# shingo-edge shipped defaults — generated, do not edit by hand.\n")
	b.WriteString("# See config/defaults_snapshot.go. Regenerate: " + DefaultsRegenCommand + "\n\n")

	b.WriteString("[FIELDS]\n")
	// Defaults(), not a zero value: this is what Load() starts from before any
	// yaml is read, so it is what an installation with an empty config actually
	// runs on. A zero-value dump would be a page of empty strings that only
	// changes when a FIELD is added — missing the thing this file exists to
	// catch, which is somebody editing a shipped VALUE.
	cfg := Defaults()
	lines := renderStruct("", reflect.ValueOf(cfg).Elem())
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}

	b.WriteString("\n[RESOLVED]\n")
	// Behaviour, not storage. A reviewer reading "hysteresis_percent = <nil>"
	// learns nothing; the margin at a representative reorder point is the thing
	// that would actually change.
	for _, rp := range []int{0, 5, 10, 50, 100, 300} {
		b.WriteString(fmt.Sprintf("demand.hysteresis_margin(reorder_point=%d) = %d\n", rp, cfg.HysteresisMargin(rp)))
	}
	b.WriteString(fmt.Sprintf("demand.default_hysteresis_percent = %g\n", DefaultHysteresisPercent))
	b.WriteString(fmt.Sprintf("demand.min_hysteresis_uop = %d\n", MinHysteresisUOP))
	// Resolved the same way engine.multiWindowEnabled does: nil means ON.
	// Exactly the kind of inversion a raw "<unset>" in the field dump hides.
	b.WriteString(fmt.Sprintf("loaders_multi_window.resolved = %v\n",
		cfg.LoadersMultiWindow == nil || *cfg.LoadersMultiWindow))
	// Same reason: the raw field reads "0s", which looks like "no delay — always
	// ask" and means the opposite.
	b.WriteString(fmt.Sprintf("uop_accumulating_cta_after.resolved = %v\n",
		cfg.UOPAccumulatingCTADelay()))
	return b.String()
}

// renderStruct walks exported fields, using yaml tag names where present so the
// rendering matches what an operator would write in a config file.
func renderStruct(prefix string, v reflect.Value) []string {
	var out []string
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name := f.Name
		if tag := f.Tag.Get("yaml"); tag != "" && tag != "-" {
			name = strings.Split(tag, ",")[0]
		} else if tag == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(time.Time{}) {
			out = append(out, renderStruct(path, fv)...)
			continue
		}
		if isSecretField(f, name) {
			// TWO REASONS, either sufficient. Defaults() GENERATES a random
			// session secret, so rendering it would make this file differ from
			// itself on every run and the staleness test would fail
			// permanently on a tree nobody touched. And a snapshot that renders
			// secrets is a snapshot that COMMITS them.
			//
			// Whether a secret is set at all is still visible, which is the
			// reviewable fact — a default that stopped being generated would
			// show up here.
			out = append(out, path+" = "+secretState(fv))
			continue
		}
		out = append(out, path+" = "+renderValue(fv))
	}
	return out
}

func renderValue(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			// A nil pointer is how "unset, use the fallback" is spelled. Say so
			// rather than printing an address.
			return "<unset>"
		}
		return renderValue(v.Elem())
	case reflect.Slice, reflect.Map:
		if v.IsNil() || v.Len() == 0 {
			return "<empty>"
		}
		return fmt.Sprintf("%v", v.Interface())
	case reflect.Int64:
		// time.Duration is an int64; print it as a duration or the number is
		// meaningless in review.
		if v.Type() == reflect.TypeOf(time.Duration(0)) {
			return time.Duration(v.Int()).String()
		}
		return fmt.Sprintf("%d", v.Int())
	default:
		return fmt.Sprintf("%v", v.Interface())
	}
}

// SECRET DETECTION IS BELT AND BRACES, because the failure mode is a
// credential in git and there is no undoing that.
//
//  1. An explicit `snapshot:"secret"` struct tag — the declaration, which is
//     the only signal that cannot be defeated by a rename.
//  2. A field-NAME heuristic, as the safety net for a field whose author did
//     not know about the tag.
//  3. A scan of the RENDERED OUTPUT (see scanForUnredactedSecrets) that fails
//     the test if anything got through anyway.
//
// The name heuristic alone was the original design and it is not enough: it
// breaks the day somebody adds `apiToken` spelled `bearer` or `pat`. The tag
// alone is not enough either, because it has to be remembered. The output scan
// is what makes a miss loud rather than silent.
var secretMarkers = []string{"secret", "password", "key", "token", "credential", "passwd", "auth"}

func isSecretField(f reflect.StructField, name string) bool {
	if tag := f.Tag.Get("snapshot"); tag == "secret" {
		return true
	}
	lower := strings.ToLower(name + " " + f.Name)
	for _, m := range secretMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// secretLike matches a rendered VALUE that looks like a credential regardless
// of what its field is called: a long unbroken run of hex or base64-ish
// characters is not a hostname, a path or a duration.
var secretLike = regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`)

// ScanForUnredactedSecrets returns the rendered lines whose value looks like a
// credential. Empty is the only acceptable result.
//
// This is the backstop that makes the two detectors above fail LOUDLY instead
// of silently. It is deliberately dumb: it does not know what a secret is, only
// that a 24-character opaque blob has no business in a defaults file.
func ScanForUnredactedSecrets(rendered string) []string {
	var bad []string
	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		_, value, found := strings.Cut(line, " = ")
		if !found || strings.HasPrefix(value, "<") {
			continue
		}
		if secretLike.MatchString(value) {
			bad = append(bad, line)
		}
	}
	return bad
}

// secretState reports only whether a secret has a value, never what it is.
// "generated" vs "unset" is the reviewable fact — a default that stopped being
// generated would change this line.
func secretState(v reflect.Value) string {
	if v.Kind() == reflect.String {
		if v.String() == "" {
			return "<unset>"
		}
		return "<set: generated or shipped>"
	}
	return "<redacted>"
}
