package cursoride

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// A local CA signs the certificates the proxy presents for Cursor's hosts.
// Cursor accepts those only after this CA is trusted. magpie writes the
// files and does not install them into the system trust store: that needs
// an administrator, and a test must not touch the machine's store.

const (
	caCertName = "ca.crt"
	caKeyName  = "ca.key"
)

// CertFile is the CA certificate in dir.
func CertFile(dir string) string { return filepath.Join(dir, caCertName) }

// EnsureCA writes a CA into dir when one is not already there, and returns
// the certificate and its key. A key that does not match the certificate
// is an error, not a new CA written over the old files.
func EnsureCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	certPEM, certErr := os.ReadFile(CertFile(dir))
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, caKeyName))
	if certErr == nil && keyErr == nil {
		return parseCA(certPEM, keyPEM)
	}
	if (certErr == nil) != (keyErr == nil) {
		return nil, nil, errf("CA files are incomplete")
	}
	return writeCA(dir)
}

func writeCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "magpie Cursor IDE CA", Organization: []string{"magpie"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(filepath.Join(dir, caKeyName), "PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}
	if err := writePEM(CertFile(dir), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errf("CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, nil, errf("CA key cannot sign")
	}
	if !cert.IsCA {
		return nil, nil, errf("certificate is not a CA")
	}
	got, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return nil, nil, err
	}
	want, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(got, want) {
		return nil, nil, errf("CA key does not match the certificate")
	}
	return cert, key, nil
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	body := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// leaf is a certificate for host signed by the CA.
func leaf(ca *x509.Certificate, key crypto.Signer, host string) (*tls.Certificate, error) {
	lk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, lk.Public(), key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: lk}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(1)
	}
	return n
}
