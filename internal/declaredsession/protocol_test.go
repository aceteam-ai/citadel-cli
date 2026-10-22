package declaredsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type memoryAddress struct{}

func (memoryAddress) Network() string { return "memory" }
func (memoryAddress) String() string  { return "memory" }

type memoryConnection struct {
	input  *bytes.Reader
	output bytes.Buffer
}

func (m *memoryConnection) Read(p []byte) (int, error) {
	return m.input.Read(p)
}
func (m *memoryConnection) Write(p []byte) (int, error) {
	return m.output.Write(p)
}
func (*memoryConnection) Close() error { return nil }
func (*memoryConnection) LocalAddr() net.Addr { return memoryAddress{} }
func (*memoryConnection) RemoteAddr() net.Addr { return memoryAddress{} }
func (*memoryConnection) SetDeadline(time.Time) error { return nil }
func (*memoryConnection) SetReadDeadline(time.Time) error { return nil }
func (*memoryConnection) SetWriteDeadline(time.Time) error { return nil }

func TestFramingRejectsAmbiguitySizeVersionAndTruncation(t *testing.T) {
	bad := []string{
		`{"version":1,"op":"status","op":"stop"}`,
		`{"version":1,"op":"status","unknown":"private"}`,
		`{"version":2,"op":"status"}`,
		`{"version":1,"op":"status"} {}`,
		`{"version":1,"op":"input","data":"not*base64"}`,
		`{"version":1,"op":"status","hello":{"name":"a","name":"b"}}`,
		`{"version":1,"op":"status","state":` + strings.Repeat("[", 18) + `0` + strings.Repeat("]", 18) + `}`,
		"{\"version\":1,\"op\":\"status\",\"reason\":\"\xff\"}",
	}
	for _, raw := range bad {
		if _, err := decodeFrame([]byte(raw)); err == nil {
			t.Fatal("ambiguous/unsupported frame accepted")
		}
	}
	for _, size := range []uint32{0, maxFrameBytes + 1, 10} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		conn := &memoryConnection{input: bytes.NewReader(header[:])}
		if _, err := (&framedConn{conn: conn}).read(context.Background(), operationWait); err == nil {
			t.Fatal("invalid length/truncated frame accepted")
		}
	}
}

func TestFrameRoundTripUsesBoundedTypedBinarySafeOutput(t *testing.T) {
	conn := &memoryConnection{input: bytes.NewReader(nil)}
	original := frame("output")
	original.Data = []byte{0xff, 0, '\x1b', '[', 'm'}
	writer := &framedConn{conn: conn}
	if err := writer.write(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	conn.input = bytes.NewReader(conn.output.Bytes())
	got, err := writer.read(context.Background(), operationWait)
	if err != nil || got.Op != "output" || !bytes.Equal(got.Data, original.Data) {
		t.Fatal("PTY byte chunk was rewritten")
	}
	original.Data = make([]byte, maxFrameBytes)
	if writer.write(context.Background(), original) == nil {
		t.Fatal("encoded frame ceiling ignored")
	}
}

func TestFrameKeysRejectCaseFoldAndUnicodeAliasesAtEveryDepth(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"op":"status","OP":"stop"}`,
		`{"version":1,"op":"attach","readonly":true,"READONLY":false}`,
		`{"version":1,"op":"status","state":{"holder":{"pid":1,"PID":2}}}`,
		`{"version":1,"op":"status","state":{},"ſtate":{"phase":"running"}}`,
		`{"version":1,"op":"attach","take_control":false,"taKe_control":true}`,
		`{"version":1,"op":"status","state":{"holder":{"ſtart_ticks":1}}}`,
	} {
		if _, err := decodeFrame([]byte(raw)); !errors.Is(err, ErrProtocol) {
			t.Fatal("noncanonical field alias accepted")
		}
	}
	// Canonical lowercase underscore keys retain their existing typed meaning.
	attach, err := decodeFrame([]byte(`{"version":1,"op":"attach","incarnation":"0123456789abcdef0123456789abcdef","readonly":false,"take_control":true,"cols":80,"rows":24}`))
	if err != nil || !validClientFrame(attach, false) || !attach.TakeControl {
		t.Fatal("canonical attach control fields rejected")
	}
	status, err := decodeFrame([]byte(`{"version":1,"op":"status","state":{"holder":{"pid":7,"start_ticks":9,"uid":1000,"boot_id":"boot"}}}`))
	if err != nil || status.State == nil || status.State.Holder.PID != 7 || status.State.Holder.StartTicks != 9 {
		t.Fatal("canonical nested state fields rejected")
	}
}

func TestCleanViewerEOFIsDistinctFromTruncatedHeader(t *testing.T) {
	for _, test := range []struct {
		input []byte
		want  error
	}{
		{nil, io.EOF},
		{[]byte{0, 0}, ErrProtocol},
	} {
		conn := &memoryConnection{input: bytes.NewReader(test.input)}
		_, err := (&framedConn{conn: conn}).read(context.Background(), operationWait)
		if !errors.Is(err, test.want) {
			t.Fatalf("EOF classification: %v", err)
		}
	}
}

func TestRequestedReadonlyCannotBeUpgradedByUnexpectedRoleMessage(t *testing.T) {
	conn := &memoryConnection{input: bytes.NewReader(nil)}
	f := &framedConn{conn: conn}
	if f.write(context.Background(), frame("role")) != nil {
		t.Fatal("fixture frame unavailable")
	}
	conn.input = bytes.NewReader(append([]byte(nil), conn.output.Bytes()...))
	conn.output.Reset()
	a := &Attachment{client: &Client{framed: f}, ceiling: true, readOnly: true}
	event, err := a.Next(context.Background())
	if err != nil || !event.ReadOnly || !a.ReadOnly() {
		t.Fatal("requested readonly ceiling was upgraded")
	}
	if !errors.Is(a.Input(context.Background(), []byte("secret")), ErrRole) || !errors.Is(a.Resize(context.Background(), 100, 40), ErrRole) || !errors.Is(a.TakeControl(context.Background()), ErrRole) || conn.output.Len() != 0 {
		t.Fatal("requested readonly emitted terminal/control traffic")
	}
}

func TestClientOperationFieldsAreClosedAndLifecycleIdentityRequired(t *testing.T) {
	status := frame("status")
	if !validClientFrame(status, false) || validClientFrame(status, true) {
		t.Fatal("status on attachment permitted")
	}
	stop := frame("stop")
	if validClientFrame(stop, false) {
		t.Fatal("unbound lifecycle request accepted")
	}
	stop.Incarnation = strings.Repeat("a", 32)
	if !validClientFrame(stop, false) {
		t.Fatal("bound lifecycle request rejected")
	}
	for _, mutate := range []func(*wireFrame){
		func(f *wireFrame) { f.Data = []byte("private") },
		func(f *wireFrame) { f.State = &State{} },
		func(f *wireFrame) { f.Hello = &Hello{} },
		func(f *wireFrame) { f.Reason = "private" },
		func(f *wireFrame) { f.ReadOnly = true },
		func(f *wireFrame) { f.Sequence = 1 },
	} {
		candidate := stop
		mutate(&candidate)
		if validClientFrame(candidate, false) {
			t.Fatal("unexpected lifecycle fields accepted")
		}
	}
	input := frame("input")
	input.Data = make([]byte, maxInputBytes+1)
	if validClientFrame(input, true) {
		t.Fatal("oversized input accepted")
	}
	resize := frame("resize")
	resize.Cols, resize.Rows = 1001, 24
	if validClientFrame(resize, true) {
		t.Fatal("unbounded dimensions accepted")
	}
}

func TestHelloRequiresExactSavedIncarnationPeerBirthAndScope(t *testing.T) {
	peer := ProcessIdentity{PID: 42, UID: 1000, StartTicks: 33, BootID: "boot"}
	saved := State{Name: "test", ProtocolVersion: ProtocolVersion, BinaryVersion: "version", Incarnation: strings.Repeat("a", 32), Holder: peer, Scope: ScopeIdentity{Name: "scope", InvocationID: "invocation"}, DeclarationRevision: "revision"}
	hello := Hello{Name: saved.Name, ProtocolVersion: saved.ProtocolVersion, BinaryVersion: saved.BinaryVersion, Incarnation: saved.Incarnation, Holder: saved.Holder, Scope: saved.Scope, DeclarationRevision: saved.DeclarationRevision}
	if !validHello(hello, saved, "test", peer) {
		t.Fatal("matching local peer rejected")
	}
	for _, mutate := range []func(*Hello){
		func(h *Hello) { h.Incarnation = strings.Repeat("b", 32) },
		func(h *Hello) { h.Holder.StartTicks++ },
		func(h *Hello) { h.Holder.UID = 0 },
		func(h *Hello) { h.Holder.PID++ },
		func(h *Hello) { h.Holder.BootID = "other" },
		func(h *Hello) { h.Scope.InvocationID = "other" },
		func(h *Hello) { h.Name = "other" },
		func(h *Hello) { h.ProtocolVersion++ },
	} {
		candidate := hello
		mutate(&candidate)
		if validHello(candidate, saved, "test", peer) {
			t.Fatal("stale/replaced identity accepted")
		}
	}
}

func TestProtocolErrorsNeverIncludeRawInputOrPrivateLoaderText(t *testing.T) {
	secret := "credential=never-render-this"
	_, err := decodeFrame([]byte(secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("protocol reflected untrusted bytes")
	}
	for _, err := range []error{ErrProtocol, ErrPeer, ErrRole, errChildIO} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("diagnostic leaked private input")
		}
	}
	// Raw terminal output has a separate intentional owner-only field; it
	// cannot accidentally become an event/free-form diagnostic payload.
	raw, err := json.Marshal(Event{Kind: "attach", Reason: "protocol_error"})
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatal("event projection contained console bytes")
	}
}

var _ io.ReadWriteCloser = (*memoryConnection)(nil)
