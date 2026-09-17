#!/usr/bin/env python3
"""anonymise-plant-pull.py — derive a committable fixture from a real plant pull.

The fixtures under shared/scenefixtures/ used to be the pulls themselves, which
put customer part numbers, PLC tag names, pull timestamps and the plant's floor
map into the repository. Owner ruling 2026-09-13: no plant data or information
in the repo. The reviewers were also right that a hand-typed fixture "gets the
join right by accident" — a scene small enough to type is one in which both the
`instance_name` join and the `label` join work, and a test on it cannot tell
them apart. So this derives a SYNTHETIC fixture from a real pull, keeping every
property the tests rely on and none of the plant's facts.

THIS SCRIPT CARRIES NO DATA. Its input lives outside the repo — the design
folder in the GitHub root, whose README records the hosts and the method. Its
output is what gets committed.

    python scripts/anonymise-plant-pull.py \
        --in  ../hmi-flow-composer-design-2026-09-02/data/hk-press-setups-2026-09-03.json \
        --out shared/scenefixtures/a-press-setups.json --tag A

DETERMINISTIC, AND THE TRANSFORM IS IN HERE, NOT ON THE COMMAND LINE. Every
rename is a pure function of the source value and the plant tag, and each tag's
rigid motion is a constant in TRANSFORMS below, so the invocation above is the
whole invocation and a regenerated fixture is a no-op diff. The first cut took
--rotate/--mirror/--tx/--ty as flags, which meant the bytes in the repo could
only be reproduced by someone who remembered four numbers nothing recorded.
There is no RNG.

WHAT IS PRESERVED, and why each one matters:

  * Row counts, ids, and every structural relation — style_id, process_id,
    operator_station_id, parent_name, the edge from/to join on instance_name.
  * WHICH scene points carry a `label`. Hopkinsville populates 60 of 383 and
    Springfield 12 of 350, which is the trap the fixtures exist for: a
    node->map join on `label` looks right at one plant and renders the other
    empty. Renaming a label must never create or destroy one.
  * The vendor's class strings verbatim (ActionPoint, LocationMark,
    DegenerateBezier, StraightPath, BezierPath) and the bezier handles. 494 of
    Springfield's 588 segments say DegenerateBezier and only the handles say
    which of them bow.
  * The dirty rows: claims whose style is gone, claims on soft-deleted styles,
    the claim whose process row is gone. A naive join drops them and a join
    that assumes success throws; the backfill must do neither.
  * Relative timestamp ORDER, shifted to a fixed epoch.
  * Node names (PLN_01, SMN_BUF_100, Supermarket Area), the plant tags, and
    every mode/role/state enum. These are shingo's own vocabulary, not the
    plant's — owner ruling, same day.

WHAT IS REPLACED:

  * Part numbers and payload codes, and the style names shaped like them.
  * Process, station and group names and descriptions.
  * PLC names and tag names, and expected CATIDs.
  * Every timestamp, and the plant's own `pulled_at`.
  * THE FLOOR PLAN, by one affine transform per plant.

WHAT IS DROPPED, rather than rewritten:

  * `scene_points.properties_json`. It is the vendor's per-point blob and it
    carries the plant three ways at once: `bindRobotMap` names the site's own
    .smap file on all 733 points, a `binTask` names its carrier recfile, and a
    LocationMark's `points` key holds the corner polygon in RAW, UNTRANSFORMED
    coordinates — which would have handed back the floor plan the transform
    above exists to take away. Nothing in the tree reads the column (the
    trimmed pulls this replaces did not even carry it), so it goes rather than
    being rewritten key by key. The self-check below now looks inside it, so
    re-adding the column fails the run instead of shipping.

THE TRANSFORM IS A RIGID MOTION — mirror, then rotate, then translate; scale 1.
See TRANSFORMS for why the rotation is zero.
That is deliberate and it is the one choice here worth arguing about. An affine
with a scale would satisfy "straight stays straight and a DegenerateBezier
keeps bowing the same way" just as well, but it would change every DISTANCE,
and distances are load-bearing: operator-flow.test.js pins PLN_01 to PLN_02 at
1.772 m, the picture draws positions at true spacing, and the key-route walk
sums real edge lengths. A rigid motion keeps all of that exactly while making
the map not the map. Headings (`dir`) rotate with it, and mirroring negates
them, so a point still faces the way its geometry says.
"""

import argparse
import hashlib
import json
import math
import sys
from collections import OrderedDict

# ── one rigid motion per plant tag ───────────────────────────────────────────
#
# (rotate degrees, mirror, translate x, translate y). Scale is 1 by
# construction — see the module docstring for why distances may not move.
# These are the numbers the committed fixtures were derived with; changing one
# rewrites that plant's whole map, which is a fixture re-pin, not a tweak.
#
# THE MOTION MAY NOT TOUCH y, and that is not an aesthetic choice. The station
# picture places cards from the plant's own coordinates and then clusters them
# into a FRONT row and a BACK row on screen y (operator-flow.js rowsOf, through
# an unrotated projector), so the map's ORIENTATION is load-bearing where its
# position is not. Both alternatives were tried and both broke a rule the
# fixture exists to hold:
#
#   * 37 degrees: a press's two rows come out diagonal, no legible scale keeps
#     the cards apart, and the picture drops to "positions not to scale" — the
#     fixture silently no longer exercising the placed path.
#   * 180 degrees (or any y flip): the rows survive and SWAP. PLN_01 is Kind
#     `front` and is DRAWN in the BACK row at this plant, which is F2's whole
#     point; upside down it draws in the front row and the shot harness fails
#     on the caption.
#   * 90 degrees turns the rows into columns.
#
# So: mirror in x, translate. A rigid motion cannot change the shape anyway —
# that is what preserving distances MEANS — so what the transform can take away
# is the absolute position and the handedness, and it takes both.
TRANSFORMS = {
    "A": (0.0, True, 120.0, -45.0),
    "B": (0.0, True, -60.0, 210.0),
}


def _h(tag, kind, value, mod):
    """A stable small integer for (tag, kind, value)."""
    d = hashlib.sha256(("%s\x00%s\x00%s" % (tag, kind, value)).encode("utf-8")).digest()
    return int.from_bytes(d[:8], "big") % mod


def make_part_code(tag, kind, n):
    """An invented part number: SYN-<tag>-<kind><n>, e.g. SYN-A-P007.

    NOT SHAPED LIKE A REAL PART NUMBER, and it used to be. This returned the
    plants' own format — a five-digit block, a dash, three letters, two digits,
    a dot, two digits — on the reasoning that a fixture should exercise the
    shapes the plants use. Two things were wrong with that. The lesser one is
    that nothing downstream cares: the only code that inspects a part string is
    composer-model's shortPart, which strips a `PIA<n>`/`Payload` suffix and is
    a no-op on every other shape. The larger one is that it defeated the point
    of the exercise. A committed fixture, a test literal and a handoff shot all
    then read like a customer's part number, so nobody looking at one could tell
    by looking whether it was invented, and the anonymiser's own leak scan had
    to special-case the collisions it manufactured. A synthetic value that is
    indistinguishable from the real thing is not anonymised, it is unlabelled.

    SEQUENTIAL, NOT HASHED, for two reasons. Distinctness is guaranteed rather
    than probable — the B fixture has 242 part codes, and 242 draws from a
    four-digit hash collide better than half the time, which would silently
    merge two of the plant's parts into one synthetic one. And the numbers are
    then readable in a diff and in a shot. Stability is per RUN, which is all
    anything needs: the Anonymiser's own dict holds one invented code per real
    one for the whole document, and a new pull replaces the fixture wholesale.
    """
    return "SYN-%s-%s%03d" % (tag, kind, n)


def make_style_name(tag, n):
    """A style name. Real ones are part numbers or a word; both become a code.

    S AND P ARE SEPARATE NUMBER LINES so a style name and a payload code can
    never read as the same thing. A style whose real name is a word an engineer
    typed ("Press", "L Bracket") carries nothing, but it is still the plant's
    word, so it becomes a neutral one of the same kind as the rest.
    """
    return "PART %s" % make_part_code(tag, "S", n)


class Anonymiser:
    def __init__(self, tag, theta_deg, mirror, tx, ty, epoch):
        self.tag = tag
        self.theta = math.radians(theta_deg)
        self.mirror = -1.0 if mirror else 1.0
        self.tx, self.ty = tx, ty
        self.epoch = epoch
        self.parts = {}       # real payload code -> invented
        self.styles = {}      # real style name  -> invented
        self.names = {}       # real process/station/group name -> invented
        self._name_n = 0
        self._times = {}      # real timestamp -> invented, order preserved

    # ── strings ──────────────────────────────────────────────────────────
    def part(self, code):
        if code in (None, ""):
            return code
        # Sentinels and obvious test rows are shingo's, not the plant's.
        if code.startswith("__") or code in ("Test-Payload", "Core_Test"):
            return code
        if code not in self.parts:
            self.parts[code] = make_part_code(self.tag, "P", len(self.parts) + 1)
        return self.parts[code]

    def style(self, name):
        if name in (None, ""):
            return name
        if name not in self.styles:
            self.styles[name] = make_style_name(self.tag, len(self.styles) + 1)
        return self.styles[name]

    def label(self, kind, name):
        """A process / station / group name.

        THE PLANT TAG IS IN THE NAME so the scheme cannot collide with a real
        one. It used to be "Press 1", "Press 2" — and Springfield really does
        run presses called "Press 4" and "Press 6", so two of the invented
        names came out identical to the ones they were replacing. The check at
        the end of this file caught it, which is the reason that check compares
        whole values rather than trusting the rename.
        """
        if name in (None, ""):
            return name
        if name not in self.names:
            self._name_n += 1
            self.names[name] = "%s %s%d" % (kind, self.tag, self._name_n)
        return self.names[name]

    def when(self, ts):
        """A timestamp, shifted to a fixed epoch with its ORDER preserved.

        Order matters — `recent changeovers` sorts on it and the claim
        round-trip compares before and after — and the real dates do not.
        """
        if ts in (None, ""):
            return ts
        if ts not in self._times:
            self._times[ts] = None  # placeholder; filled by finish_times
        return ts  # rewritten in a second pass, once every stamp is known

    def finish_times(self):
        """Assign the shifted stamps, in the real ones' sorted order."""
        for i, real in enumerate(sorted(t for t in self._times if t)):
            # One minute apart from the epoch, keeping the order and nothing else.
            self._times[real] = "%s%02d:%02d:00Z" % (self.epoch, (i // 60) % 24, i % 60)

    def rewrite_time(self, ts):
        if ts in (None, ""):
            return ts
        return self._times.get(ts, ts)

    # ── geometry ─────────────────────────────────────────────────────────
    def xy(self, x, y):
        if x is None or y is None:
            return x, y
        mx = x * self.mirror
        rx = mx * math.cos(self.theta) - y * math.sin(self.theta)
        ry = mx * math.sin(self.theta) + y * math.cos(self.theta)
        return round(rx + self.tx, 3), round(ry + self.ty, 3)

    def heading(self, d):
        """A `dir` is a heading in radians; it turns with the map."""
        if d is None:
            return d
        if self.mirror < 0:
            d = math.pi - d
        return round((d + self.theta) % (2 * math.pi), 6)


def anonymise(src, tag, theta, mirror, tx, ty, epoch):
    a = Anonymiser(tag, theta, mirror, tx, ty, epoch)
    edge, core = src["edge"], src["core"]

    # ── pass 1: collect every timestamp so the order survives the shift ──
    def walk_times(rows, keys):
        for r in rows:
            for k in keys:
                if r.get(k):
                    a.when(r[k])

    # EVERY column whose values are dates, enumerated from both pulls rather
    # than guessed: "last_seen" was in this list and the column is
    # "last_seen_at", so an operator station carried the pull's own clock time
    # ("2026-09-03 02:45:15") straight into the fixture.
    stamp_keys = ("created_at", "updated_at", "deleted_at", "synced_at",
                  "started_at", "completed_at", "below_reorder_since",
                  "last_seen_at", "archived_at")
    for table in list(edge.values()) + list(core.values()):
        if isinstance(table, list) and table and isinstance(table[0], dict):
            walk_times(table, stamp_keys)
    a.when(src.get("pulled_at"))
    a.finish_times()

    def rows(table):
        """Copy rows, rewriting every timestamp. Column order is preserved so
        the output diffs against the source row by row."""
        out = []
        for r in table:
            o = OrderedDict()
            for k, v in r.items():
                o[k] = a.rewrite_time(v) if k in stamp_keys else v
            out.append(o)
        return out

    processes = rows(edge["processes"])
    for p in processes:
        p["name"] = a.label("Press", p.get("name"))
        if p.get("description"):
            p["description"] = p["name"]
        # A PLC struct path and its tag are the plant's naming, not shingo's.
        if p.get("counter_plc_name"):
            p["counter_plc_name"] = "PLC-%s" % p["name"].replace(" ", "-")
        if p.get("counter_tag_name"):
            p["counter_tag_name"] = "MES_%s.Prod_Counter_01" % p["name"].replace(" ", "_")

    stations = rows(edge["operator_stations"])
    for s in stations:
        s["name"] = a.label("Screen", s.get("name"))
        if s.get("code"):
            s["code"] = s["name"].lower().replace(" ", "-")
        # A free-text note an engineer typed at the plant.
        if s.get("note"):
            s["note"] = ""

    styles = rows(edge["styles"])
    for s in styles:
        s["name"] = a.style(s.get("name"))
        if s.get("description"):
            s["description"] = s["name"]
        if s.get("expected_catid"):
            # A CATID is the PLC's number for a style. Keep it numeric and
            # keep it distinct; the value itself is the plant's.
            s["expected_catid"] = str(1000 + _h(tag, "catid", str(s["expected_catid"]), 9000))

    claims = rows(edge["style_node_claims"])
    for c in claims:
        c["payload_code"] = a.part(c.get("payload_code"))
        if isinstance(c.get("allowed_payload_codes"), list):
            c["allowed_payload_codes"] = [a.part(x) for x in c["allowed_payload_codes"]]
        elif isinstance(c.get("allowed_payload_codes"), str) and c["allowed_payload_codes"].startswith("["):
            c["allowed_payload_codes"] = json.dumps([a.part(x) for x in json.loads(c["allowed_payload_codes"])])

    # A CATALOG ROW HAS THREE PLACES TO PUT A PART NUMBER, and the trimmed
    # copy that used to be committed carried only two of them. `description` is
    # the part's ENGINEERING NAME at Springfield — "BRKT-FR FDR LWR LH",
    # "DAMPER-MASS" — which is customer data of a different shape and slipped
    # straight through a code-shaped rename. Everything that is not the code
    # becomes the code.
    catalog = rows(edge["payload_catalog"])
    for e in catalog:
        e["code"] = a.part(e.get("code"))
        for k in ("name", "description"):
            if k in e:
                e[k] = e["code"]
        # The PLC's number for the part, same as a style's expected_catid.
        if e.get("catid") not in (None, ""):
            e["catid"] = str(1000 + _h(tag, "catid", str(e["catid"]), 9000))

    payloads = rows(core["payloads"])
    for p in payloads:
        p["code"] = a.part(p.get("code"))
        for k in ("name", "description"):
            if k in p:
                p[k] = p["code"]

    points = rows(core["scene_points"])
    for p in points:
        p["pos_x"], p["pos_y"] = a.xy(p.get("pos_x"), p.get("pos_y"))
        p["dir"] = a.heading(p.get("dir"))
        # THE LABEL IS A NODE NAME WHERE IT IS POPULATED, and node names stay.
        # Whether it is populated is the fixture's whole point, so the empty
        # string stays the empty string.
        #
        # properties_json GOES ENTIRELY — the vendor blob, three leaks deep:
        # the site's .smap name on every row, a carrier recfile, and a
        # LocationMark's corner polygon in untransformed coordinates. Nothing
        # reads it. See the module docstring.
        p.pop("properties_json", None)

    edges = rows(core["scene_edges"])
    for e in edges:
        e["from_x"], e["from_y"] = a.xy(e.get("from_x"), e.get("from_y"))
        e["to_x"], e["to_y"] = a.xy(e.get("to_x"), e.get("to_y"))
        e["ctrl1_x"], e["ctrl1_y"] = a.xy(e.get("ctrl1_x"), e.get("ctrl1_y"))
        e["ctrl2_x"], e["ctrl2_y"] = a.xy(e.get("ctrl2_x"), e.get("ctrl2_y"))

    out = OrderedDict()
    out["plant"] = tag
    out["note"] = ("Synthetic. Derived from a real plant pull by "
                   "scripts/anonymise-plant-pull.py; the pull is held outside the repo. "
                   "Structure, counts, joins, label sparsity, bezier handles and dirty rows "
                   "are the plant's; part numbers, names, PLC tags, timestamps and the map "
                   "are not.")
    out["processes"] = processes
    out["operator_stations"] = stations
    out["process_nodes"] = rows(edge["process_nodes"])
    out["styles"] = styles
    out["style_node_claims"] = claims
    out["payload_catalog"] = catalog
    out["scene_points"] = points
    out["scene_edges"] = edges
    out["nodes"] = rows(core["nodes"])
    out["payloads"] = payloads
    out["payload_bin_types"] = rows(core["payload_bin_types"])
    return out


def sensitive_strings(src):
    """Every string in the pull that must not survive into the fixture.

    AN OUTPUT CHECK, NOT AN INPUT LIST. The rename above works column by column,
    and the pull has more columns than anyone holds in their head: the first run
    of this script left payload_catalog.description untouched, which at
    Springfield is the part's engineering name ("BRKT-FR FDR LWR LH") — customer
    data of a shape no part-number regex matches, and a column the trimmed copy
    that used to be committed did not even carry. So the script checks its own
    output against its own input, and a column nobody thought of fails the run
    instead of shipping.

    Node names, vendor class strings, enums and ids are deliberately NOT here:
    they are shingo's vocabulary and the vendor's, and they stay by owner ruling.
    """
    out = set()
    edge, core = src["edge"], src["core"]

    def add(rows, *keys):
        for r in rows or ():
            for k in keys:
                v = r.get(k)
                if isinstance(v, str) and len(v) > 3:
                    out.add(v)

    add(edge.get("payload_catalog"), "code", "name", "description")
    add(core.get("payloads"), "code", "name", "description")
    add(edge.get("styles"), "name", "description", "expected_catid")
    add(edge.get("processes"), "name", "description", "counter_plc_name", "counter_tag_name")
    add(edge.get("operator_stations"), "name", "note")
    add(edge.get("style_node_claims"), "payload_code", "auto_request_payload")
    if isinstance(src.get("pulled_at"), str):
        out.add(src["pulled_at"])
    # THE VENDOR BLOB, flattened into the same set. properties_json is dropped
    # above, so nothing here can survive — which is the point: if someone puts
    # the column back, the .smap name and the untransformed corner polygons
    # fail the run instead of shipping. Its `label` key is skipped: that one
    # holds the node name, which stays by owner ruling and is a value the
    # fixture is REQUIRED to keep.
    for p in core.get("scene_points") or ():
        for kv in p.get("properties_json") or ():
            if isinstance(kv, dict) and kv.get("key") not in _VOCABULARY_KEYS:
                v = kv.get("stringValue")
                if isinstance(v, str) and len(v) > 3:
                    out.add(v)
    # Shingo's own sentinels, which are not the plant's to take away.
    out -= {"Test-Payload", "Core_Test"}
    return out


# Fields that keep the plant's word by owner ruling: node names are shingo's
# vocabulary (PLN_01, SMN_BUF_100, Supermarket Area, and the line designators
# that are node names too), and the vendor's class and point identifiers are
# the vendor's. A sensitive string is allowed to appear in one of these.
_VOCABULARY_KEYS = {
    "core_node_name", "paired_core_node", "second_paired_core_node",
    "inbound_source", "inbound_staging", "outbound_staging", "outbound_source",
    "outbound_destination", "changeover_evac_destination", "changeover_evac_nodes",
    "release_node", "staging_node", "inbound_source_node", "outbound_source_node",
    "inbound_source_node_group", "outbound_source_node_group",
    "name", "parent_name", "label", "instance_name", "from_name", "to_name",
    "class_name", "area_name", "point_name", "node_type_code", "code",
}


def leaked_values(out_doc, sensitive):
    """Sensitive source strings that survive as a WHOLE field value.

    Whole values, not substrings. A substring scan reports every invented
    "Press 41" that contains a real "Press 4" and hides the ones that matter
    underneath them. It used to have the same trouble with part codes, which
    were invented in the real ones' shape and so contained real CATIDs by
    coincidence; make_part_code no longer does that, and the numbers it emits
    are sequential, so a digit run inside one carries nothing from the plant.
    """
    hits = {}

    def walk(node, key=None, path=""):
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, k, path + "." + k)
        elif isinstance(node, list):
            for v in node:
                walk(v, key, path + "[]")
        elif isinstance(node, str) and node in sensitive:
            # STRIP THE LIST MARKER. A row's path reads ".scene_points[].label",
            # so the table component is "scene_points[]" and the plain compare
            # this used to do never matched — the whole allowance was dead, and
            # invisible while the sensitive set held no node names.
            table = path.split(".")[1].removesuffix("[]") if "." in path[1:] else ""
            if key in _VOCABULARY_KEYS and table in ("nodes", "process_nodes", "scene_points",
                                                     "scene_edges", "style_node_claims"):
                return
            hits.setdefault(node, set()).add(path)

    walk(out_doc)
    return hits


def write_rows_per_line(f, doc):
    """ONE ROW PER LINE — the encoding the pulls this replaces already used.

    A row is the unit here: a diff between two pulls is rows added, removed and
    changed, and a reviewer reads it that way. json.dump(indent=1) puts every
    FIELD on its own line, which turned 1,238 lines of Hopkinsville into 24,322
    for no information at all — a ~49,000-line swing across the two fixtures,
    all of it whitespace, in the bucket the report counts. Top-level keys get a
    line each so the tables are findable; scalars and the row objects are
    compact.
    """
    f.write("{\n")
    keys = list(doc)
    for i, k in enumerate(keys):
        tail = "" if i == len(keys) - 1 else ","
        v = doc[k]
        if isinstance(v, list) and v and isinstance(v[0], dict):
            f.write("%s: [\n" % json.dumps(k))
            for j, row in enumerate(v):
                f.write("  %s%s\n" % (json.dumps(row, ensure_ascii=False, separators=(",", ":")),
                                      "" if j == len(v) - 1 else ","))
            f.write("]%s\n" % tail)
        else:
            f.write("%s: %s%s\n" % (json.dumps(k), json.dumps(v, ensure_ascii=False), tail))
    f.write("}\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="src", required=True, help="a real pull, from OUTSIDE the repo")
    ap.add_argument("--out", dest="dst", required=True)
    ap.add_argument("--tag", required=True, choices=sorted(TRANSFORMS),
                    help="the synthetic plant tag; its rigid motion is TRANSFORMS[tag]")
    args = ap.parse_args()

    rotate, mirror, tx, ty = TRANSFORMS[args.tag]
    with open(args.src, encoding="utf-8") as f:
        src = json.load(f)
    out = anonymise(src, args.tag, rotate, mirror, tx, ty, "2026-01-05T")

    # THE SELF-CHECK. Fails the run rather than writing a fixture that still
    # carries the plant.
    sensitive = sensitive_strings(src)
    leaked = leaked_values(out, sensitive)
    if leaked:
        print("REFUSING TO WRITE %s — %d source value(s) survived:" % (args.dst, len(leaked)),
              file=sys.stderr)
        for v in sorted(leaked)[:20]:
            print("   %r at %s" % (v, ", ".join(sorted(leaked[v]))), file=sys.stderr)
        if len(leaked) > 20:
            print("   ... and %d more" % (len(leaked) - 20), file=sys.stderr)
        return 1

    with open(args.dst, "w", encoding="utf-8", newline="\n") as f:
        write_rows_per_line(f, out)
    print("%s -> %s  (%d points, %d edges, %d claims, %d styles, %d source strings checked)" % (
        args.src, args.dst, len(out["scene_points"]), len(out["scene_edges"]),
        len(out["style_node_claims"]), len(out["styles"]), len(sensitive)),
        file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
