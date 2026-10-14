// Package artifact detects ordinary file replacement and mutation on a trusted
// node. A generation is neither a signature nor a content rehash at read time.
package artifact

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

const Algorithm = "linux-stat-v1"
const MaxSafeInteger int64 = 1<<53 - 1

// Identity contains only descriptor observations. Access time is deliberately
// absent: reading an artifact must not invalidate its generation.
type Identity struct {
	Bytes                          int64
	Device, Inode                  uint64
	MtimeSeconds, MtimeNanoseconds int64
	CtimeSeconds, CtimeNanoseconds int64
}

type Statter interface{ Stat() (os.FileInfo, error) }

func Capture(file Statter) (Identity, error) {
	info, err := file.Stat()
	if err != nil {
		return Identity{}, err
	}
	return observe(info)
}

func canonicalDigest(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	for _, c := range value[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func ValidSHA256(value string) bool     { return canonicalDigest(value, "sha256:") }
func ValidGeneration(value string) bool { return Supported() && canonicalDigest(value, Algorithm+":") }

// Generation associates a recorded content hash with descriptor metadata. The
// LF-terminated encoding is the versioned wire contract; regular is a literal
// field because Capture refuses nonregular files.
func (i Identity) Generation(contentSHA256 string) (string, error) {
	if !Supported() || !ValidSHA256(contentSHA256) || i.Bytes < 0 || i.Bytes > MaxSafeInteger ||
		i.MtimeNanoseconds < 0 || i.MtimeNanoseconds >= 1e9 || i.CtimeNanoseconds < 0 || i.CtimeNanoseconds >= 1e9 {
		return "", fmt.Errorf("unsupported artifact identity")
	}
	encoded := fmt.Sprintf("%s\n%s\n%d\n%d\n%d\nregular\n%d\n%d\n%d\n%d\n", Algorithm, contentSHA256, i.Bytes,
		i.Device, i.Inode, i.MtimeSeconds, i.MtimeNanoseconds, i.CtimeSeconds, i.CtimeNanoseconds)
	return fmt.Sprintf("%s:%x", Algorithm, sha256.Sum256([]byte(encoded))), nil
}
