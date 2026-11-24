package keyvault

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"time"
)

// Issuing a certificate: the crypto/x509 half, kept apart from the HTTP half
// so neither file runs past this project's 500-line limit.
//
// Everything here is the standard library. There is no vendor image to lean
// on and none is needed, which makes certificates the opposite problem from
// Azure Functions -- all the difficulty is protocol fidelity rather than
// getting somebody else's runtime to start.

// selfSign issues the certificate, signed with its own key.
func selfSign(ver *KeyVersion, pol certPolicy) ([]byte, error) {
	subject := pol.X509Props.Subject
	if subject == "" {
		subject = "CN=nimbus"
	}
	months := pol.X509Props.ValidityMonths
	if months == 0 {
		months = 12
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      parseDN(subject),
		NotBefore:    now.Add(-5 * time.Minute),
		// Computed as a plain duration in UTC rather than with AddDate,
		// which works in the local zone and shifts by an hour across a
		// daylight-saving boundary -- the trap the server certificates
		// already record.
		NotAfter:              now.Add(time.Duration(months) * 30 * 24 * time.Hour),
		KeyUsage:              keyUsageFrom(pol.X509Props.KeyUsage),
		BasicConstraintsValid: true,
		DNSNames:              pol.X509Props.Sans.DNSNames,
		EmailAddresses:        pol.X509Props.Sans.Emails,
	}
	for _, e := range pol.X509Props.Ekus {
		switch e {
		case "1.3.6.1.5.5.7.3.1":
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		case "1.3.6.1.5.5.7.3.2":
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}
	}
	pub := ver.Public()
	priv := any(ver.RSA)
	if ver.EC != nil {
		priv = ver.EC
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
}

// parseDN reads the handful of RDN types a certificate policy realistically
// carries. A full RFC 4514 parser is not warranted: azurerm sends a subject
// its own schema validated, and anything unrecognised lands in the CN so the
// certificate still has a usable name rather than being rejected.
func parseDN(dn string) pkix.Name {
	var out pkix.Name
	for _, part := range strings.Split(dn, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "CN":
			out.CommonName = val
		case "O":
			out.Organization = append(out.Organization, val)
		case "OU":
			out.OrganizationalUnit = append(out.OrganizationalUnit, val)
		case "C":
			out.Country = append(out.Country, val)
		case "L":
			out.Locality = append(out.Locality, val)
		case "ST":
			out.Province = append(out.Province, val)
		default:
			if out.CommonName == "" {
				out.CommonName = val
			}
		}
	}
	if out.CommonName == "" {
		out.CommonName = dn
	}
	return out
}

func keyUsageFrom(names []string) x509.KeyUsage {
	var ku x509.KeyUsage
	for _, n := range names {
		switch n {
		case "digitalSignature":
			ku |= x509.KeyUsageDigitalSignature
		case "keyEncipherment":
			ku |= x509.KeyUsageKeyEncipherment
		case "dataEncipherment":
			ku |= x509.KeyUsageDataEncipherment
		case "keyAgreement":
			ku |= x509.KeyUsageKeyAgreement
		case "keyCertSign":
			ku |= x509.KeyUsageCertSign
		case "cRLSign":
			ku |= x509.KeyUsageCRLSign
		case "nonRepudiation", "contentCommitment":
			ku |= x509.KeyUsageContentCommitment
		}
	}
	if ku == 0 {
		ku = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}
	return ku
}

// pemBundle is the certificate and its private key, which is what the SECRET
// under a certificate returns.
func pemBundle(ver *KeyVersion, der []byte) (string, error) {
	var priv any
	switch {
	case ver.RSA != nil:
		priv = ver.RSA
	case ver.EC != nil:
		priv = ver.EC
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := pem.Encode(&b, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		return "", err
	}
	if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return "", err
	}
	return b.String(), nil
}
