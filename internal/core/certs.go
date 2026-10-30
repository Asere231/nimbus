package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Certs holds the paths of the local CA and the server certificate.
//
// Why TLS at all: the Terraform azurerm provider and the Azure SDKs only talk
// HTTPS to the metadata and ARM endpoints. Nimbus generates its own CA once;
// you trust that CA on your machine, and every server cert it signs is valid.
type Certs struct {
	Dir    string
	CACert string // trust this file in your OS
	CAKey  string
	Cert   string
	Key    string
}

// EnsureCerts creates a CA and a localhost server certificate in dir if they
// do not exist yet, and returns their paths.
func EnsureCerts(dir string) (*Certs, error) {
	c := &Certs{
		Dir:    dir,
		CACert: filepath.Join(dir, "nimbus-ca.pem"),
		CAKey:  filepath.Join(dir, "nimbus-ca-key.pem"),
		Cert:   filepath.Join(dir, "nimbus-server.pem"),
		Key:    filepath.Join(dir, "nimbus-server-key.pem"),
	}
	if fileExists(c.CACert) && fileExists(c.Cert) && fileExists(c.Key) {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// 1. Certificate authority (10 years).
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Nimbus Local CA", Organization: []string{"Nimbus"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caParsed, _ := x509.ParseCertificate(caDER)

	// 2. Server certificate for localhost (825 days: the max macOS accepts).
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "localhost", Organization: []string{"Nimbus"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 825),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "*.localhost", "nimbus", "host.docker.internal"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caParsed, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	if err := writePEM(c.CACert, "CERTIFICATE", caDER, 0o644); err != nil {
		return nil, err
	}
	if err := writeKey(c.CAKey, caKey); err != nil {
		return nil, err
	}
	// Server cert file holds the leaf followed by the CA (a full chain).
	chain := append(pemBytes("CERTIFICATE", srvDER), pemBytes("CERTIFICATE", caDER)...)
	if err := os.WriteFile(c.Cert, chain, 0o644); err != nil {
		return nil, err
	}
	if err := writeKey(c.Key, srvKey); err != nil {
		return nil, err
	}
	return c, nil
}

// TLSConfig loads the server certificate.
func (c *Certs) TLSConfig() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// CAPool returns a cert pool containing the Nimbus CA (used by tests).
func (c *Certs) CAPool() (*x509.CertPool, error) {
	data, err := os.ReadFile(c.CACert)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("could not parse CA certificate")
	}
	return pool, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func pemBytes(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pemBytes(typ, der), mode)
}

func writeKey(path string, k *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return err
	}
	return writePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
