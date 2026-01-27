package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/elazarl/goproxy"
)

func LoadOrGenerateCA(certDir string) error {
	certPath := filepath.Join(certDir, "ca.crt")
	keyPath := filepath.Join(certDir, "ca.key")

	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		// Generate new CA
		return generateCA(certPath, keyPath)
	}

	// Load existing CA
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}

	// Parse leaf to get x509.Certificate
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}

	// Set goproxy global CA
	goproxy.GoproxyCa = tls.Certificate{
		Certificate: [][]byte{x509Cert.Raw},
		PrivateKey:  cert.PrivateKey,
		Leaf:        x509Cert,
	}
	// goproxy uses the leaf field for SNI during signing if available?
	// The library's default signHost uses GoproxyCa.PrivateKey directly to sign.

	return nil
}

func generateCA(certPath, keyPath string) error {
	// 1. Create Private Key
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// 2. Create Template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Cloud ProxyPool CA"},
			CommonName:   "Cloud ProxyPool Root CA",
		},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * 10 * time.Hour), // 10 years

		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// 3. Self-sign
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	// 4. Save to files
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return err
	}

	// Save Cert
	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()

	// Save Key
	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	privBytes := x509.MarshalPKCS1PrivateKey(priv)
	pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes})
	keyOut.Close()

	return nil
}
