//go:build linux

package catrust

import (
	"crypto/x509"
	"fmt"
	"os"
)

// Keep this in the same order as crypto/x509/root_linux.go. Go loads the
// first available file plus every certificate directory.
var linuxSystemCertFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

func installProcessRoots(customPEM []byte, certs []*x509.Certificate) error {
	original, wasSet := os.LookupEnv("SSL_CERT_FILE")
	systemPEM, err := systemCertFileBytes(original, linuxSystemCertFiles)
	if err != nil {
		return err
	}
	bundle := append([]byte(nil), systemPEM...)
	if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
		bundle = append(bundle, '\n')
	}
	bundle = append(bundle, customPEM...)

	f, err := os.CreateTemp("", "citadel-control-ca-*.pem")
	if err != nil {
		return fmt.Errorf("create temporary CA bundle: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return fmt.Errorf("secure temporary CA bundle: %w", err)
	}
	if _, err := f.Write(bundle); err != nil {
		f.Close()
		return fmt.Errorf("write temporary CA bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temporary CA bundle: %w", err)
	}
	if err := os.Setenv("SSL_CERT_FILE", name); err != nil {
		return fmt.Errorf("set temporary CA bundle: %w", err)
	}
	roots, loadErr := x509.SystemCertPool()
	var restoreErr error
	if wasSet {
		restoreErr = os.Setenv("SSL_CERT_FILE", original)
	} else {
		restoreErr = os.Unsetenv("SSL_CERT_FILE")
	}
	if restoreErr != nil {
		return fmt.Errorf("restore SSL_CERT_FILE: %w", restoreErr)
	}
	if loadErr != nil {
		return fmt.Errorf("load system and private CA roots: %w", loadErr)
	}
	for _, cert := range certs {
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err != nil {
			return fmt.Errorf("private CA is absent from process trust roots; restart citadel and supply --ca-cert before any TLS work: %w", err)
		}
	}
	return nil
}

func systemCertFileBytes(explicit string, candidates []string) ([]byte, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return nil, fmt.Errorf("read existing SSL_CERT_FILE %s: %w", explicit, err)
		}
		return data, nil
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read system CA bundle %s: %w", path, err)
		}
	}
	return nil, nil
}
