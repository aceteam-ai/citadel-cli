package cobrowseprofile

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSuspendManifestEncryptionAndBinding(t *testing.T) {
	v, dir := testVault(t)
	h, err := OpenHandle(dir, "suspend-a", testPIN, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	const id = "cb-0123456789abcdef"
	plain := []byte(`{"url":"https://example.invalid/?token=synthetic-private"}`)
	blob, err := h.SealSuspendManifest(id, 7, plain)
	if err != nil || bytes.Contains(blob, []byte("synthetic-private")) || bytes.Equal(blob, plain) {
		t.Fatal("manifest was not encrypted")
	}
	got, err := h.UnsealSuspendManifest(id, 7, blob)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("manifest round trip failed")
	}
	zero(got)
	for _, tc := range []struct {
		id         string
		generation uint64
	}{{"cb-aaaaaaaaaaaaaaaa", 7}, {id, 8}} {
		if got, err := h.UnsealSuspendManifest(tc.id, tc.generation, blob); err == nil || len(got) != 0 {
			t.Fatal("foreign session or generation decrypted a manifest")
		}
	}
	if got, err := h.sess.Unseal(h.context, blob); err == nil || len(got) != 0 {
		t.Fatal("profile archive context decrypted a suspended manifest")
	}
	other := &Handle{sess: h.sess, context: contextPrefix + "suspend-b"}
	if got, err := other.UnsealSuspendManifest(id, 7, blob); err == nil || len(got) != 0 {
		t.Fatal("another profile decrypted a suspended manifest")
	}
	blob[len(blob)-1] ^= 1
	if got, err := h.UnsealSuspendManifest(id, 7, blob); err == nil || len(got) != 0 {
		t.Fatal("tampered manifest decrypted")
	}
	_ = h.Close()
	if got, err := h.SealSuspendManifest(id, 7, plain); err == nil || len(got) != 0 {
		t.Fatal("closed profile retained manifest encryption authority")
	}
	if got, err := h.UnsealSuspendManifest(id, 7, blob); err == nil || len(got) != 0 {
		t.Fatal("closed profile retained manifest decryption authority")
	}
}

type suspendFakeSealer struct {
	calls  int
	output []byte
	err    error
}

func (s *suspendFakeSealer) Seal(string, []byte) ([]byte, error)   { s.calls++; return s.output, s.err }
func (s *suspendFakeSealer) Unseal(string, []byte) ([]byte, error) { s.calls++; return s.output, s.err }
func (*suspendFakeSealer) Lock()                                   {}

func TestSuspendManifestBoundsAndOpaqueFailures(t *testing.T) {
	if maxSuspendManifestBytes != 262144 || maxSuspendSealedBytes != 266240 {
		t.Fatal("manifest bounds drifted")
	}
	const id = "cb-0123456789abcdef"
	s := &suspendFakeSealer{output: []byte("ok")}
	h := &Handle{sess: s, context: contextPrefix + "a"}
	for _, input := range [][]byte{nil, make([]byte, maxSuspendManifestBytes+1)} {
		if _, err := h.SealSuspendManifest(id, 1, input); err == nil {
			t.Fatal("plaintext bound removed")
		}
	}
	for _, input := range [][]byte{nil, make([]byte, maxSuspendSealedBytes+1)} {
		if _, err := h.UnsealSuspendManifest(id, 1, input); err == nil {
			t.Fatal("ciphertext bound removed")
		}
	}
	for _, invalid := range []string{"", "../session", "cb-" + strings.Repeat("a", 62), "cb-not-hex"} {
		if _, err := h.SealSuspendManifest(invalid, 1, []byte("x")); err == nil {
			t.Fatal("unsafe identity accepted")
		}
	}
	if _, err := h.SealSuspendManifest(id, 0, []byte("x")); err == nil {
		t.Fatal("zero generation accepted")
	}
	var missing *Handle
	if _, err := missing.SealSuspendManifest(id, 1, []byte("x")); err == nil {
		t.Fatal("nil handle accepted")
	}
	if s.calls != 0 {
		t.Fatal("invalid input reached crypto")
	}
	if _, err := h.SealSuspendManifest(id, 1, make([]byte, maxSuspendManifestBytes)); err != nil {
		t.Fatal("exact plaintext bound must be admitted")
	}
	if _, err := h.UnsealSuspendManifest(id, 1, make([]byte, maxSuspendSealedBytes)); err != nil {
		t.Fatal("exact ciphertext bound must be admitted")
	}
	s.output = make([]byte, maxSuspendManifestBytes+1)
	if got, err := h.UnsealSuspendManifest(id, 1, []byte("x")); err == nil || len(got) != 0 {
		t.Fatal("plaintext output bound removed")
	}
	for _, seal := range []bool{false, true} {
		for _, output := range [][]byte{nil, make([]byte, maxSuspendSealedBytes+1)} {
			s.output = output
			var got []byte
			var err error
			if seal {
				got, err = h.SealSuspendManifest(id, 1, []byte("x"))
			} else {
				got, err = h.UnsealSuspendManifest(id, 1, []byte("x"))
			}
			if err == nil || len(got) != 0 {
				t.Fatal("unbounded crypto output accepted")
			}
		}
	}
	s.output = []byte("partial-private")
	s.err = errors.New("synthetic-private-token")
	got, err := h.UnsealSuspendManifest(id, 1, []byte("x"))
	if err == nil || strings.Contains(err.Error(), "synthetic-private-token") || len(got) != 0 || !bytes.Equal(s.output, make([]byte, len(s.output))) {
		t.Fatal("unseal failure exposed partial plaintext or crypto details")
	}
}
