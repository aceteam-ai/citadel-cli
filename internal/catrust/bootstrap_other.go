//go:build !linux

package catrust

import (
	"crypto/x509"
	"fmt"
	"runtime"
)

func installProcessRoots(_ []byte, _ []*x509.Certificate) error {
	return fmt.Errorf("private CA bootstrap is currently supported on Linux only; install the CA in the %s system trust store", runtime.GOOS)
}
