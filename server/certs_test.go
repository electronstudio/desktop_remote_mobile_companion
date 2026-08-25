package server

import (
	"crypto/ecdsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCAISRootCA checks the generated CA carries the properties a client
// trust store requires of an installable root: IsCA and the cert-sign key
// usage (Android/iOS refuse to anchor trust on a cert marked non-CA).
func TestCAISRootCA(t *testing.T) {
	dir := t.TempDir()
	ca, _, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Fatal("CA certificate has IsCA=false")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA certificate lacks KeyUsageCertSign")
	}
}

// TestCAStableAcrossRuns loads the CA twice: the second load must reuse the
// stored pair (same fingerprint), because clients install the CA once and a
// silently rotated CA would break every installed device.
func TestCAStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	ca1, key1, pem1, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, key2, pem2, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintOf(ca1) != fingerprintOf(ca2) {
		t.Fatal("CA fingerprint changed across reloads: CA was regenerated instead of reused")
	}
	if !key1.PublicKey.Equal(&key2.PublicKey) {
		t.Fatal("reloaded CA key differs from the original")
	}
	if string(pem1) != string(pem2) {
		t.Fatal("reloaded CA PEM differs from the stored bytes")
	}
}

// TestCABrokenKeyRegenerates simulates a corrupt CA key file: the pair must
// be regenerated (clients then need to re-install, which loadOrGenerateCA
// logs) and the result must be coherent again.
func TestCABrokenKeyRegenerates(t *testing.T) {
	dir := t.TempDir()
	ca1, _, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not a PEM key"), 0600); err != nil {
		t.Fatal(err)
	}
	ca2, _, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintOf(ca1) == fingerprintOf(ca2) {
		t.Fatal("corrupt CA key did not trigger CA regeneration")
	}
}

// TestLeafVerifiesAgainstCA signs a leaf and verifies the full chain
// Android/iOS will verify: leaf signed by our CA, ServerAuth EKU,
// localhost hostname, and the browser-mandated 825-day validity cap.
func TestLeafVerifiesAgainstCA(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	leaf, err := generateLeaf(ca, caKey, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Certificate) == 0 {
		t.Fatal("leaf chain is empty")
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := parsed.Verify(x509.VerifyOptions{
		DNSName: "localhost",
		Roots:   pool,
	}); err != nil {
		t.Fatalf("leaf does not verify against CA for localhost: %v", err)
	}

	hasLoopback := false
	for _, ip := range parsed.IPAddresses {
		if ip.String() == "127.0.0.1" {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Fatal("leaf lacks 127.0.0.1 IP SAN")
	}

	// Apple measures the cap end-to-end (NotAfter minus NotBefore) and
	// rejects leaves over 825 days even under user-installed CAs; the
	// NotBefore clock-skew backdate must not push the total over the cap.
	if got := parsed.NotAfter.Sub(parsed.NotBefore); got > leafValidityDays*24*time.Hour {
		t.Fatalf("leaf validity %s exceeds Apple's %d-day TLS server cert cap", got, leafValidityDays)
	}

	// The leaf files must be written (0600 key) so the on-disk layout
	// mirrors what is served.
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("leaf key file mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestLeafFreshEachStart signs two leaves back to back: they must be
// different certificates with different keypairs (regeneration-on-every-run
// is the design, so IP changes self-heal on restart).
func TestLeafFreshEachStart(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "server.crt")
	keyFile := filepath.Join(dir, "server.key")
	leaf1, err := generateLeaf(ca, caKey, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, err := generateLeaf(ca, caKey, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf1.Certificate) == 0 || len(leaf2.Certificate) == 0 {
		t.Fatal("empty leaf chain")
	}
	if string(leaf1.Certificate[0]) == string(leaf2.Certificate[0]) {
		t.Fatal("regenerated leaf is byte-identical to the previous one")
	}
	pub1 := leaf1.PrivateKey.(*ecdsa.PrivateKey).PublicKey
	pub2 := leaf2.PrivateKey.(*ecdsa.PrivateKey).PublicKey
	if pub1.Equal(&pub2) {
		t.Fatal("regenerated leaf reuses the same keypair")
	}
}
