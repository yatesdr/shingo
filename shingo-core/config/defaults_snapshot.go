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
// The same machinery as shingo-edge's config/defaults_snapshot.go, and for the
// same reason: the defaults are how Core behaves when nobody has said
// otherwise, there is no file you can open to see them all, and retuning one is
// a one-character edit that is invisible in review. With the rendering
// committed, it is a line somebody can question.
//
// It is a copy rather than a shared package on purpose. docs/shared-layer-promotion.md
// promotes code only where Core and Edge disagreeing is a defect that reaches
// the floor; two renderings of two modules' own defaults can drift apart
// harmlessly, so each module owns its own.
//
// WHAT IT PINS. The SHIPPED CODE DEFAULT — what every installation gets when
// its yaml says nothing. A plant overriding a value in its own config does not
// touch this file.
//
// The committed rendering is also what the docs' configuration key table is
// generated from (shared/scripts/docgen): the default column of
// shingo-core/docs/configuration.md is this file's [FIELDS] section.
//
// Regenerate with `make defaults-snapshot`. TestDefaultsSnapshotIsCurrent fails
// with that command in its message.

// DefaultsSnapshotPath is the committed rendering, relative to the module root.
const DefaultsSnapshotPath = "config/defaults.snapshot.txt"

// DefaultsRegenCommand is the exact command the staleness test names.
const DefaultsRegenCommand = "make defaults-snapshot"

// RenderDefaults produces the stable text rendering of the shipped defaults.
//
// Two sections, as on Edge:
//
//   - FIELDS — Defaults()'s fields by yaml path, which is what an installation
//     with an empty yaml actually holds.
//   - RESOLVED — values an accessor computes with a fallback, which a field
//     dump shows only as the zero it falls back from.
func RenderDefaults() string {
	var b strings.Builder
	b.WriteString("# shingo-core shipped defaults — generated, do not edit by hand.\n")
	b.WriteString("# See config/defaults_snapshot.go. Regenerate: " + DefaultsRegenCommand + "\n\n")

	b.WriteString("[FIELDS]\n")
	cfg := Defaults()
	lines := renderStruct("", reflect.ValueOf(cfg).Elem())
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}

	b.WriteString("\n[RESOLVED]\n")
	// The field ships as zero; the probe Connect runs uses this.
	b.WriteString(fmt.Sprintf("messaging.kafka.dial_timeout(effective) = %s\n", cfg.Messaging.Kafka.DialTimeoutOr()))
	return b.String()
}

// renderStruct walks exported fields, using yaml tag names so the rendering
// matches what an operator would write in a config file. A struct field other
// than time.Time is a section and is walked; everything else is a leaf.
func renderStruct(prefix string, v reflect.Value) []string {
	var out []string
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		name := f.Name
		if tag := f.Tag.Get("yaml"); tag == "-" {
			continue
		} else if tag != "" {
			name = strings.Split(tag, ",")[0]
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
			// A snapshot that renders secrets is a snapshot that commits them.
			// Whether one is set at all is still visible, which is the
			// reviewable fact.
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

// Secret detection is belt and braces, as on Edge: an explicit
// `snapshot:"secret"` tag, a field-name heuristic for the field whose author
// did not know about the tag, and ScanForUnredactedSecrets over the rendered
// output so a miss fails a test rather than reaching git.
var secretMarkers = []string{"secret", "password", "key", "token", "credential", "passwd", "auth"}

func isSecretField(f reflect.StructField, name string) bool {
	if f.Tag.Get("snapshot") == "secret" {
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
func secretState(v reflect.Value) string {
	if v.Kind() == reflect.String {
		if v.String() == "" {
			return "<unset>"
		}
		return "<set: generated or shipped>"
	}
	return "<redacted>"
}
