package ingress

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/rfc2136"
	"go.uber.org/zap"
)

// CertProvider supplies the public wildcard TLS material for the ingress
// listener. It is an interface so tests inject a self-signed provider and never
// touch ACME, and so the production ACME path (certmagic DNS-01) can be swapped
// for a mounted, already-issued wildcard (StaticFileCertProvider) -- the DoR's
// recommended first-deployment form, which avoids multi-issuer churn.
//
// Start is separate from TLSConfig because the ACME provider must be able to
// BLOCK until the wildcard is present (certmagic ManageSync) or fail loudly; a
// TLSConfig() accessor alone cannot express that. Start is called once, before
// the listener serves; TLSConfig() is called after Start returns nil.
type CertProvider interface {
	Start(ctx context.Context) error
	TLSConfig() *tls.Config
}

// --- Self-signed provider (tests / dev only) --------------------------------

// SelfSignedCertProvider generates an in-memory self-signed wildcard cert. It is
// for unit tests and local development ONLY -- it never touches ACME and a real
// client would reject its certificate.
type SelfSignedCertProvider struct {
	AppsDomain string
	cert       *tls.Certificate
}

// Start generates the self-signed certificate.
func (p *SelfSignedCertProvider) Start(_ context.Context) error {
	cert, err := generateSelfSigned(p.AppsDomain)
	if err != nil {
		return err
	}
	p.cert = cert
	return nil
}

// TLSConfig returns a config serving the generated cert for every SNI.
func (p *SelfSignedCertProvider) TLSConfig() *tls.Config {
	c := p.cert
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c, nil
		},
	}
}

func generateSelfSigned(appsDomain string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: appsDomain},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{appsDomain, "*." + appsDomain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// --- Static-file provider (mounted, already-issued wildcard) ----------------

// StaticFileCertProvider loads an already-issued wildcard cert + key from disk.
// This is the DoR's recommended FIRST-deployment form: issue the wildcard once
// (out of band or on one box), mount cert+key into each ingress container, and
// avoid every ingress instance racing to issue the same wildcard against the CA
// (the duplicate-certificate rate limit). It adds no ACME dependency at runtime.
type StaticFileCertProvider struct {
	CertFile string
	KeyFile  string
	cert     *tls.Certificate
}

// Start loads and validates the cert/key pair.
func (p *StaticFileCertProvider) Start(_ context.Context) error {
	cert, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
	if err != nil {
		return fmt.Errorf("ingress: load wildcard cert: %w", err)
	}
	p.cert = &cert
	return nil
}

// TLSConfig serves the loaded cert for every SNI.
func (p *StaticFileCertProvider) TLSConfig() *tls.Config {
	c := p.cert
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c, nil
		},
	}
}

// --- ACME DNS-01 provider (certmagic) ---------------------------------------

// RFC2136Config is a TSIG-scoped dynamic-DNS credential for the delegated
// _acme-challenge zone. RFC 2136 + TSIG is the protocol-level way to update ANY
// authoritative nameserver (BIND/Knot/PowerDNS/CoreDNS) and a TSIG key is
// exactly the "scoped to the delegated zone, never the parent" credential the
// DoR requires -- it does not commit the design to a specific DNS vendor SDK.
type RFC2136Config struct {
	Server  string // authoritative nameserver, e.g. "ns1.example.net:53"
	KeyName string // TSIG key name
	KeyAlg  string // TSIG algorithm, e.g. "hmac-sha256."
	Key     string // base64-encoded TSIG secret
}

// ACMEConfig configures the certmagic DNS-01 wildcard provider.
type ACMEConfig struct {
	AppsDomain string
	Email      string
	// CA is the ACME directory URL. Empty uses certmagic's default (Let's
	// Encrypt production). Tests/staging pass the staging directory.
	CA string
	// StorageDir is where the cert cache lives. It is passed in by the cmd layer
	// (filepath.Join(network.GetNodeConfigDir(), "ingress", "certs")); this
	// package NEVER calls network.GetNodeConfigDir itself -- on a box that also
	// runs a real node that would resolve to the live node's dir (CLAUDE.md #787).
	StorageDir string
	// DNSZone is the delegated zone set as certmagic's OverrideDomain. It is
	// REQUIRED: certmagic's DNS01Solver does NOT follow CNAMEs (confirmed against
	// v0.25.x), so the challenge TXT must be published directly on the delegated
	// zone the scoped credential can write, not on _acme-challenge.<apps-domain>.
	DNSZone string
	RFC2136 RFC2136Config
	// Logger is optional; nil uses a no-op logger (certmagic's default zap logger
	// is very noisy).
	Logger *zap.Logger
}

// ACMECertProvider manages the wildcard via certmagic DNS-01.
type ACMECertProvider struct {
	cfg  ACMEConfig
	tlsC *tls.Config
}

// NewACMECertProvider validates config and returns the provider. It does not
// contact the CA until Start.
func NewACMECertProvider(cfg ACMEConfig) (*ACMECertProvider, error) {
	if cfg.AppsDomain == "" {
		return nil, fmt.Errorf("ingress acme: apps domain required")
	}
	if cfg.DNSZone == "" {
		return nil, fmt.Errorf("ingress acme: delegated DNS zone required (OverrideDomain; solver does not follow CNAME)")
	}
	if cfg.StorageDir == "" {
		return nil, fmt.Errorf("ingress acme: storage dir required")
	}
	return &ACMECertProvider{cfg: cfg}, nil
}

// Start obtains (or loads from cache) the wildcard cert and BLOCKS until it is
// present or fails loudly. It never falls back to a self-signed cert.
func (p *ACMECertProvider) Start(ctx context.Context) error {
	logger := p.cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	storage := &certmagic.FileStorage{Path: p.cfg.StorageDir}

	dnsProvider := &rfc2136.Provider{
		Server:  p.cfg.RFC2136.Server,
		KeyName: p.cfg.RFC2136.KeyName,
		KeyAlg:  p.cfg.RFC2136.KeyAlg,
		Key:     p.cfg.RFC2136.Key,
	}

	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	magic = certmagic.New(cache, certmagic.Config{
		Storage: storage,
		Logger:  logger,
	})

	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     p.cfg.CA, // "" -> certmagic default (LE production)
		Email:  p.cfg.Email,
		Agreed: true,
		Logger: logger,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider:    dnsProvider,
				OverrideDomain: p.cfg.DNSZone,
				Logger:         logger,
			},
		},
	})
	magic.Issuers = []certmagic.Issuer{issuer}

	names := []string{p.cfg.AppsDomain, "*." + p.cfg.AppsDomain}
	if err := magic.ManageSync(ctx, names); err != nil {
		return fmt.Errorf("ingress acme: manage %v: %w", names, err)
	}

	tlsC := magic.TLSConfig()
	// Prepend our application protocols; certmagic pre-populated NextProtos with
	// the ACME TLS-ALPN sentinel, which we leave intact (harmless here, since we
	// solve via DNS-01, but required if TLS-ALPN is ever enabled).
	tlsC.NextProtos = append([]string{"h2", "http/1.1"}, tlsC.NextProtos...)
	tlsC.MinVersion = tls.VersionTLS12
	p.tlsC = tlsC
	return nil
}

// TLSConfig returns certmagic's managed TLS config (GetCertificate reads from
// the in-memory cache / storage).
func (p *ACMECertProvider) TLSConfig() *tls.Config { return p.tlsC }
