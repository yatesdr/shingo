package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// feeds.go — the digests Core and Edge trade on every heartbeat, so that
// slowly-changing data is re-sent only when it differs.
//
// THE RULE, both directions. Every heartbeat names, per feed, the digest of what
// the Edge holds; every ack names Core's current digest of the same feed. The
// side that owns the data, seeing a different digest, sends the data. The side
// that holds a copy, seeing an equal one, marks its copy confirmed. A feed left
// out of the owner's map means "could not read it": nothing is sent and nothing
// is confirmed, because silence is not evidence that a copy is current.
//
// ONE DEFINITION. Every digest is computed here, over the wire value, so the two
// sides cannot disagree about what was hashed. A holder quotes the digest that
// arrived with the data and never recomputes it — except refusals, a table both
// sides write, where the Edge digests its own open set with RefusalsDigest.

// Feed keys, as they appear in EdgeHeartbeat.Feeds and EdgeHeartbeatAck.Feeds.
const (
	// FeedContainment is Core's quality containment state: flags, held bins and
	// what sits at each containment destination. Carrier: SubjectContainmentSnapshot.
	FeedContainment = "containment"
	// FeedRefusals is the open supply-refusal set. Carrier:
	// SubjectSupplyRefusalSnapshot.
	FeedRefusals = "refusals"
	// FeedNodes is one station's node list with its loaders and payload bin
	// types — the three slices NodesDigest covers. Carrier: NodeListResponse.
	FeedNodes = "nodes"
	// FeedScene is the vendor scene's revision (NodeListResponse.SceneRevision).
	// It is the one key whose value is not a Digest: the revision already
	// identifies the rows, and geometry is sent only when it differs.
	FeedScene = "scene"
	// FeedCatalog is the payload catalog. Carrier: CatalogPayloadsResponse.
	FeedCatalog = "catalog"
)

// SceneRevisionNone is the scene revision of a plant with no scene an Edge can
// cache — no points, no edges, or one without the other (the Edge keeps
// geometry only when a response carries both). An Edge that receives it clears
// its geometry and holds this value, so the scene feed converges on "no scene"
// rather than resending forever to an Edge still quoting a deleted map. It is
// not "": that is a read that minted no revision (a partial read), which
// changes nothing on the Edge. A real revision is 64 hex characters, so the two
// can never be confused.
const SceneRevisionNone = "none"

// Digest is the first 16 hex characters of the SHA-256 of v's JSON encoding.
//
// Callers pass the value they send, canonicalised by the feed's own digest
// function below; nothing else hashes feed data. 64 bits is ample for telling
// two versions of one feed apart, and keeps a heartbeat's map small.
func Digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

// The per-feed digest functions digest a CANONICAL COPY of the value: top-level
// rows sorted on their key, times in UTC, nil slices as empty. The value sent is
// the value read, in the order read; the copy only makes the digest independent
// of row order, time zone and nil-versus-empty, none of which is a change in the
// data. Order inside a row (a loader's positions, a style's claims) is part of
// the data — Core stores a claim's index as its sequence — and is kept.

// NodesDigest is the FeedNodes digest: exactly the node list, loaders and payload
// bin types one station's NodeListResponse carries. Geometry and the scene
// revision are outside it; they have FeedScene.
func NodesDigest(nodes []NodeInfo, loaders []LoaderInfo, binTypes []PayloadBinTypeInfo) (string, error) {
	n := append([]NodeInfo{}, nodes...)
	sort.Slice(n, func(i, j int) bool { return n[i].Name < n[j].Name })
	l := append([]LoaderInfo{}, loaders...)
	sort.Slice(l, func(i, j int) bool { return l[i].LoaderKey < l[j].LoaderKey })
	b := append([]PayloadBinTypeInfo{}, binTypes...)
	sort.Slice(b, func(i, j int) bool {
		if b[i].PayloadCode != b[j].PayloadCode {
			return b[i].PayloadCode < b[j].PayloadCode
		}
		return b[i].BinTypeCode < b[j].BinTypeCode
	})
	return Digest(struct {
		Nodes    []NodeInfo           `json:"nodes"`
		Loaders  []LoaderInfo         `json:"loaders"`
		BinTypes []PayloadBinTypeInfo `json:"payload_bin_types"`
	}{n, l, b})
}

// CatalogDigest is the FeedCatalog digest over the payload list.
func CatalogDigest(payloads []CatalogPayloadInfo) (string, error) {
	p := append([]CatalogPayloadInfo{}, payloads...)
	sort.Slice(p, func(i, j int) bool { return p[i].ID < p[j].ID })
	return Digest(p)
}

// ContainmentDigest is the FeedContainment digest over a snapshot's flags, held
// bins and destinations. The snapshot's own Digest field is not part of it.
func ContainmentDigest(s ContainmentSnapshot) (string, error) {
	flags := make([]PayloadContainmentRow, len(s.Flags))
	for i, f := range s.Flags {
		f.ActivatedAt = utcPtr(f.ActivatedAt)
		f.DeactivatedAt = utcPtr(f.DeactivatedAt)
		flags[i] = f
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].PayloadCode < flags[j].PayloadCode })
	held := make([]HeldBinRow, len(s.HeldBins))
	for i, h := range s.HeldBins {
		h.HoldAt = utcPtr(h.HoldAt)
		held[i] = h
	}
	sort.Slice(held, func(i, j int) bool { return held[i].BinID < held[j].BinID })
	dests := make([]ContainmentDestination, len(s.Destinations))
	for i, d := range s.Destinations {
		children := append([]string{}, d.Children...)
		sort.Strings(children)
		bins := append([]ContainmentBin{}, d.Bins...)
		sort.Slice(bins, func(i, j int) bool {
			if bins[i].Node != bins[j].Node {
				return bins[i].Node < bins[j].Node
			}
			return bins[i].BinID < bins[j].BinID
		})
		dests[i] = ContainmentDestination{Node: d.Node, Children: children, Bins: bins}
	}
	sort.Slice(dests, func(i, j int) bool { return dests[i].Node < dests[j].Node })
	return Digest(ContainmentSnapshot{Flags: flags, HeldBins: held, Destinations: dests})
}

// refusalDigestRow is the part of an open refusal both sides agree on. No times:
// the Edge stamps refused_at itself and Core keeps the time it was told, so a
// time would make the two digests differ for the same refusal.
type refusalDigestRow struct {
	LoaderNode   string `json:"loader_node"`
	PayloadCode  string `json:"payload_code"`
	RefusedBy    string `json:"refused_by"`
	AckChoice    string `json:"ack_choice"`
	AckProcessID string `json:"ack_process_id"`
}

// RefusalsDigest is the FeedRefusals digest over an open refusal set, sorted by
// (loader, payload) and projected to the fields both sides hold. Core digests
// its open rows; the Edge digests its own table with the same function.
func RefusalsDigest(open []SupplyRefusalState) (string, error) {
	rows := make([]refusalDigestRow, len(open))
	for i, st := range open {
		rows[i] = refusalDigestRow{
			LoaderNode: st.LoaderNode, PayloadCode: st.PayloadCode, RefusedBy: st.RefusedBy,
			AckChoice: st.AckChoice, AckProcessID: st.AckProcessID,
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LoaderNode != rows[j].LoaderNode {
			return rows[i].LoaderNode < rows[j].LoaderNode
		}
		return rows[i].PayloadCode < rows[j].PayloadCode
	})
	return Digest(rows)
}

// ClaimsDigest is the digest of one process's PlantClaimsReport, without its
// Digest field. The Edge sets PlantClaimsReport.Digest from it; Core stores the
// digest that arrived and quotes it back, and never recomputes it from its
// mirror rows, which are a projection of the report, not the report.
func ClaimsDigest(r PlantClaimsReport) (string, error) {
	styles := make([]PlantClaimsStyle, len(r.Styles))
	for i, st := range r.Styles {
		claims := make([]PlantClaim, len(st.Claims))
		for j, c := range st.Claims {
			c.AllowedPayloadCodes = append([]string{}, c.AllowedPayloadCodes...)
			claims[j] = c
		}
		st.Claims = claims
		styles[i] = st
	}
	sort.Slice(styles, func(i, j int) bool { return styles[i].StyleID < styles[j].StyleID })
	r.Styles = styles
	r.Digest = ""
	return Digest(r)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
