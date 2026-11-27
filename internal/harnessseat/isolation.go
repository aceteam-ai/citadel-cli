// Package harnessseat holds offline, node-local seat identity foundations.
// Nothing here reads a credential, starts a vendor process, grants a node
// permission, selects a subscription, or establishes native login isolation.
package harnessseat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Harness string

const (
	Claude  Harness = "claude"
	Codex   Harness = "codex"
	Copilot Harness = "copilot"
	Muse    Harness = "muse"
)

var (
	ErrInvalidIdentity  = errors.New("seat_invalid_identity")
	ErrInvalidDigest    = errors.New("seat_invalid_digest")
	ErrDuplicateAccount = errors.New("seat_duplicate_account")
	seatSlug            = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// Identity is local-only. JSON serialization deliberately exposes no local
// paths, owner name or raw seat id; use Projection at a reporting boundary.
// Validation is structural only: a future registry owner must separately prove
// filesystem ownership, private modes, scope, enabled state and vendor identity.
type Identity struct {
	ID        string  `json:"-" yaml:"id"`
	Harness   Harness `json:"-" yaml:"harness"`
	Owner     string  `json:"-" yaml:"os_user"`
	HomeDir   string  `json:"-" yaml:"-"`
	ConfigDir string  `json:"-" yaml:"config_dir"`
}

func knownHarness(h Harness) bool {
	switch h {
	case Claude, Codex, Copilot, Muse:
		return true
	}
	return false
}

func cleanText(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

func privateRoot(path string) bool {
	return cleanText(path, 4096) && filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Dir(path) != path
}

func (i Identity) Validate() error {
	if !seatSlug.MatchString(i.ID) || !knownHarness(i.Harness) || !cleanText(i.Owner, 256) || !privateRoot(i.HomeDir) || !privateRoot(i.ConfigDir) {
		return ErrInvalidIdentity
	}
	rel, err := filepath.Rel(i.HomeDir, i.ConfigDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ErrInvalidIdentity
	}
	return nil
}

// ChildEnvironment builds, rather than edits, an environment. In particular an
// inherited provider token, config override, loader hook or shell startup hook
// cannot acquire authority merely because a new vendor-specific name appears.
// ownerPath must come from the trusted owner resolver, never a remote request.
// Callers must create/prove the private directories before using this plan.
func ChildEnvironment(i Identity, ownerPath string, inherited []string) ([]string, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if !cleanText(ownerPath, 16384) {
		return nil, ErrInvalidIdentity
	}
	values := map[string]string{"HOME": i.HomeDir, "USERPROFILE": i.HomeDir, "USER": i.Owner, "LOGNAME": i.Owner, "PATH": ownerPath}
	for _, entry := range inherited {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.ContainsRune(value, 0) {
			continue
		}
		key = strings.ToUpper(key)
		switch key {
		case "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ", "SYSTEMROOT":
			values[key] = value
		}
	}
	switch i.Harness {
	case Claude:
		values["CLAUDE_CONFIG_DIR"] = i.ConfigDir
	case Codex:
		values["CODEX_HOME"] = i.ConfigDir
	case Copilot:
		values["COPILOT_HOME"], values["GH_CONFIG_DIR"] = i.ConfigDir, filepath.Join(i.ConfigDir, "gh")
	case Muse:
		values["XDG_CONFIG_HOME"], values["MUSE_LOGIN"] = i.ConfigDir, "0"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func localDigest(salt []byte, domain string, parts ...string) (string, error) {
	if len(salt) != 32 {
		return "", ErrInvalidDigest
	}
	m := hmac.New(sha256.New, salt)
	_, _ = m.Write([]byte(domain))
	for _, part := range parts {
		if !cleanText(part, 4096) {
			return "", ErrInvalidDigest
		}
		_, _ = m.Write([]byte{0})
		_, _ = m.Write([]byte(part))
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

// AccountDigest accepts only a status-probe identity, not credential contents.
// The salt belongs to the node and must be persisted privately by the future
// registry owner. There is no read, persistence or logging of raw identities.
func AccountDigest(salt []byte, harness Harness, accountID string) (string, error) {
	if !knownHarness(harness) {
		return "", ErrInvalidIdentity
	}
	return localDigest(salt, "harness-account-v1", string(harness), accountID)
}

type PublicIdentity struct {
	SeatRef string  `json:"seat_ref"`
	Harness Harness `json:"harness"`
}

func (i Identity) Projection(salt []byte, nodeIdentity string) (PublicIdentity, error) {
	if err := i.Validate(); err != nil {
		return PublicIdentity{}, err
	}
	ref, err := localDigest(salt, "harness-seat-ref-v1", nodeIdentity, i.ID)
	if err != nil {
		return PublicIdentity{}, err
	}
	return PublicIdentity{SeatRef: ref, Harness: i.Harness}, nil
}

// AccountBinding never leaves the local registry. A blank/unknown identity is
// not evidence of uniqueness and must not admit automatic routing.
type AccountBinding struct {
	SeatID  string  `json:"-"`
	Harness Harness `json:"-"`
	Digest  string  `json:"-"`
}

func ValidateBindings(bindings []AccountBinding) error {
	if len(bindings) > 64 {
		return ErrInvalidIdentity
	}
	ids := map[string]bool{}
	accounts := map[string]bool{}
	for _, b := range bindings {
		if !seatSlug.MatchString(b.SeatID) || !knownHarness(b.Harness) || ids[b.SeatID] {
			return ErrInvalidIdentity
		}
		ids[b.SeatID] = true
		decoded, err := hex.DecodeString(b.Digest)
		if err != nil || len(decoded) != sha256.Size || b.Digest != strings.ToLower(b.Digest) {
			return ErrInvalidDigest
		}
		key := string(b.Harness) + ":" + b.Digest
		if accounts[key] {
			return ErrDuplicateAccount
		}
		accounts[key] = true
	}
	return nil
}
