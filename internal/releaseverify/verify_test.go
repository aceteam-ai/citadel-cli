package releaseverify

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// RFC 8032 section 7.1's first public test seed. Never a production credential.
const fixtureSeed = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
const fixturePublic = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"

func fixtureKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed, err := hex.DecodeString(fixtureSeed)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func fixtureManifest() Manifest {
	archive := []byte("offline release fixture\n")
	hash := sha256.Sum256(archive)
	return Manifest{Schema: Schema, Repository: Repository, ReleaseID: 123, Tag: "v0.0.0-test", Commit: strings.Repeat("a", 40), Artifacts: []Artifact{{Name: "citadel_v0.0.0-test_linux_amd64.tar.gz", Size: int64(len(archive)), SHA256: hex.EncodeToString(hash[:])}}}
}

func observed(m Manifest) ObservedRelease {
	return ObservedRelease{m.Repository, m.ReleaseID, m.Tag, m.Commit, true, false}
}

func signed(t *testing.T, m Manifest) ([]byte, []byte, []Root) {
	t.Helper()
	data, err := EncodeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	key := fixtureKey(t)
	signature, err := Sign(data, "fixture-old", key)
	if err != nil {
		t.Fatal(err)
	}
	signatures, err := EncodeSignatures([]Signature{signature})
	if err != nil {
		t.Fatal(err)
	}
	return data, signatures, []Root{{"fixture-old", key.Public().(ed25519.PublicKey)}}
}

func TestOfflineSignedFixture(t *testing.T) {
	m := fixtureManifest()
	data, signatures, roots := signed(t, m)
	for name, want := range map[string][]byte{"manifest.json": data, "signatures.json": signatures, "archive.fixture": []byte("offline release fixture\n")} {
		got, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("fixture %s differs", name)
		}
	}
	if hex.EncodeToString(roots[0].PublicKey) != fixturePublic {
		t.Fatal("RFC fixture key differs")
	}
	verified, err := Verify(data, signatures, observed(m), roots)
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.VerifyArchive(m.Artifacts[0].Name, strings.NewReader("offline release fixture\n")); err != nil {
		t.Fatal(err)
	}
	checksums, err := Checksums(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.VerifyChecksums(checksums); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, append(bytes.Clone(checksums), 'x'), bytes.ReplaceAll(checksums, []byte("linux_amd64"), []byte("linux_arm64")), append(bytes.Clone(checksums), checksums...)} {
		if verified.VerifyChecksums(bad) == nil {
			t.Fatal("unsigned checksum change accepted")
		}
	}
}

func TestReleaseIdentityAndImmutability(t *testing.T) {
	m := fixtureManifest()
	data, signatures, roots := signed(t, m)
	for name, change := range map[string]func(*ObservedRelease){
		"mutable":    func(r *ObservedRelease) { r.Immutable = false },
		"draft":      func(r *ObservedRelease) { r.Draft = true },
		"repository": func(r *ObservedRelease) { r.Repository = "foreign/repo" },
		"release-id": func(r *ObservedRelease) { r.ReleaseID++ },
		"tag":        func(r *ObservedRelease) { r.Tag = "v0.0.1" },
		"commit":     func(r *ObservedRelease) { r.Commit = strings.Repeat("b", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			r := observed(m)
			change(&r)
			if _, err := Verify(data, signatures, r, roots); err == nil {
				t.Fatal("mismatched release accepted")
			}
		})
	}
}

func TestRejectsUntrustedAndAlteredSignatures(t *testing.T) {
	m := fixtureManifest()
	data, signatures, roots := signed(t, m)
	badData := bytes.Replace(data, []byte(strings.Repeat("a", 40)), []byte(strings.Repeat("b", 40)), 1)
	other := m
	other.Commit = strings.Repeat("b", 40)
	if _, err := Verify(badData, signatures, observed(other), roots); err == nil {
		t.Fatal("tampered signed bytes accepted")
	}
	if _, err := Verify(data, signatures, observed(m), nil); err == nil {
		t.Fatal("empty roots accepted")
	}
	wrongKey := bytes.Clone(roots[0].PublicKey)
	wrongKey[0] ^= 1
	for name, trust := range map[string][]Root{
		"wrong-key":      {{"fixture-old", wrongKey}},
		"wrong-id":       {{"different", roots[0].PublicKey}},
		"duplicate-root": {roots[0], roots[0]},
		"short-root":     {{"fixture-old", []byte{1}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(data, signatures, observed(m), trust); err == nil {
				t.Fatal("invalid trust accepted")
			}
		})
	}
	var set signatureSet
	if err := json.Unmarshal(signatures, &set); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(set.Signatures[0].Value)
	raw[0] ^= 1
	set.Signatures[0].Value = base64.StdEncoding.EncodeToString(raw)
	bad, _ := EncodeSignatures(set.Signatures)
	if _, err := Verify(data, bad, observed(m), roots); err == nil {
		t.Fatal("altered signature accepted")
	}
	// A mathematically valid signature without the protocol domain still fails.
	raw = ed25519.Sign(fixtureKey(t), data)
	bad, _ = EncodeSignatures([]Signature{{"fixture-old", base64.StdEncoding.EncodeToString(raw)}})
	if _, err := Verify(data, bad, observed(m), roots); err == nil {
		t.Fatal("signature from another domain accepted")
	}
}

func TestRotationOverlapAndRetirement(t *testing.T) {
	m := fixtureManifest()
	data, _, oldRoots := signed(t, m)
	// A second deterministic fixture only; no production key is created or stored.
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	newer := ed25519.NewKeyFromSeed(seed)
	nextRoot := Root{"fixture-next", newer.Public().(ed25519.PublicKey)}
	oldProof, err := Sign(data, oldRoots[0].ID, fixtureKey(t))
	if err != nil {
		t.Fatal(err)
	}
	nextProof, err := Sign(data, nextRoot.ID, newer)
	if err != nil {
		t.Fatal(err)
	}
	dual, err := EncodeSignatures([]Signature{oldProof, nextProof})
	if err != nil {
		t.Fatal(err)
	}
	for _, roots := range [][]Root{oldRoots, append(append([]Root{}, oldRoots...), nextRoot), {nextRoot}} {
		if _, err := Verify(data, dual, observed(m), roots); err != nil {
			t.Fatal(err)
		}
	}
	oldOnly, _ := EncodeSignatures([]Signature{oldProof})
	if _, err := Verify(data, oldOnly, observed(m), []Root{nextRoot}); err == nil {
		t.Fatal("retired key still authorizes")
	}
	nextOnly, _ := EncodeSignatures([]Signature{nextProof})
	if _, err := Verify(data, nextOnly, observed(m), oldRoots); err == nil {
		t.Fatal("unintroduced next key authorizes")
	}
}

func TestCanonicalMetadataAndBounds(t *testing.T) {
	m := fixtureManifest()
	data, signatures, roots := signed(t, m)
	for name, bad := range map[string][]byte{
		"oversize":        bytes.Repeat([]byte{'x'}, MaxManifestBytes+1),
		"duplicate-field": bytes.Replace(data, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1),
		"unknown-field":   bytes.Replace(data, []byte(`"schema":1`), []byte(`"unknown":1,"schema":1`), 1),
		"trailing":        append(bytes.Clone(data), ' '),
		"second-value":    append(bytes.Clone(data), []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(bad, signatures, observed(m), roots); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*Manifest){
		"schema":            func(m *Manifest) { m.Schema++ },
		"repo":              func(m *Manifest) { m.Repository = "other/repo" },
		"release":           func(m *Manifest) { m.ReleaseID = 0 },
		"path-tag":          func(m *Manifest) { m.Tag = "../../release" },
		"commit":            func(m *Manifest) { m.Commit = "main" },
		"empty":             func(m *Manifest) { m.Artifacts = nil },
		"count":             func(m *Manifest) { m.Artifacts = make([]Artifact, MaxArtifacts+1) },
		"duplicate":         func(m *Manifest) { m.Artifacts = append(m.Artifacts, m.Artifacts[0]) },
		"unsigned-platform": func(m *Manifest) { m.Artifacts[0].Name = "citadel_v0.0.0-test_linux_amd64.tar.gz.evil" },
		"zero-size":         func(m *Manifest) { m.Artifacts[0].Size = 0 },
		"oversize-archive":  func(m *Manifest) { m.Artifacts[0].Size = MaxArchiveBytes + 1 },
		"digest":            func(m *Manifest) { m.Artifacts[0].SHA256 = strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := m
			copy.Artifacts = append([]Artifact{}, m.Artifacts...)
			mutate(&copy)
			if _, err := EncodeManifest(copy); err == nil {
				t.Fatal("invalid signing manifest accepted")
			}
		})
	}
	for name, bad := range map[string][]byte{
		"large-signatures":          bytes.Repeat([]byte{'x'}, MaxSignatureBytes+1),
		"empty-signatures":          []byte(`{"schema":1,"signatures":[]}`),
		"bad-schema":                bytes.Replace(signatures, []byte(`"schema":1`), []byte(`"schema":2`), 1),
		"duplicate-signature-field": bytes.Replace(signatures, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(data, bad, observed(m), roots); err == nil {
				t.Fatal("invalid proofs accepted")
			}
		})
	}
	validProof, _ := Sign(data, "fixture-old", fixtureKey(t))
	if _, err := EncodeSignatures([]Signature{validProof, validProof}); err == nil {
		t.Fatal("duplicate proofs accepted")
	}
	if _, err := EncodeSignatures(make([]Signature, MaxSignatures+1)); err == nil {
		t.Fatal("too many proofs accepted")
	}
	for _, bad := range []Signature{{"bad/id", validProof.Value}, {"fixture-old", "not-base64"}, {"fixture-old", base64.StdEncoding.EncodeToString([]byte{1})}} {
		if _, err := EncodeSignatures([]Signature{bad}); err == nil {
			t.Fatal("malformed proof accepted")
		}
	}
}

type endlessReader struct{ consumed int64 }

func (r *endlessReader) Read(p []byte) (int, error) {
	clear(p)
	r.consumed += int64(len(p))
	return len(p), nil
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("fixture failure") }

func TestArchiveIntegrityAndReadBound(t *testing.T) {
	m := fixtureManifest()
	data, signatures, roots := signed(t, m)
	verified, err := Verify(data, signatures, observed(m), roots)
	if err != nil {
		t.Fatal(err)
	}
	for name, reader := range map[string]io.Reader{
		"same-length-tamper": strings.NewReader("OFFLINE release fixture\n"),
		"short":              strings.NewReader("offline"),
		"long":               strings.NewReader("offline release fixture\nextra"),
		"read-failure":       brokenReader{},
	} {
		t.Run(name, func(t *testing.T) {
			if verified.VerifyArchive(m.Artifacts[0].Name, reader) == nil {
				t.Fatal("bad archive accepted")
			}
		})
	}
	reader := &endlessReader{}
	if verified.VerifyArchive(m.Artifacts[0].Name, reader) == nil {
		t.Fatal("oversize archive accepted")
	}
	if reader.consumed != m.Artifacts[0].Size+1 {
		t.Fatalf("read exceeds bound: %d", reader.consumed)
	}
	if verified.VerifyArchive("../"+m.Artifacts[0].Name, strings.NewReader("offline release fixture\n")) == nil {
		t.Fatal("unlisted archive accepted")
	}
	var empty Verified
	if empty.VerifyArchive(m.Artifacts[0].Name, strings.NewReader("offline release fixture\n")) == nil {
		t.Fatal("unverified artifact accepted")
	}
	if empty.VerifyChecksums(nil) == nil {
		t.Fatal("zero verifier accepted checksums")
	}
}

func TestSignerRefusesBadInput(t *testing.T) {
	data, _, _ := signed(t, fixtureManifest())
	for _, key := range []ed25519.PrivateKey{nil, {1}, bytes.Repeat([]byte{1}, ed25519.PrivateKeySize)} {
		if _, err := Sign(data, "fixture-old", key); err == nil {
			t.Fatal("invalid private key accepted")
		}
	}
	if _, err := Sign(data, "bad/id", fixtureKey(t)); err == nil {
		t.Fatal("invalid key ID accepted")
	}
	if _, err := Sign([]byte(`{}`), "fixture-old", fixtureKey(t)); err == nil {
		t.Fatal("invalid payload signed")
	}
}

func TestSizeCheckCannotBeReplacedByHashOnly(t *testing.T) {
	for _, delta := range []int64{-1, 1} {
		m := fixtureManifest()
		m.Artifacts[0].Size += delta
		data, signatures, roots := signed(t, m)
		verified, err := Verify(data, signatures, observed(m), roots)
		if err != nil {
			t.Fatal(err)
		}
		if verified.VerifyArchive(m.Artifacts[0].Name, strings.NewReader("offline release fixture\n")) == nil {
			t.Fatal("matching digest bypassed signed size")
		}
	}
}

func TestUnsortedArtifactNamesRefuse(t *testing.T) {
	m := fixtureManifest()
	next := m.Artifacts[0]
	next.Name = "citadel_v0.0.0-test_windows_amd64.zip"
	m.Artifacts = []Artifact{next, m.Artifacts[0]}
	if _, err := EncodeManifest(m); err == nil {
		t.Fatal("unsorted artifacts accepted")
	}
}
