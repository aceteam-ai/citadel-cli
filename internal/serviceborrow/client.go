package serviceborrow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// maxAdvertisementBytes bounds the advertisement body a peer may return.
const maxAdvertisementBytes = 64 << 10

// maxBorrowResponseBytes bounds the response body a borrowed service may return
// to the requester (a peer cannot exhaust the requester's memory).
const maxBorrowResponseBytes = 64 << 20

// PeerLister returns candidate same-owner peer mesh IPs for `auto` selection
// (production: derived from network.GetGlobalPeers). Returning an error aborts
// auto-select; returning an empty list means "no candidates".
type PeerLister func(ctx context.Context) ([]string, error)

// Client is the requester side of the borrow contract. It is pure over an
// injected MeshGateway (the verified mesh transport) and an optional PeerLister
// (auto-select), so it is adversarially testable with an httptest-backed fake
// gateway and no live mesh.
type Client struct {
	gw    MeshGateway
	peers PeerLister
}

// NewClient builds a requester client. peers may be nil when only explicit-IP
// borrowing is used (auto-select then returns ErrServiceUnavailable).
func NewClient(gw MeshGateway, peers PeerLister) *Client {
	return &Client{gw: gw, peers: peers}
}

// FetchAdvertisement reads the service identity + capacity a connected peer
// advertises for kind. The body is bounded.
func (c *Client) FetchAdvertisement(ctx context.Context, conn *PeerConn, kind string) (*Advertisement, error) {
	url := conn.BaseURL + IdentityPath(kind)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := conn.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdvertisementBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serviceborrow: peer %s advertisement returned %d", conn.MeshIP, resp.StatusCode)
	}
	var adv Advertisement
	if err := json.Unmarshal(body, &adv); err != nil {
		return nil, fmt.Errorf("serviceborrow: decode advertisement from %s: %w", conn.MeshIP, err)
	}
	return &adv, nil
}

// BorrowRequest is one unit of work to run on a peer's borrowed service.
type BorrowRequest struct {
	// Kind is the service kind ("embedding", "transcription", ...).
	Kind string
	// Subpath is the service path on the peer gateway under /borrow/<kind>, e.g.
	// "/v1/embeddings". It must be one the serving provider allows.
	Subpath string
	// Body / ContentType are the request payload forwarded to the loopback service.
	Body        []byte
	ContentType string
	// JobID is the per-borrow-call unique id (see MintJobID / ValidJobID). It
	// links the serving node's work record + receipt to this requester's own.
	JobID string
}

// BorrowResponse is the result of a borrowed call, plus the attribution the
// requester records: ServingNodeID is the serving receipt's node_id (the
// requester's delegated_node_id); ReceiptID identifies the serving receipt (one
// entry in the requester's receipt_ids). JobID echoes the per-call id.
type BorrowResponse struct {
	Status        int
	Body          []byte
	ContentType   string
	ServingNodeID string
	ReceiptID     string
	JobID         string
}

// Borrow POSTs req to the connected peer's borrow route and returns the response
// plus the serving-side attribution from the response headers. It validates the
// job id locally first (a malformed id would be rejected by the peer anyway).
func (c *Client) Borrow(ctx context.Context, conn *PeerConn, req BorrowRequest) (*BorrowResponse, error) {
	if !ValidJobID(req.JobID) {
		return nil, fmt.Errorf("serviceborrow: invalid borrow job id %q", req.JobID)
	}
	url := conn.BaseURL + RoutePrefix + req.Kind + req.Subpath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	httpReq.Header.Set(HeaderJobID, req.JobID)
	resp, err := conn.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBorrowResponseBytes))
	if err != nil {
		return nil, err
	}
	return &BorrowResponse{
		Status:        resp.StatusCode,
		Body:          body,
		ContentType:   resp.Header.Get("Content-Type"),
		ServingNodeID: resp.Header.Get(HeaderServingNodeID),
		ReceiptID:     resp.Header.Get(HeaderReceiptID),
		JobID:         resp.Header.Get(HeaderJobID),
	}, nil
}

// SelectedPeer is a verified peer that advertised a matching service, chosen by
// auto-select. The caller borrows against Conn.
type SelectedPeer struct {
	Conn          *PeerConn
	Advertisement Advertisement
}

// SelectPeer performs `auto` peer selection for want: it lists candidate
// same-owner peers, connects to each (verify-before-dial, so an unverified or
// foreign candidate is skipped), fetches its advertisement, keeps only the ones
// that are Ready and whose identity Matches want, and picks the best by advertised
// capacity (prefer a device match, then the lowest queue depth). A peer that
// fails to connect or whose identity mismatches is skipped, never fatal; only a
// PeerLister error aborts. Returns ErrServiceUnavailable when no candidate
// qualifies — NEVER a central/hosted fallback.
func (c *Client) SelectPeer(ctx context.Context, want ServiceIdentity) (*SelectedPeer, error) {
	if c.peers == nil {
		return nil, ErrServiceUnavailable
	}
	ips, err := c.peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("serviceborrow: list peers: %w", err)
	}
	var candidates []SelectedPeer
	for _, ip := range ips {
		conn, err := c.gw.Connect(ctx, ip)
		if err != nil {
			continue // unverified / foreign / unreachable: skip, never fatal
		}
		adv, err := c.FetchAdvertisement(ctx, conn, want.Kind)
		if err != nil || adv == nil || !adv.Ready {
			continue
		}
		if adv.Identity.Match(want) != nil {
			continue // identity mismatch: never borrow a wrong model/kind
		}
		candidates = append(candidates, SelectedPeer{Conn: conn, Advertisement: *adv})
	}
	if len(candidates) == 0 {
		return nil, ErrServiceUnavailable
	}
	best := pickByCapacity(candidates, want)
	return &best, nil
}

// pickByCapacity orders matching candidates: a candidate whose advertised device
// matches want.Device (when want.Device is set) ranks ahead of one that does not,
// then lower queue depth wins; mesh IP is the stable tie-break so selection is
// deterministic.
func pickByCapacity(cands []SelectedPeer, want ServiceIdentity) SelectedPeer {
	sort.SliceStable(cands, func(i, j int) bool {
		di := deviceRank(cands[i].Advertisement.Capacity.Device, want.Model)
		dj := deviceRank(cands[j].Advertisement.Capacity.Device, want.Model)
		if di != dj {
			return di > dj // higher rank first
		}
		qi, qj := cands[i].Advertisement.Capacity.QueueDepth, cands[j].Advertisement.Capacity.QueueDepth
		if qi != qj {
			return qi < qj // less loaded first
		}
		return cands[i].Conn.MeshIP < cands[j].Conn.MeshIP
	})
	return cands[0]
}

// deviceRank is a coarse preference: a GPU device outranks a CPU one for
// inference-style borrowing. It is advisory (capacity is never a trust input).
func deviceRank(device, _ string) int {
	switch device {
	case "cuda", "gpu", "rocm":
		return 2
	case "cpu":
		return 1
	default:
		return 0
	}
}
