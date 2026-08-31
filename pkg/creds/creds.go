// Package creds generates the credentials that secure a single ktunnel run:
// a throwaway CA, a server certificate signed by it, and a bearer token.
//
// Nothing here touches the filesystem or the cluster. A Bundle lives for as
// long as the process does, which is why a SIGKILL leaves no credential
// material behind for anyone to find later.
package creds

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// validity bounds a bundle to a working day. A ktunnel run outliving this
// should be restarted rather than carry credentials of indefinite age.
const validity = 24 * time.Hour

// tokenBytes is the size of the bearer token before base64 encoding.
const tokenBytes = 32

// Bundle is the credential material for one run. The client keeps CACert and
// Token; ServerCert, ServerKey and Token are what reach the pod.
type Bundle struct {
	CACert     []byte
	ServerCert []byte
	ServerKey  []byte
	Token      string
}

// Generate produces a fresh bundle for a tunnel server that will be reached
// as name.namespace.svc in the cluster and as loopback through the
// port-forward.
func Generate(name, namespace string) (*Bundle, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}
	caTemplate, err := certTemplate("ktunnel-ca")
	if err != nil {
		return nil, err
	}
	caTemplate.IsCA = true
	caTemplate.BasicConstraintsValid = true
	caTemplate.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("signing CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parsing generated CA certificate: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating server key: %w", err)
	}
	leafTemplate, err := certTemplate(name)
	if err != nil {
		return nil, err
	}
	leafTemplate.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	leafTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	// The client dials 127.0.0.1 through the port-forward, so loopback has
	// to be in here or hostname verification rejects a certificate that is
	// otherwise perfectly good. The service name covers a caller inside the
	// cluster reaching the Service directly.
	leafTemplate.DNSNames = []string{"localhost", fmt.Sprintf("%s.%s.svc", name, namespace)}
	leafTemplate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("signing server certificate: %w", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, fmt.Errorf("marshalling server key: %w", err)
	}

	token := make([]byte, tokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generating token: %w", err)
	}

	return &Bundle{
		CACert:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		ServerCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		ServerKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
		Token:      base64.RawURLEncoding.EncodeToString(token),
	}, nil
}

// certTemplate is the shared skeleton for both certificates: a random serial
// and the same validity window.
func certTemplate(commonName string) (*x509.Certificate, error) {
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial: %w", err)
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		// A minute of backdating, so a pod whose clock trails the laptop's
		// does not reject a certificate issued moments earlier.
		NotBefore: now.Add(-time.Minute),
		NotAfter:  now.Add(validity),
	}, nil
}
