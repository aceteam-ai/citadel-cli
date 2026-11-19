package cobrowseprofile

// Suspend manifests are separate from profile archives. They share the already
// unlocked vault session, but use a different context, bound to the session,
// capture generation and profile. No passphrase/key is retained or exported.

import (
	"errors"
	"regexp"
	"strconv"
)

const (
	maxSuspendManifestBytes = 256 * 1024
	maxSuspendSealedBytes   = maxSuspendManifestBytes + 4096
)

var suspendSessionID = regexp.MustCompile(`^cb-[0-9a-f]{1,61}$`)

func (h *Handle) suspendContext(id string, generation uint64) (string, error) {
	if h == nil || h.sess == nil || h.context == "" || !suspendSessionID.MatchString(id) || generation == 0 {
		return "", errors.New("cobrowseprofile: invalid suspended manifest binding")
	}
	return "cobrowse-suspend/v1/" + id + "/" + strconv.FormatUint(generation, 10) + "/" + h.context, nil
}

// SealSuspendManifest seals a bounded manifest under this unlocked profile.
// Callers must validate its schema and clear their plaintext buffer afterward.
// This does not persist the profile or write any file, and never unlocks a vault.
func (h *Handle) SealSuspendManifest(id string, generation uint64, plaintext []byte) ([]byte, error) {
	context, err := h.suspendContext(id, generation)
	if err != nil {
		return nil, err
	}
	if len(plaintext) == 0 || len(plaintext) > maxSuspendManifestBytes {
		return nil, errors.New("cobrowseprofile: suspended manifest exceeds plaintext bound")
	}
	blob, err := h.sess.Seal(context, plaintext)
	if err != nil {
		zero(blob)
		return nil, errors.New("cobrowseprofile: could not seal suspended manifest")
	}
	if len(blob) == 0 || len(blob) > maxSuspendSealedBytes {
		zero(blob)
		return nil, errors.New("cobrowseprofile: suspended manifest exceeds ciphertext bound")
	}
	return blob, nil
}

// UnsealSuspendManifest requires a newly human-unlocked handle at reopen. An
// wrong profile or capture cannot be decrypted through another context. The
// manager must check authorization and expiry before calling this primitive.
// Callers own the returned plaintext and must validate it and clear it after use.
func (h *Handle) UnsealSuspendManifest(id string, generation uint64, blob []byte) ([]byte, error) {
	context, err := h.suspendContext(id, generation)
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 || len(blob) > maxSuspendSealedBytes {
		return nil, errors.New("cobrowseprofile: suspended manifest exceeds ciphertext bound")
	}
	plain, err := h.sess.Unseal(context, blob)
	if err != nil {
		zero(plain)
		return nil, errors.New("cobrowseprofile: could not unseal suspended manifest")
	}
	if len(plain) == 0 || len(plain) > maxSuspendManifestBytes {
		zero(plain)
		return nil, errors.New("cobrowseprofile: suspended manifest exceeds plaintext bound")
	}
	return plain, nil
}
