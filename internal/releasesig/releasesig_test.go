package releasesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PrivateKey, []TrustedKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, []TrustedKey{{ID: KeyID(pub), PublicKey: pub}}
}

const sampleChecksums = "" +
	"1111111111111111111111111111111111111111111111111111111111111111  citadel_v2.300.0_linux_amd64.tar.gz\n" +
	"2222222222222222222222222222222222222222222222222222222222222222  citadel_v2.300.0_windows_amd64.zip\n"

func TestCompiledKeyIDsMatchKeyBytes(t *testing.T) {
	if len(trustedKeys) == 0 {
		t.Fatal("compiled trusted key set is empty")
	}
	seen := map[string]bool{}
	for _, k := range trustedKeys {
		if got := KeyID(k.PublicKey); got != k.ID {
			t.Errorf("compiled key id %s does not match its bytes (derived %s)", k.ID, got)
		}
		if seen[k.ID] {
			t.Errorf("duplicate compiled key id %s", k.ID)
		}
		seen[k.ID] = true
	}
	if !seen["acc280898c1b8791"] {
		t.Error("expected the acc280898c1b8791 release key in the compiled set")
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	priv, keys := testKey(t)
	sig, err := Sign(priv, []byte(sampleChecksums))
	if err != nil {
		t.Fatal(err)
	}
	id, err := VerifyWithKeys([]byte(sampleChecksums), sig, keys)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id != keys[0].ID {
		t.Fatalf("key id = %s, want %s", id, keys[0].ID)
	}
	var env Envelope
	if err := json.Unmarshal(sig, &env); err != nil || env.V != 1 || env.KeyID != keys[0].ID {
		t.Fatalf("unexpected envelope %s (%v)", sig, err)
	}
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	priv, keys := testKey(t)
	sig, _ := Sign(priv, []byte(sampleChecksums))
	tampered := strings.Replace(sampleChecksums, "1111", "1112", 1)
	if _, err := VerifyWithKeys([]byte(tampered), sig, keys); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsUnknownKey(t *testing.T) {
	priv, _ := testKey(t)
	_, otherKeys := testKey(t)
	sig, _ := Sign(priv, []byte(sampleChecksums))
	if _, err := VerifyWithKeys([]byte(sampleChecksums), sig, otherKeys); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	// A test key is never trusted by the compiled set.
	if _, err := Verify([]byte(sampleChecksums), sig); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("compiled-set err = %v, want ErrUnknownKey", err)
	}
}

func TestVerifyRejectsKeyIDSwap(t *testing.T) {
	// A signature by key A relabelled with trusted key B's id must not verify.
	privA, _ := testKey(t)
	_, keysB := testKey(t)
	sig, _ := Sign(privA, []byte(sampleChecksums))
	var env Envelope
	_ = json.Unmarshal(sig, &env)
	env.KeyID = keysB[0].ID
	relabelled, _ := json.Marshal(env)
	if _, err := VerifyWithKeys([]byte(sampleChecksums), relabelled, keysB); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifyRejectsMalformedEnvelopes(t *testing.T) {
	priv, keys := testKey(t)
	good, _ := Sign(priv, []byte(sampleChecksums))
	var env Envelope
	_ = json.Unmarshal(good, &env)
	short := base64.StdEncoding.EncodeToString(make([]byte, 10))
	cases := map[string]string{
		"empty":         "",
		"not json":      "not a signature",
		"wrong version": `{"v":2,"key_id":"` + env.KeyID + `","sig":"` + env.Sig + `"}`,
		"unknown field": `{"v":1,"key_id":"` + env.KeyID + `","sig":"` + env.Sig + `","x":1}`,
		"short sig":     `{"v":1,"key_id":"` + env.KeyID + `","sig":"` + short + `"}`,
		"bad key id":    `{"v":1,"key_id":"zz","sig":"` + env.Sig + `"}`,
		"trailing":      string(good) + `{}`,
		"oversize":      strings.Repeat(" ", MaxSignatureFileSize+1),
	}
	for name, in := range cases {
		if _, err := VerifyWithKeys([]byte(sampleChecksums), []byte(in), keys); !errors.Is(err, ErrMalformedSignature) {
			t.Errorf("%s: err = %v, want ErrMalformedSignature", name, err)
		}
	}
}

func TestParseChecksumsBindsTag(t *testing.T) {
	got, err := ParseChecksums([]byte(sampleChecksums), "v2.300.0")
	if err != nil {
		t.Fatal(err)
	}
	if got["citadel_v2.300.0_linux_amd64.tar.gz"] != strings.Repeat("1", 64) {
		t.Fatalf("unexpected digests %v", got)
	}
	// Replay: the same (validly signed) file presented for another tag.
	for _, tag := range []string{"v2.301.0", "v2.300.0-rc1", "v2.30.0"} {
		if _, err := ParseChecksums([]byte(sampleChecksums), tag); err == nil {
			t.Errorf("checksums for v2.300.0 accepted for %s", tag)
		}
	}
}

func TestParseChecksumsRejectsMalformed(t *testing.T) {
	h := strings.Repeat("a", 64)
	cases := map[string]string{
		"no newline":   h + "  citadel_v1.0.0_linux_amd64.tar.gz",
		"blank line":   h + "  citadel_v1.0.0_linux_amd64.tar.gz\n\n",
		"short digest": "abc  citadel_v1.0.0_linux_amd64.tar.gz\n",
		"path":         h + "  dir/citadel_v1.0.0_linux_amd64.tar.gz\n",
		"foreign file": h + "  other_v1.0.0_linux_amd64.tar.gz\n",
		"duplicate":    h + "  citadel_v1.0.0_linux_amd64.tar.gz\n" + h + " *citadel_v1.0.0_linux_amd64.tar.gz\n",
		"crlf":         h + "  citadel_v1.0.0_linux_amd64.tar.gz\r\n",
		"empty":        "",
	}
	for name, in := range cases {
		if _, err := ParseChecksums([]byte(in), "v1.0.0"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseChecksums([]byte(h+"  citadel_v1.0.0_linux_amd64.tar.gz\n"), "../v1.0.0"); err == nil {
		t.Error("invalid tag accepted")
	}
}

func TestValidTag(t *testing.T) {
	for _, ok := range []string{"v1.2.3", "v2.212.0", "v1.0.0-rc.1"} {
		if !ValidTag(ok) {
			t.Errorf("%s rejected", ok)
		}
	}
	for _, bad := range []string{"", "1.2.3", "v1.2", "v1.2.3/../x", "v1.2.3?x", "latest"} {
		if ValidTag(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
}
