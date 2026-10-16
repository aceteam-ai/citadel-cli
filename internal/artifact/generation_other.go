//go:build !linux

package artifact

import (
	"fmt"
	"os"
)

func Supported() bool { return false }
func observe(os.FileInfo) (Identity, error) {
	return Identity{}, fmt.Errorf("artifact generation observations unsupported on this platform")
}
