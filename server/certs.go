package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Certificate layout in certDirectory(): a long-lived local root CA
// (ca.crt/ca.key) that clients install into their trust store once, plus a
// short-lived server leaf (server.crt/server.key) signed by that CA. The CA
// must stay stable across runs — it is the one artifact users install — so
// it is only (re)generated when its files are missing or unreadable. The
// leaf is regenerated on every start instead: fresh keypair, fresh SAN list
// (LAN IPs can change between runs), 825-day validity. Leaf rotation is
// invisible to clients because they trust the CA, not the leaf.
//
// SECURITY: ca.key can now sign certificates that every installed client
// trusts, so it is written mode 0600 and is never served over HTTP (only
// the public ca.crt is, via /ca.crt).

// leafValidityDays: Apple requires TLS server certificates to be valid for
// 825 days or fewer (measured end-to-end, NotAfter minus NotBefore), and
// applies this even to chains anchored by user-installed root CAs — a leaf
// over the cap makes iOS show "untrusted" no matter how the CA is
// installed. NotBefore is backdated 1h for clock skew, so NotAfter is
// placed one day inside the cap to keep the total below 825 days.
const leafValidityDays = 825

func certDirectory() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "inara")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

// randomSerial returns a random 128-bit certificate serial number.
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// fingerprintOf returns the SHA-256 fingerprint of a certificate as
// lowercase hex (the stable string users compare when installing the CA).
func fingerprintOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// loadOrGenerateCA returns the local root CA (certificate and matching
// key) plus the PEM encoding of ca.crt. If both files load cleanly and
// belong together they are reused untouched; on any gap (first run, a
// missing file, corrupt PEM, key/cert mismatch) a fresh CA pair is
// generated and the caller-visible log warning says that every client
// device that installed the old CA must re-install the new one.
func loadOrGenerateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	caCrtPath := filepath.Join(dir, "ca.crt")
	caKeyPath := filepath.Join(dir, "ca.key")

	if certPEM, err := os.ReadFile(caCrtPath); err == nil {
		if keyPEM, err := os.ReadFile(caKeyPath); err == nil {
			cert, key, err := parseCA(certPEM, keyPEM)
			if err == nil {
				log.Printf("certificate: loaded existing CA from %s (fingerprint %s)", caCrtPath, fingerprintOf(cert))
				return cert, key, certPEM, nil
			}
			log.Printf("certificate: stored CA is unusable (%v); generating a new one", err)
		} else {
			log.Printf("certificate: CA key file missing; generating a new CA pair")
		}
	} else {
		log.Printf("certificate: no CA found at %s; generating one", caCrtPath)
	}

	log.Printf("certificate: every client device that installed the old inara certificate must RE-INSTALL the new one from /ca.crt")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}

	cn := "inara Local CA"
	if hostname, herr := os.Hostname(); herr == nil && hostname != "" {
		cn += " (" + hostname + ")"
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"inara"},
		},
		// CAs have no browser-mandated validity cap; 10 years keeps the
		// installed trust anchor stable for the foreseeable life of an
		// install. Devices typically warn when an installed CA nears expiry.
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}

	// ca.crt is a public key and is served unauthenticated from /ca.crt, so
	// a world-readable file is fine; ca.key is the sensitive one.
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(caCrtPath, certPEM, 0644); err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(caKeyPath, keyPEM, 0600); err != nil {
		return nil, nil, nil, err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, err
	}
	log.Printf("certificate: generated new CA %q in %s (fingerprint %s)", cn, caCrtPath, fingerprintOf(cert))
	return cert, key, certPEM, nil
}

// parseCA decodes the stored CA PEM files and verifies they describe a CA
// whose key matches its certificate.
func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("failed to decode CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("stored certificate is not a CA")
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, nil, fmt.Errorf("failed to decode CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("CA key does not match CA certificate")
	}
	return cert, key, nil
}

// generateLeaf signs a fresh server certificate with the CA and writes it
// to server.crt/server.key. It runs on every start: the keypair is fresh
// and the SAN list reflects the current interface IPs, so LAN address
// changes (DHCP) never strand the cert. SANs: localhost/loopbacks plus all
// non-loopback interface IPs, matching the URLs Run prints.
func generateLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, certFile, keyFile string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"inara"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		// Total NotBefore->NotAfter must stay <= 825 days (see above), so
		// anchor NotAfter to NotBefore rather than to now.
		NotAfter:              time.Now().Add(-time.Hour).Add((leafValidityDays - 1) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	for _, ip := range LocalIPs(false) {
		parsed := net.ParseIP(ip)
		if parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, caCert, &priv.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// The leaf files are rewritten unconditionally on every start; they
	// exist for inspection/debugging and to mirror on disk what tls.Config
	// serves. Only the CA is ever reused across runs.
	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		return tls.Certificate{}, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse generated keypair: %w", err)
	}
	return cert, nil
}
