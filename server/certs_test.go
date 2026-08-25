package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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

// TestGenerateLeafFreshEachCall signs two leaves back to back: they must
// be different certificates with different keypairs. generateLeaf itself
// always generates; the reuse policy lives in loadOrGenerateLeaf.
func TestGenerateLeafFreshEachCall(t *testing.T) {
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

// leafFingerprint parses the stored server.crt and returns its fingerprint.
func leafFingerprint(t *testing.T, certFile string) string {
	t.Helper()
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode stored leaf PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprintOf(cert)
}

// setupLeaf generates a CA plus a leaf in a temp dir, returning the pieces
// the leaf-reuse tests mutate between loads.
func setupLeaf(t *testing.T) (dir string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, certFile, keyFile string) {
	t.Helper()
	dir = t.TempDir()
	ca, caKey, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if _, err := loadOrGenerateLeaf(ca, caKey, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	return dir, ca, caKey, certFile, keyFile
}

// TestLeafStableAcrossRuns loads the leaf twice with an unchanged CA and
// network: the second load must reuse the stored pair (same fingerprint).
// Clients that skip CA installation key their browser warning exception to
// the leaf, so a silently rotated leaf would force them to re-accept the
// warning on every start.
func TestLeafStableAcrossRuns(t *testing.T) {
	_, ca, caKey, certFile, keyFile := setupLeaf(t)
	before := leafFingerprint(t, certFile)
	if _, err := loadOrGenerateLeaf(ca, caKey, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	if after := leafFingerprint(t, certFile); before != after {
		t.Fatal("leaf fingerprint changed across reloads: leaf was regenerated instead of reused")
	}
}

// TestLeafMissingKeyRegenerates deletes server.key: the pair is unusable,
// so a fresh leaf must be generated (and the files restored to coherence).
func TestLeafMissingKeyRegenerates(t *testing.T) {
	_, ca, caKey, certFile, keyFile := setupLeaf(t)
	before := leafFingerprint(t, certFile)
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateLeaf(ca, caKey, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	if after := leafFingerprint(t, certFile); before == after {
		t.Fatal("missing leaf key did not trigger leaf regeneration")
	}
}

// TestLeafCorruptRegenerates truncates server.crt: the parse fails, so a
// fresh leaf must be generated.
func TestLeafCorruptRegenerates(t *testing.T) {
	_, ca, caKey, certFile, keyFile := setupLeaf(t)
	before := leafFingerprint(t, certFile)
	if err := os.WriteFile(certFile, []byte("not a PEM cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateLeaf(ca, caKey, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	if after := leafFingerprint(t, certFile); before == after {
		t.Fatal("corrupt leaf did not trigger leaf regeneration")
	}
}

// TestLeafOrphanedByCARotationRegenerates regenerates the CA while keeping
// the old leaf: the old leaf's signature no longer verifies against the
// current CA, so a fresh leaf must be generated.
func TestLeafOrphanedByCARotationRegenerates(t *testing.T) {
	dir, _, _, certFile, keyFile := setupLeaf(t)
	before := leafFingerprint(t, certFile)
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	ca2, caKey2, _, err := loadOrGenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrGenerateLeaf(ca2, caKey2, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	if after := leafFingerprint(t, certFile); before == after {
		t.Fatal("CA rotation did not trigger leaf regeneration")
	}
}

// TestLeafMissingSANRegenerates and TestLeafExpiredRegenerates regenerate
// the stored leaf with a defect and check the reuse path rejects it.
func TestLeafMissingSANRegenerates(t *testing.T) {
	testDefectiveLeafRegenerates(t, func(tpl *x509.Certificate) {
		tpl.IPAddresses = nil // no IP SANs at all: current IPs cannot match
	})
}

func TestLeafExpiredRegenerates(t *testing.T) {
	testDefectiveLeafRegenerates(t, func(tpl *x509.Certificate) {
		tpl.NotBefore = time.Now().Add(-2 * leafReuseMargin)
		tpl.NotAfter = time.Now().Add(-time.Hour)
	})
}

// testDefectiveLeafRegenerates overwrites the stored leaf with one validly
// signed by the current CA but carrying mutate's defect: the reuse check
// must reject it and loadOrGenerateLeaf must generate a fresh leaf.
func testDefectiveLeafRegenerates(t *testing.T, mutate func(*x509.Certificate)) {
	_, ca, caKey, certFile, keyFile := setupLeaf(t)
	before := leafFingerprint(t, certFile)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	dnsNames, ips := leafSANs()
	tpl := x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(-time.Hour).Add((leafValidityDays - 1) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	mutate(&tpl)
	certDER, err := x509.CreateCertificate(rand.Reader, &tpl, ca, &priv.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadOrGenerateLeaf(ca, caKey, certFile, keyFile); err != nil {
		t.Fatal(err)
	}
	if before == leafFingerprint(t, certFile) || string(certPEM) == string(mustRead(t, certFile)) {
		t.Fatal("defective leaf was reused instead of regenerated")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
