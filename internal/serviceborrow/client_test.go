package serviceborrow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePeer is an httptest-backed stand-in for a peer gateway's borrow routes.
type fakePeer struct {
	srv       *httptest.Server
	conn      *PeerConn
	adv       Advertisement
	servingID string
	receiptID string
}

// newFakePeer serves GET /borrow/<kind>/identity (adv) and POST
// /borrow/<kind>/<sub> (echo body + serving headers).
func newFakePeer(t *testing.T, meshIP string, adv Advertisement, servingID, receiptID string) *fakePeer {
	t.Helper()
	fp := &fakePeer{adv: adv, servingID: servingID, receiptID: receiptID}
	mux := http.NewServeMux()
	mux.HandleFunc(RoutePrefix, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/identity") {
			_ = json.NewEncoder(w).Encode(fp.adv)
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set(HeaderServingNodeID, fp.servingID)
			w.Header().Set(HeaderReceiptID, fp.receiptID)
			w.Header().Set(HeaderJobID, r.Header.Get(HeaderJobID))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	fp.srv = httptest.NewServer(mux)
	t.Cleanup(fp.srv.Close)
	fp.conn = &PeerConn{HTTP: fp.srv.Client(), BaseURL: fp.srv.URL, MeshIP: meshIP, StableID: "s-" + meshIP, OwnerID: "o"}
	return fp
}

// fakeGateway maps mesh IPs to fake peer connections, or to connect errors
// (foreign/unreachable peers that verify-before-dial would refuse).
type fakeGateway struct {
	conns map[string]*PeerConn
	errs  map[string]error
}

func (f *fakeGateway) Connect(_ context.Context, meshIP string) (*PeerConn, error) {
	if e := f.errs[meshIP]; e != nil {
		return nil, e
	}
	if c := f.conns[meshIP]; c != nil {
		return c, nil
	}
	return nil, errors.New("no such peer")
}

func embeddingAdv(model, device string, queue int, ready bool) Advertisement {
	return Advertisement{
		Identity: ServiceIdentity{Kind: "embedding", Model: model},
		Capacity: Capacity{Device: device, QueueDepth: queue},
		Ready:    ready,
	}
}

func TestClient_FetchAdvertisement(t *testing.T) {
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cuda", 0, true), "node-A", "r1")
	c := NewClient(nil, nil)
	adv, err := c.FetchAdvertisement(context.Background(), fp.conn, "embedding")
	if err != nil {
		t.Fatalf("FetchAdvertisement: %v", err)
	}
	if adv.Identity.Model != "gte" || adv.Capacity.Device != "cuda" || !adv.Ready {
		t.Fatalf("advertisement = %+v", adv)
	}
}

func TestClient_Borrow_ReadsServingHeaders(t *testing.T) {
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cuda", 0, true), "node-A", "receipt-xyz")
	c := NewClient(nil, nil)
	jobID := MintJobID("parent-1", 3)
	resp, err := c.Borrow(context.Background(), fp.conn, BorrowRequest{
		Kind:        "embedding",
		Subpath:     "/v1/embeddings",
		Body:        []byte(`{"input":["hi"]}`),
		ContentType: "application/json",
		JobID:       jobID,
	})
	if err != nil {
		t.Fatalf("Borrow: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if resp.ServingNodeID != "node-A" || resp.ReceiptID != "receipt-xyz" || resp.JobID != jobID {
		t.Fatalf("serving attribution = (%q,%q,%q), want (node-A, receipt-xyz, %q)", resp.ServingNodeID, resp.ReceiptID, resp.JobID, jobID)
	}
	if string(resp.Body) != `{"input":["hi"]}` {
		t.Fatalf("body round-trip = %q", resp.Body)
	}
}

func TestClient_Borrow_InvalidJobID_Refused(t *testing.T) {
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cuda", 0, true), "node-A", "r1")
	c := NewClient(nil, nil)
	_, err := c.Borrow(context.Background(), fp.conn, BorrowRequest{Kind: "embedding", Subpath: "/v1/embeddings", JobID: "bad\nid"})
	if err == nil {
		t.Fatal("a job id with a newline must be refused before dialing")
	}
}

func TestClient_SelectPeer_PicksMatchingReadyPeer(t *testing.T) {
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cuda", 0, true), "node-A", "r1")
	gw := &fakeGateway{conns: map[string]*PeerConn{"100.64.0.9": fp.conn}}
	c := NewClient(gw, func(context.Context) ([]string, error) { return []string{"100.64.0.9"}, nil })
	sel, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding", Model: "gte"})
	if err != nil {
		t.Fatalf("SelectPeer: %v", err)
	}
	if sel.Conn.MeshIP != "100.64.0.9" {
		t.Fatalf("selected %q", sel.Conn.MeshIP)
	}
}

func TestClient_SelectPeer_SkipsIdentityMismatch(t *testing.T) {
	// The only candidate advertises a DIFFERENT model -> skipped -> unavailable.
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("bge-small", "cuda", 0, true), "node-A", "r1")
	gw := &fakeGateway{conns: map[string]*PeerConn{"100.64.0.9": fp.conn}}
	c := NewClient(gw, func(context.Context) ([]string, error) { return []string{"100.64.0.9"}, nil })
	_, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"})
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable (model mismatch must not be borrowed)", err)
	}
}

func TestClient_SelectPeer_SkipsNotReady(t *testing.T) {
	fp := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cuda", 0, false /* not ready */), "node-A", "r1")
	gw := &fakeGateway{conns: map[string]*PeerConn{"100.64.0.9": fp.conn}}
	c := NewClient(gw, func(context.Context) ([]string, error) { return []string{"100.64.0.9"}, nil })
	_, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding", Model: "gte"})
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable (a not-ready peer must be skipped)", err)
	}
}

func TestClient_SelectPeer_SkipsConnectFailure(t *testing.T) {
	// One candidate that fails to Connect (a foreign peer verify-before-dial
	// refuses) is skipped, never fatal; the matching one is still chosen.
	good := newFakePeer(t, "100.64.0.10", embeddingAdv("gte", "cuda", 0, true), "node-B", "r2")
	gw := &fakeGateway{
		conns: map[string]*PeerConn{"100.64.0.10": good.conn},
		errs:  map[string]error{"100.64.0.9": errors.New("not same-owner")},
	}
	c := NewClient(gw, func(context.Context) ([]string, error) { return []string{"100.64.0.9", "100.64.0.10"}, nil })
	sel, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding", Model: "gte"})
	if err != nil {
		t.Fatalf("SelectPeer: %v", err)
	}
	if sel.Conn.MeshIP != "100.64.0.10" {
		t.Fatalf("selected %q, want the reachable same-owner peer", sel.Conn.MeshIP)
	}
}

func TestClient_SelectPeer_PrefersGPUThenLowerQueue(t *testing.T) {
	cpu := newFakePeer(t, "100.64.0.9", embeddingAdv("gte", "cpu", 0, true), "cpu-node", "r1")
	gpuBusy := newFakePeer(t, "100.64.0.10", embeddingAdv("gte", "cuda", 5, true), "gpu-busy", "r2")
	gpuIdle := newFakePeer(t, "100.64.0.11", embeddingAdv("gte", "cuda", 1, true), "gpu-idle", "r3")
	gw := &fakeGateway{conns: map[string]*PeerConn{
		"100.64.0.9":  cpu.conn,
		"100.64.0.10": gpuBusy.conn,
		"100.64.0.11": gpuIdle.conn,
	}}
	c := NewClient(gw, func(context.Context) ([]string, error) {
		return []string{"100.64.0.9", "100.64.0.10", "100.64.0.11"}, nil
	})
	sel, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding", Model: "gte"})
	if err != nil {
		t.Fatalf("SelectPeer: %v", err)
	}
	if sel.Conn.MeshIP != "100.64.0.11" {
		t.Fatalf("selected %q, want the idlest GPU peer 100.64.0.11", sel.Conn.MeshIP)
	}
}

func TestClient_SelectPeer_NoLister_Unavailable(t *testing.T) {
	c := NewClient(&fakeGateway{}, nil)
	if _, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding"}); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable when no peer lister is configured", err)
	}
}

func TestClient_SelectPeer_ListerError_Propagates(t *testing.T) {
	c := NewClient(&fakeGateway{}, func(context.Context) ([]string, error) { return nil, errors.New("mesh down") })
	if _, err := c.SelectPeer(context.Background(), ServiceIdentity{Kind: "embedding"}); err == nil {
		t.Fatal("a peer-lister error must propagate, not be swallowed")
	}
}
