// Command releasesign signs and verifies citadel release checksums.
//
// It is run by scripts/release.sh; see docs/release-signing.md.
//
//	go run ./scripts/releasesign check  --key KEY.pem
//	go run ./scripts/releasesign sign   --key KEY.pem --tag vX.Y.Z --in checksums.txt --out checksums.txt.sig
//	go run ./scripts/releasesign verify --tag vX.Y.Z --in checksums.txt --sig checksums.txt.sig
//
// KEY.pem is a PKCS#8 PEM Ed25519 private key. This command never prints key
// material; it reports only the derived key id.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aceteam-ai/citadel-cli/internal/releasesig"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasesign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: releasesign check|sign|verify [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	keyPath := fs.String("key", "", "PKCS#8 PEM Ed25519 private key")
	tag := fs.String("tag", "", "release tag the checksums must belong to (vX.Y.Z)")
	in := fs.String("in", "", "checksums.txt path")
	out := fs.String("out", "", "signature output path")
	sigPath := fs.String("sig", "", "signature path")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "check":
		priv, err := loadTrustedKey(*keyPath)
		if err != nil {
			return err
		}
		fmt.Printf("release signing key ok: key_id=%s\n", releasesig.KeyID(priv.Public().(ed25519.PublicKey)))
		return nil
	case "sign":
		if *in == "" || *out == "" {
			return errors.New("sign requires --in and --out")
		}
		priv, err := loadTrustedKey(*keyPath)
		if err != nil {
			return err
		}
		msg, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		if _, err := releasesig.ParseChecksums(msg, *tag); err != nil {
			return fmt.Errorf("refusing to sign %s: %w", *in, err)
		}
		sig, err := releasesig.Sign(priv, msg)
		if err != nil {
			return err
		}
		// Self-check against the compiled trusted set before publishing.
		id, err := releasesig.Verify(msg, sig)
		if err != nil {
			return fmt.Errorf("produced signature does not verify: %w", err)
		}
		if err := writeNew(*out, sig); err != nil {
			return err
		}
		fmt.Printf("signed %s for %s: key_id=%s\n", filepath.Base(*in), *tag, id)
		return nil
	case "verify":
		if *in == "" || *sigPath == "" {
			return errors.New("verify requires --in and --sig")
		}
		msg, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(*sigPath)
		if err != nil {
			return err
		}
		id, err := releasesig.Verify(msg, sig)
		if err != nil {
			return err
		}
		if _, err := releasesig.ParseChecksums(msg, *tag); err != nil {
			return err
		}
		fmt.Printf("signature ok for %s: key_id=%s\n", *tag, id)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// loadTrustedKey loads the private key and requires that its public key is in
// the compiled trusted set, so a release is never signed with a key the
// updater would refuse.
func loadTrustedKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("--key is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("release signing key unreadable: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("release signing key is not a PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("release signing key could not be parsed as PKCS#8")
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("release signing key is not Ed25519")
	}
	id := releasesig.KeyID(priv.Public().(ed25519.PublicKey))
	for _, k := range releasesig.TrustedKeys() {
		if k.ID == id && k.PublicKey.Equal(priv.Public()) {
			return priv, nil
		}
	}
	return nil, fmt.Errorf("release signing key %s is not in the compiled trusted key set", id)
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
