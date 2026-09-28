#!/usr/bin/env python3
"""anonymise-robotsstatus-fixture.py — derive a committable /robotsStatus fixture.

Sibling of anonymise-plant-pull.py, for the two wire-drift fixtures under
shingo-core/fleet/seerrds/testdata/. Those were /robotsStatus captures and
carried each plant's robot addresses, the fleet server's address in every lock
record, robot and map names, order ids, coordinates, battery and temperature
readings and timestamps. Owner ruling 2026-09-13: no plant data in the repo.

    python scripts/anonymise-robotsstatus-fixture.py --in <capture.json> --out <fixture.json>

THIS SCRIPT CARRIES NO DATA and its input stays outside the repo. Running it on
its own output is a no-op diff (the self-check below proves that on every run),
so a committed fixture can be regenerated from itself.

KEPT VERBATIM — the shape wiredrift_test.go exists to watch:

  * EVERY KEY PATH, at every depth, including inside nested objects and arrays
    (the vendor's alarm-code keys too: they are the vendor's codes, not the
    site's). A vendor field added or removed must still show as a diff.
  * Array lengths, and the JSON kind of every value (string, integer, float,
    bool, null, array, object), so the payload decodes into
    rds.RobotsStatusResponse exactly as the capture did.
  * Booleans and nulls: flags, not facts about a site.
  * Which strings were EMPTY. mapSceneState drops blank disabled-path ids, so
    blank-vs-not is behaviour.
  * Equality between values: every distinct value maps to one synthetic value,
    so "the same address in every lock record" stays the same address.

REPLACED, deterministically (no RNG; order of first appearance):

  * numbers -> 0 (integers stay integers, floats stay floats)
  * IPv4 strings -> TEST-NET-1, 192.0.2.<n> (RFC 5737), never a private range
  * timestamps shaped like RFC 3339 -> 2000-01-01T00:00:00Z
  * 32-hex strings (scene_md5, model_md5) -> md5 of "<key>-<n>"
  * all-digit strings -> "0"
  * disabled-path/point ids "A-B" -> "LM<i>-LM<j>" per map-point token, so the
    "A-B" next to "B-A" pairing survives (the count the scene-state test reads)
  * every other non-empty string -> "<key>-<n>" (e.g. vehicle_id-3)
"""

import argparse
import hashlib
import json
import re
import sys

IPV4 = re.compile(r"^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$")
RFC3339 = re.compile(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d")
HEX32 = re.compile(r"^[0-9a-fA-F]{32}$")
DIGITS = re.compile(r"^\d+$")
RFC1918 = re.compile(
    r"(^|[^0-9.])(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}"
    r"|192\.168\.\d{1,3}\.\d{1,3})([^0-9]|$)")
ID_LISTS = ("disable_paths", "disable_points")


class Scrubber:
    def __init__(self):
        self.ips = {}
        self.tokens = {}
        self.strings = {}

    def ip(self, v):
        if v not in self.ips:
            self.ips[v] = "192.0.2.%d" % (len(self.ips) + 1)
        return self.ips[v]

    def path_id(self, v):
        parts = []
        for tok in v.split("-"):
            if tok not in self.tokens:
                self.tokens[tok] = "LM%d" % (len(self.tokens) + 1)
            parts.append(self.tokens[tok])
        return "-".join(parts)

    def string(self, key, v):
        if v == "":
            return ""
        if IPV4.match(v):
            return self.ip(v)
        if RFC3339.match(v):
            return "2000-01-01T00:00:00Z"
        if DIGITS.match(v):
            return "0"
        seen = self.strings.setdefault(key, {})
        if v not in seen:
            seen[v] = "%s-%d" % (key, len(seen) + 1)
        label = seen[v]
        if HEX32.match(v):
            return hashlib.md5(label.encode()).hexdigest()
        return label

    def value(self, key, v, in_ids=False):
        if v is None or isinstance(v, bool):
            return v
        if isinstance(v, int):
            return 0
        if isinstance(v, float):
            return 0.0
        if isinstance(v, str):
            if in_ids and key == "id" and v:
                return self.path_id(v)
            return self.string(key, v)
        if isinstance(v, list):
            return [self.value(key, x, in_ids) for x in v]
        if isinstance(v, dict):
            return {k: self.value(k, x, in_ids or k in ID_LISTS) for k, x in v.items()}
        raise TypeError(type(v))


def keypaths(v, p="", out=None):
    out = set() if out is None else out
    if isinstance(v, dict):
        for k, x in v.items():
            out.add(p + "." + k)
            keypaths(x, p + "." + k, out)
    elif isinstance(v, list):
        for x in v:
            keypaths(x, p + "[]", out)
    return out


def render(doc):
    return json.dumps(Scrubber().value("", doc), indent=1, sort_keys=True) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="inp", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    with open(a.inp, encoding="utf-8") as f:
        src = json.load(f)
    text = render(src)
    out = json.loads(text)

    # Self-checks: the key paths are exactly the capture's, nothing private
    # survived, and the derivation is a fixed point.
    if keypaths(out) != keypaths(src):
        sys.exit("self-check: key paths changed")
    if RFC1918.search(text):
        sys.exit("self-check: a private IPv4 address survived")
    if render(out) != text:
        sys.exit("self-check: derivation is not idempotent")

    with open(a.out, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


if __name__ == "__main__":
    main()
