// Command anonymiserobotsstatus derives a committable /robotsStatus fixture.
//
// Sibling of anonymiseplantpull, for the two wire-drift fixtures under
// shingo-core/fleet/seerrds/testdata/. Those were /robotsStatus captures and
// carried each plant's robot addresses, the fleet server's address in every
// lock record, robot and map names, order ids, coordinates, battery and
// temperature readings and timestamps. Owner ruling 2026-09-13: no plant data
// in the repo. From the repo root:
//
//	go run ./shared/scripts/anonymiserobotsstatus --in <capture.json> --out <fixture.json>
//
// THIS PROGRAM CARRIES NO DATA and its input stays outside the repo. Running it
// on its own output is a no-op diff (the self-check below proves that on every
// run), so a committed fixture can be regenerated from itself.
//
// KEPT VERBATIM — the shape wiredrift_test.go exists to watch:
//
//   - EVERY KEY PATH, at every depth, including inside nested objects and
//     arrays (the vendor's alarm-code keys too: they are the vendor's codes,
//     not the site's). A vendor field added or removed must still show as a
//     diff.
//   - Array lengths, and the JSON kind of every value (string, integer, float,
//     bool, null, array, object), so the payload decodes into
//     rds.RobotsStatusResponse exactly as the capture did.
//   - Booleans and nulls: flags, not facts about a site.
//   - Which strings were EMPTY. mapSceneState drops blank disabled-path ids, so
//     blank-vs-not is behaviour.
//   - Equality between values: every distinct value maps to one synthetic
//     value, so "the same address in every lock record" stays the same address.
//
// REPLACED, deterministically (no RNG; order of first appearance):
//
//   - numbers -> 0 (integers stay integers, floats stay floats)
//   - IPv4 strings -> TEST-NET-1, 192.0.2.<n> (RFC 5737), never a private range
//   - timestamps shaped like RFC 3339 -> 2000-01-01T00:00:00Z
//   - 32-hex strings (scene_md5, model_md5) -> md5 of "<key>-<n>"
//   - all-digit strings -> "0"
//   - disabled-path/point ids "A-B" -> "LM<i>-LM<j>" per map-point token, so
//     the "A-B" next to "B-A" pairing survives (the count the scene-state test
//     reads)
//   - every other non-empty string -> "<key>-<n>" (e.g. vehicle_id-3)
//
// The output is indented one space per level with sorted keys and ASCII-only
// strings (shared/scripts/internal/fixturejson). Exit 0 = written; 1 = a
// self-check or the run failed; 2 = bad arguments.
package main

import (
	"crypto/md5" //nolint:gosec // a label's stand-in for a vendor md5 field, not security
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"shingo/shared/scripts/internal/fixturejson"
)

type value = fixturejson.Value

// A digit is any decimal digit, and an end may sit before one trailing newline
// — the patterns' long-standing meaning, kept so a fixture regenerates
// byte-identical.
var (
	ipv4    = regexp.MustCompile(`^\p{Nd}{1,3}\.\p{Nd}{1,3}\.\p{Nd}{1,3}\.\p{Nd}{1,3}\n?$`)
	rfc3339 = regexp.MustCompile(`^\p{Nd}{4}-\p{Nd}\p{Nd}-\p{Nd}\p{Nd}T\p{Nd}\p{Nd}:\p{Nd}\p{Nd}:\p{Nd}\p{Nd}`)
	hex32   = regexp.MustCompile(`^[0-9a-fA-F]{32}\n?$`)
	digits  = regexp.MustCompile(`^\p{Nd}+\n?$`)
	// Run over the rendered text, which is ASCII.
	rfc1918 = regexp.MustCompile(`(^|[^0-9.])(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}` +
		`|192\.168\.\d{1,3}\.\d{1,3})([^0-9]|$)`)
)

// idLists are the objects whose `id` strings are map-point pairs.
var idLists = map[string]bool{"disable_paths": true, "disable_points": true}

type scrubber struct {
	ips     map[string]string
	tokens  map[string]string
	strings map[string]map[string]string
}

func newScrubber() *scrubber {
	return &scrubber{ips: map[string]string{}, tokens: map[string]string{}, strings: map[string]map[string]string{}}
}

func (s *scrubber) ip(v string) string {
	if _, ok := s.ips[v]; !ok {
		s.ips[v] = fmt.Sprintf("192.0.2.%d", len(s.ips)+1)
	}
	return s.ips[v]
}

func (s *scrubber) pathID(v string) string {
	parts := strings.Split(v, "-")
	for i, tok := range parts {
		if _, ok := s.tokens[tok]; !ok {
			s.tokens[tok] = fmt.Sprintf("LM%d", len(s.tokens)+1)
		}
		parts[i] = s.tokens[tok]
	}
	return strings.Join(parts, "-")
}

func (s *scrubber) str(key, v string) string {
	switch {
	case v == "":
		return ""
	case ipv4.MatchString(v):
		return s.ip(v)
	case rfc3339.MatchString(v):
		return "2000-01-01T00:00:00Z"
	case digits.MatchString(v):
		return "0"
	}
	seen := s.strings[key]
	if seen == nil {
		seen = map[string]string{}
		s.strings[key] = seen
	}
	if _, ok := seen[v]; !ok {
		seen[v] = fmt.Sprintf("%s-%d", key, len(seen)+1)
	}
	label := seen[v]
	if hex32.MatchString(v) {
		sum := md5.Sum([]byte(label)) //nolint:gosec // see the import
		return hex.EncodeToString(sum[:])
	}
	return label
}

func (s *scrubber) value(key string, v *value, inIDs bool) *value {
	switch v.Kind {
	case fixturejson.Int:
		return fixturejson.IntValue(0)
	case fixturejson.Float:
		return fixturejson.Num(0)
	case fixturejson.String:
		if inIDs && key == "id" && v.S != "" {
			return fixturejson.Str(s.pathID(v.S))
		}
		return fixturejson.Str(s.str(key, v.S))
	case fixturejson.Array:
		out := make([]*value, 0, len(v.A))
		for _, x := range v.A {
			out = append(out, s.value(key, x, inIDs))
		}
		return fixturejson.Arr(out...)
	case fixturejson.Object:
		o := fixturejson.NewObj()
		for _, k := range v.O.Keys() {
			x, _ := v.O.Get(k)
			o.Set(k, s.value(k, x, inIDs || idLists[k]))
		}
		return fixturejson.ObjValue(o)
	}
	return v // null and booleans
}

func keypaths(v *value, p string, out map[string]bool) {
	switch v.Kind {
	case fixturejson.Object:
		for _, k := range v.O.Keys() {
			x, _ := v.O.Get(k)
			out[p+"."+k] = true
			keypaths(x, p+"."+k, out)
		}
	case fixturejson.Array:
		for _, x := range v.A {
			keypaths(x, p+"[]", out)
		}
	}
}

func sameKeypaths(a, b *value) bool {
	ka, kb := map[string]bool{}, map[string]bool{}
	keypaths(a, "", ka)
	keypaths(b, "", kb)
	if len(ka) != len(kb) {
		return false
	}
	for k := range ka {
		if !kb[k] {
			return false
		}
	}
	return true
}

func render(doc *value) string {
	return fixturejson.Indented(newScrubber().value("", doc, false), 1, true, true) + "\n"
}

// derive renders src and runs the self-checks: the key paths are exactly the
// capture's, nothing private survived, and the derivation is a fixed point.
func derive(data []byte) (string, error) {
	src, err := fixturejson.Parse(data)
	if err != nil {
		return "", err
	}
	text := render(src)
	out, err := fixturejson.Parse([]byte(text))
	if err != nil {
		return "", err
	}
	if !sameKeypaths(out, src) {
		return "", errors.New("self-check: key paths changed")
	}
	if rfc1918.MatchString(text) {
		return "", errors.New("self-check: a private IPv4 address survived")
	}
	if render(out) != text {
		return "", errors.New("self-check: derivation is not idempotent")
	}
	return text, nil
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("anonymiserobotsstatus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "a /robotsStatus capture, from OUTSIDE the repo")
	out := fs.String("out", "", "the fixture to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(stderr, "usage: anonymiserobotsstatus --in IN --out OUT")
		return 2
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	text, err := derive(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }
