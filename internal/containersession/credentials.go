package containersession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/declaredsession"
)

// ErrNoCredentials means no sealed bundle exists for the session yet.
var ErrNoCredentials = errors.New("no sealed credentials for session")

const (
	maxCredentialBytes = 64 << 10
	maxBundleBytes     = 1 << 20
	cfgDirName         = "cfg"
	envDirName         = "envdir"
	credEnvFileName    = "credentials.env"
	sealedSuffix       = ".sealed"
)

// Bundle maps a declared credential name to its secret value.
type Bundle map[string][]byte

// CredentialStore is the per-session credential compartment.
type CredentialStore interface {
	Load(session string) (Bundle, error)
	Save(session string, b Bundle) error
}

// Sealer is the sealing half of a nodevault Session. nodevault.Session
// satisfies it, opened unattended via nodevault.OpenKeyCompartment.
type Sealer interface {
	Seal(context string, plaintext []byte) ([]byte, error)
	Unseal(context string, ciphertext []byte) ([]byte, error)
}

// SealedFileStore keeps one sealed bundle per session in Dir, which must lie
// outside every session mount: it holds ciphertext only, bound to the session
// by the sealing context, so a bundle copied to another session fails to open.
type SealedFileStore struct {
	Dir    string
	Sealer Sealer
}

func sealContext(session string) string { return "sessions/" + session }

func (s *SealedFileStore) path(session string) (string, error) {
	if declaredsession.ValidateName(session) != nil || !filepath.IsAbs(s.Dir) {
		return "", ErrSpec
	}
	return filepath.Join(s.Dir, session+sealedSuffix), nil
}

func (s *SealedFileStore) Load(session string) (Bundle, error) {
	p, err := s.path(session)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredentials
	}
	if err != nil || len(raw) > maxBundleBytes {
		return nil, ErrCredential
	}
	plain, err := s.Sealer.Unseal(sealContext(session), raw)
	if err != nil {
		return nil, ErrCredential
	}
	defer wipe(plain)
	var b Bundle
	if json.Unmarshal(plain, &b) != nil {
		return nil, ErrCredential
	}
	return b, nil
}

func (s *SealedFileStore) Save(session string, b Bundle) error {
	p, err := s.path(session)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(b)
	if err != nil {
		return ErrCredential
	}
	defer wipe(plain)
	sealed, err := s.Sealer.Seal(sealContext(session), plain)
	if err != nil {
		return ErrCredential
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return ErrCredential
	}
	tmp, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return ErrCredential
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return ErrCredential
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ErrCredential
	}
	if err := tmp.Close(); err != nil {
		return ErrCredential
	}
	return os.Rename(tmp.Name(), p)
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Materialized is the set of host tmpfs paths a wake mounts.
type Materialized struct {
	// ConfigDir is the host tmpfs directory bound at the harness config dir.
	ConfigDir string
	// Files maps a credential name to its host tmpfs path inside ConfigDir.
	Files map[string]string
	// EnvDir is the host directory bound read-only at the container credential
	// dir, or "" when no credential is an environment variable.
	EnvDir string
}

// credFilePath is the host path of a file credential: the tmpfs mirror of its
// in-container path under the harness config dir.
func credFilePath(runDir string, cfg declaredsession.HarnessConfig, c declaredsession.CredentialSpec) string {
	return filepath.Join(runDir, cfgDirName, filepath.FromSlash(strings.TrimPrefix(c.Path, cfg.Dir+"/")))
}

// Materialize builds the host tmpfs tree for one wake under runDir (which the
// caller has already proven to be tmpfs) with 0700/0600 permissions:
//
//   - cfg/ is bound at the harness config dir, so credentials AND any temp file a
//     rename-based writer stages beside them stay off the volume;
//   - an empty mountpoint directory per persisted subpath, created here as the
//     node user so the engine never has to create one as root;
//   - envdir/credentials.env for env credentials.
//
// A declared credential with no value is a refusal that names only the
// credential. Nothing is written until every credential has been checked.
func Materialize(runDir string, cfg declaredsession.HarnessConfig, specs []declaredsession.CredentialSpec, b Bundle) (*Materialized, error) {
	if err := ValidateBundle(specs, b); err != nil {
		return nil, err
	}
	m := &Materialized{Files: map[string]string{}, ConfigDir: filepath.Join(runDir, cfgDirName)}
	fail := func() (*Materialized, error) {
		_ = Wipe(runDir)
		return nil, ErrRunDir
	}
	if err := os.MkdirAll(m.ConfigDir, 0o700); err != nil {
		return fail()
	}
	for _, sub := range cfg.Persist {
		if err := os.MkdirAll(filepath.Join(m.ConfigDir, sub), 0o700); err != nil {
			return fail()
		}
	}
	var envLines []string
	for _, c := range specs {
		if c.Path != "" {
			p := credFilePath(runDir, cfg, c)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return fail()
			}
			if err := writePrivate(p, b[c.Name]); err != nil {
				return fail()
			}
			m.Files[c.Name] = p
			continue
		}
		envLines = append(envLines, c.Env+"="+string(b[c.Name]))
	}
	if len(envLines) != 0 {
		sort.Strings(envLines)
		m.EnvDir = filepath.Join(runDir, envDirName)
		if err := os.MkdirAll(m.EnvDir, 0o700); err != nil {
			return fail()
		}
		if err := writePrivate(filepath.Join(m.EnvDir, credEnvFileName), []byte(strings.Join(envLines, "\n")+"\n")); err != nil {
			return fail()
		}
	}
	return m, nil
}

// ValidateBundle checks every declared credential has a usable value, naming
// only the credential on failure. Wake calls it before any engine mutation.
func ValidateBundle(specs []declaredsession.CredentialSpec, b Bundle) error {
	for _, c := range specs {
		v, ok := b[c.Name]
		if !ok || len(v) == 0 || len(v) > maxCredentialBytes {
			return fmt.Errorf("%w: %s", ErrCredential, c.Name)
		}
		if c.Env != "" && !validEnvValue(v) {
			return fmt.Errorf("%w: %s (value not representable as an env file line)", ErrCredential, c.Name)
		}
	}
	return nil
}

func validEnvValue(v []byte) bool {
	return !bytes.ContainsAny(v, "\n\r\x00\"'")
}

func writePrivate(path string, data []byte) error {
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ResealBundle reads each file credential back from runDir and returns the
// bundle to store. A harness that rotated an OAuth token rewrote the mounted
// file, so the new bytes must be sealed before the run dir is wiped or the
// rotation (and with it the login) is lost. A missing or empty file never
// replaces the sealed value: an unreadable token must not erase the good one.
func ResealBundle(runDir string, cfg declaredsession.HarnessConfig, specs []declaredsession.CredentialSpec, old Bundle) (Bundle, bool, error) {
	next := Bundle{}
	for k, v := range old {
		next[k] = append([]byte(nil), v...)
	}
	changed := false
	for _, c := range specs {
		if c.Path == "" {
			continue
		}
		got, err := os.ReadFile(credFilePath(runDir, cfg, c))
		if err != nil || len(got) == 0 || len(got) > maxCredentialBytes {
			continue
		}
		if !bytes.Equal(got, old[c.Name]) {
			next[c.Name] = got
			changed = true
		}
	}
	return next, changed, nil
}

// Wipe removes every materialized secret from runDir.
func Wipe(runDir string) error {
	var errs []error
	for _, name := range []string{cfgDirName, envDirName} {
		if err := os.RemoveAll(filepath.Join(runDir, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// HasMaterialized reports whether runDir still holds secrets from a previous
// wake that was never re-sealed (a node crash between wake and sleep).
func HasMaterialized(runDir string) bool {
	for _, name := range []string{cfgDirName, envDirName} {
		if _, err := os.Lstat(filepath.Join(runDir, name)); err == nil {
			return true
		}
	}
	return false
}
