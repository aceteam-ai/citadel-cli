package serviceborrow

import (
	"strconv"
	"strings"
)

// Wire contract shared by the requester client and the serving gateway route.
//
// Routes (served on the peer gateway's mesh VPN listener, under one namespace so
// the gateway can always-allow the prefix in categoryForPath and hand sole
// gating to the borrow gate — exactly as /cache/ does):
//
//	GET  /borrow/<kind>/identity  -> Advertisement JSON (gated)
//	POST /borrow/<kind>/<subpath> -> proxied to the loopback service (gated),
//	                                 with a signed serving receipt + work record
const (
	// RoutePrefix is the borrow route namespace.
	RoutePrefix = "/borrow/"

	// HeaderJobID carries the per-borrow-call unique job id from requester to
	// peer. It becomes the serving receipt's JobID AND one entry in the
	// requester's receipt lineage, so the two ledgers reference the same unit of
	// work by id. It MUST be unique per call (see MintJobID): a FILE_INDEX job
	// that makes 100 embedding borrows must send 100 distinct ids, or the
	// serving ledger (INSERT OR IGNORE on job_id) collapses them to one row.
	HeaderJobID = "X-Borrow-Job-Id"
	// HeaderServingNodeID is the serving receipt's node_id, echoed to the
	// requester so its usage record's delegated_node_id == the serving receipt's
	// node_id (joinable with no translation).
	HeaderServingNodeID = "X-Borrow-Serving-Node-Id"
	// HeaderReceiptID is an identifier for the signed serving receipt, echoed so
	// the requester can carry it in its usage record's receipt_ids.
	HeaderReceiptID = "X-Borrow-Receipt-Id"

	// maxJobIDLen bounds the job id. It becomes a signed receipt field, so it
	// must also contain no newline (aep refuses to sign newlines).
	maxJobIDLen = 256
)

// IdentityPath is the advertisement route for a service kind.
func IdentityPath(kind string) string { return RoutePrefix + kind + "/identity" }

// ValidJobID reports whether s is a usable per-borrow-call job id: non-empty,
// bounded, and free of newline/carriage-return and other control characters (it
// is signed into an AEP receipt's JobID field, which refuses newlines). The
// serving side rejects a request whose job id fails this, rather than minting a
// server-side id the requester's ledger can never see.
func ValidJobID(s string) bool {
	if s == "" || len(s) > maxJobIDLen {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// MintJobID derives a per-borrow-call job id from the requester's originating
// job id and a per-call sequence number: "<originating>.<seq>". Lineage stays
// human-readable (the originating id is the prefix) while every call is unique,
// which keeps both ledgers' counts matching. originating is trimmed; a blank
// originating yields "borrow.<seq>" so the id is still valid and unique.
func MintJobID(originating string, seq int) string {
	originating = strings.TrimSpace(originating)
	if originating == "" {
		originating = "borrow"
	}
	return originating + "." + strconv.Itoa(seq)
}
