package botprofile

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Construction (the strings below are pinned by tests):
//
//	secret = nodeShare(32) || aceTeamShare(32)      fixed lengths, so unambiguous
//	key    = HKDF-SHA256(secret, salt=kdfSalt,
//	                     info=kdfInfo || 0x00 || lp(org) || lp(identity), 32 bytes)
//	blob   = 0x01 || nonce(12, crypto/rand, fresh per Seal) ||
//	         AES-256-GCM(key, nonce, inner,
//	                     aad = aadLabel || 0x00 || lp(org) || lp(identity) || lp(contextLabel))
//	inner  = be32(len(metaJSON)) || metaJSON || tar
//
// lp(x) is a uvarint length followed by the bytes. The Chromium major rides in
// metaJSON, inside the AEAD, so it cannot be edited on disk.
const (
	kdfSalt       = "citadel-cli/bot-profile/hkdf-salt/v1"
	kdfInfo       = "citadel-cli/bot-profile/key/v1"
	aadLabel      = "citadel-cli/bot-profile/aead/v1"
	contextPrefix = "bot-profile/v1/"
	blobVersion   = 1
	maxMetaLen    = 1024
	metaFormatV1  = 1
)

func contextLabel(id Identity) string { return contextPrefix + id.ProfileName() }

func lp(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// deriveKey combines the two shares. Both must be exactly ShareLen bytes.
func deriveKey(nodeShare, aceShare []byte, id Identity) ([]byte, error) {
	if len(nodeShare) != ShareLen || len(aceShare) != ShareLen {
		return nil, ErrBadShare
	}
	secret := make([]byte, 0, 2*ShareLen)
	secret = append(secret, nodeShare...)
	secret = append(secret, aceShare...)
	defer zero(secret)
	info := append([]byte(kdfInfo), 0)
	info = lp(info, id.OrgID)
	info = lp(info, id.IdentityID)
	key, err := hkdf.Key(sha256.New, secret, []byte(kdfSalt), string(info), 32)
	if err != nil {
		return nil, fmt.Errorf("botprofile: derive key: %w", err)
	}
	return key, nil
}

type meta struct {
	Format        int `json:"format"`
	ChromiumMajor int `json:"chromium_major"`
}

// sealer implements cobrowseprofile.Sealer over a derived key.
type sealer struct {
	mu    sync.Mutex
	key   []byte
	id    Identity
	major int
}

func newSealer(key []byte, id Identity, major int) (*sealer, error) {
	if len(key) != 32 {
		return nil, ErrBadShare
	}
	return &sealer{key: append([]byte(nil), key...), id: id, major: major}, nil
}

func (s *sealer) aead(label string) (cipher.AEAD, []byte, error) {
	if s.key == nil {
		return nil, nil, ErrLocked
	}
	blk, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, nil, fmt.Errorf("botprofile: cipher: %w", err)
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, fmt.Errorf("botprofile: gcm: %w", err)
	}
	aad := append([]byte(aadLabel), 0)
	aad = lp(aad, s.id.OrgID)
	aad = lp(aad, s.id.IdentityID)
	aad = lp(aad, label)
	return g, aad, nil
}

func (s *sealer) Seal(label string, plaintext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, aad, err := s.aead(label)
	if err != nil {
		return nil, err
	}
	m, _ := json.Marshal(meta{Format: metaFormatV1, ChromiumMajor: s.major})
	inner := make([]byte, 0, 4+len(m)+len(plaintext))
	inner = binary.BigEndian.AppendUint32(inner, uint32(len(m)))
	inner = append(inner, m...)
	inner = append(inner, plaintext...)
	defer zero(inner)
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("botprofile: nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(inner)+g.Overhead())
	out = append(out, blobVersion)
	out = append(out, nonce...)
	return g.Seal(out, nonce, inner, aad), nil
}

func (s *sealer) Unseal(label string, blob []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, aad, err := s.aead(label)
	if err != nil {
		return nil, err
	}
	if len(blob) < 1+g.NonceSize()+g.Overhead() || blob[0] != blobVersion {
		return nil, ErrOpenFailed
	}
	nonce := blob[1 : 1+g.NonceSize()]
	inner, err := g.Open(nil, nonce, blob[1+g.NonceSize():], aad)
	if err != nil {
		return nil, ErrOpenFailed
	}
	defer zero(inner)
	if len(inner) < 4 {
		return nil, ErrOpenFailed
	}
	n := binary.BigEndian.Uint32(inner)
	if n == 0 || n > maxMetaLen || int(n) > len(inner)-4 {
		return nil, ErrOpenFailed
	}
	var m meta
	if json.Unmarshal(inner[4:4+n], &m) != nil || m.Format != metaFormatV1 || m.ChromiumMajor <= 0 {
		return nil, ErrOpenFailed
	}
	if m.ChromiumMajor > s.major {
		return nil, fmt.Errorf("%w (profile %d, runtime %d)", ErrNewerChromium, m.ChromiumMajor, s.major)
	}
	return append([]byte(nil), inner[4+n:]...), nil
}

// Lock clears the key. Idempotent.
func (s *sealer) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	zero(s.key)
	s.key = nil
}
