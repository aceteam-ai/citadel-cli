package status

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log"
	"net"
	"net/http"
)

// ControlTLSReport describes the public leaf loaded by a serving control
// listener. It is observational telemetry, not a device-origin signature or a
// statement that the server certificate was issued by the fabric CA.
type ControlTLSReport struct {
	CertPEM string `json:"cert_pem"`
}

// ControlTLS returns an independent snapshot of the active listener's loaded
// certificate. Never read control-tls files here: replacing a file does not
// replace the TLS configuration held by a running process.
func (s *Server) ControlTLS() *ControlTLSReport {
	s.controlTLSMu.Lock()
	defer s.controlTLSMu.Unlock()
	if s.controlTLSReport == nil {
		return nil
	}
	report := *s.controlTLSReport
	return &report
}

// freezeControlCertificate copies the DER slices that TLS and reporting share.
// A caller replacing its config certificate cannot mutate a serving listener.
func freezeControlCertificate(cert tls.Certificate) (tls.Certificate, *ControlTLSReport) {
	cert.Certificate = append([][]byte(nil), cert.Certificate...)
	for i, der := range cert.Certificate {
		cert.Certificate[i] = append([]byte(nil), der...)
	}
	if len(cert.Certificate) == 0 {
		return cert, nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil
	}
	cert.Leaf = leaf
	return cert, &ControlTLSReport{CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))}
}

func (s *Server) serveControlListener(ln net.Listener, cfg *tls.Config, report *ControlTLSReport) {
	s.controlTLSMu.Lock()
	s.controlTLSListeners++
	s.controlTLSReport = report
	s.controlTLSMu.Unlock()
	defer func() {
		s.controlTLSMu.Lock()
		s.controlTLSListeners--
		if s.controlTLSListeners == 0 {
			s.controlTLSReport = nil
		}
		s.controlTLSMu.Unlock()
	}()
	if err := s.controlHTTPServer.Serve(tls.NewListener(ln, cfg)); err != nil && err != http.ErrServerClosed {
		log.Printf("[status] control listener error (mutating endpoints refused): %v", err)
	}
}
