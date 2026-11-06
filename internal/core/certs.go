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

// certValidity is how long the server certificate is valid for. See the long
// comment in EnsureCerts: Apple caps the validity span of TLS server
// certificates, and a day of margin under the documented 398 keeps us clear of
// rounding and clock-skew arguments.
const certValidity = 397 * 24 * time.Hour

// baseDNSNames is what every Nimbus certificate covers, whatever clouds are
// running. A cloud whose clients address data planes by hostname adds its
// own names on top -- see internal/azure/dns.go, which is where the storage
// and Key Vault suffixes live, because they are Azure's business and not
// this package's.
//
// A wildcard matches exactly ONE label: "*.localhost" covers "a.localhost"
// but NOT "a.b.localhost", which is why a cloud with nested data-plane names
// has to list each level.
var baseDNSNames = []string{
	"localhost",
	"*.localhost",
	"nimbus",
	"host.docker.internal",
}

// coversAllNames reports whether an existing certificate carries every name
// it should. Adding a name must reissue certificates that predate it, or an
// upgrading user gets an opaque TLS failure.
func coversAllNames(crt *x509.Certificate, want []string) bool {
	have := make(map[string]bool, len(crt.DNSNames))
	for _, n := range crt.DNSNames {
		have[n] = true
	}
	for _, name := range want {
		if !have[name] {
			return false
		}
	}
	return true
}

// dnsNames is the full set for a certificate: the base names plus whatever
// the running clouds need, deduplicated and in a stable order.
func dnsNames(extra []string) []string {
	out := append([]string(nil), baseDNSNames...)
	seen := map[string]bool{}
	for _, n := range out {
		seen[n] = true
	}
	for _, n := range extra {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

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
// extraNames are the data-plane hostnames the running clouds need covered,
// on top of the base set; see internal/azure/dns.go for Azure's.
func EnsureCerts(dir string, extraNames ...string) (*Certs, error) {
	names := dnsNames(extraNames)
	c := &Certs{
		Dir:    dir,
		CACert: filepath.Join(dir, "nimbus-ca.pem"),
		CAKey:  filepath.Join(dir, "nimbus-ca-key.pem"),
		Cert:   filepath.Join(dir, "nimbus-server.pem"),
		Key:    filepath.Join(dir, "nimbus-server-key.pem"),
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// 1. Certificate authority (10 years). Loaded if we already have one: it is
	// trusted in the OS keychain, so reissuing it would break every client
	// until the user ran the `nimbus cert` trust command again.
	caParsed, caKey, caDER, err := c.loadOrCreateCA()
	if err != nil {
		return nil, err
	}

	// Nothing more to do if the server certificate is present and not close to
	// expiring. The CA is deliberately not part of this check.
	if fileExists(c.Cert) && fileExists(c.Key) {
		if crt, err := readCert(c.Cert); err == nil && !needsReissue(crt.NotAfter) && spanOK(crt) && coversAllNames(crt, names) {
			return c, nil
		}
	}

	// 2. Server certificate for localhost.
	//
	// Validity is deliberately short. Apple's Security.framework refuses a TLS
	// server certificate whose validity SPAN is too long, and reports it as
	//
	//	x509: "localhost" certificate is not standards compliant
	//
	// which says nothing about dates. Two traps here, both of which bit us:
	//
	//  1. The limit is on NotAfter-NotBefore, not on time from now. Backdating
	//     NotBefore for clock skew spends part of the budget, so NotAfter must
	//     be computed from NotBefore — not from time.Now().
	//  2. macOS 27 enforces 825 days (824 passes, 825 fails), but Apple
	//     *documents* 398 days for certificates issued after 2020-09-01. We use
	//     the documented figure so a future tightening cannot break Nimbus.
	//
	// Cost: regenerate roughly yearly by deleting the certs directory and
	// re-trusting the new CA.
	//
	// The span is added as a plain duration in UTC, not with AddDate: AddDate
	// works in the local zone, so crossing a daylight-saving boundary shifts
	// the span by an hour and can push it over a limit you thought you met.
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	notBefore := time.Now().UTC().Add(-time.Hour) // tolerate a slightly slow client clock
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "localhost", Organization: []string{"Nimbus"}},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caParsed, &srvKey.PublicKey, caKey)
	if err != nil {
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

// renewBefore is how long before expiry the server certificate is reissued, so
// a long-running Nimbus never serves an expired certificate.
const renewBefore = 30 * 24 * time.Hour

func needsReissue(notAfter time.Time) bool {
	return time.Now().Add(renewBefore).After(notAfter)
}

// spanOK reports whether an existing certificate's validity span is one macOS
// will accept. Nimbus used to issue 825-day certificates, which Apple's
// verifier rejects; this reissues those on the next start instead of leaving
// the user with an opaque TLS error and no idea that deleting a file fixes it.
func spanOK(crt *x509.Certificate) bool {
	return crt.NotAfter.Sub(crt.NotBefore) <= certValidity
}

// loadOrCreateCA returns the existing CA if one is on disk, otherwise creates
// and writes a new one. Returning the DER too lets the caller build the chain
// file without re-encoding.
func (c *Certs) loadOrCreateCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	if fileExists(c.CACert) && fileExists(c.CAKey) {
		crt, err := readCert(c.CACert)
		if err != nil {
			return nil, nil, nil, err
		}
		key, err := readECKey(c.CAKey)
		if err != nil {
			return nil, nil, nil, err
		}
		return crt, key, crt.Raw, nil
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	notBefore := time.Now().UTC().Add(-time.Hour)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Nimbus Local CA", Organization: []string{"Nimbus"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writePEM(c.CACert, "CERTIFICATE", caDER, 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := writeKey(c.CAKey, caKey); err != nil {
		return nil, nil, nil, err
	}
	caParsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, err
	}
	return caParsed, caKey, caDER, nil
}

// readCert reads the FIRST certificate out of a PEM file. The server cert file
// is a chain (leaf then CA), so this returns the leaf.
func readCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, errors.New("not a PEM certificate: " + path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func readECKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, errors.New("not a PEM key: " + path)
	}
	return x509.ParseECPrivateKey(blk.Bytes)
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
