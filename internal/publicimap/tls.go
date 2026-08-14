package publicimap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadOrCreateTLSConfig loads an operator-provided certificate pair, or
// creates a persistent self-signed pair when generate is true and both files
// are absent. The generated certificate is intended for local installations;
// production deployments can point the same function at their managed pair.
func LoadOrCreateTLSConfig(certFile, keyFile, serverName string, generate bool) (*tls.Config, bool, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	serverName = strings.TrimSpace(serverName)
	if certFile == "" || keyFile == "" {
		return nil, false, errors.New("IMAPS certificate and key paths are required")
	}
	if serverName == "" || strings.ContainsAny(serverName, " \t\r\n") {
		return nil, false, errors.New("IMAPS server name must be one non-empty token")
	}

	certExists, err := regularFileExists(certFile)
	if err != nil {
		return nil, false, fmt.Errorf("inspect IMAPS certificate: %w", err)
	}
	keyExists, err := regularFileExists(keyFile)
	if err != nil {
		return nil, false, fmt.Errorf("inspect IMAPS private key: %w", err)
	}
	created := false
	if !certExists || !keyExists {
		if !generate {
			return nil, false, fmt.Errorf("configured IMAPS certificate pair is incomplete")
		}
		if certExists != keyExists {
			return nil, false, fmt.Errorf("generated IMAPS certificate pair is incomplete; restore or remove both files")
		}
		if err := generateSelfSignedPair(certFile, keyFile, serverName, time.Now().UTC()); err != nil {
			return nil, false, err
		}
		created = true
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, false, fmt.Errorf("load IMAPS certificate pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, false, errors.New("IMAPS certificate pair contains no certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, false, fmt.Errorf("parse IMAPS leaf certificate: %w", err)
	}
	if time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return nil, false, fmt.Errorf("IMAPS certificate is outside its validity period")
	}
	if err := leaf.VerifyHostname(serverName); err != nil {
		return nil, false, fmt.Errorf("IMAPS certificate does not cover %q: %w", serverName, err)
	}
	pair.Leaf = leaf
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}, created, nil
}

func regularFileExists(filename string) (bool, error) {
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%q is not a regular file", filename)
	}
	return true, nil
}

func generateSelfSignedPair(certFile, keyFile, serverName string, now time.Time) error {
	if filepath.Clean(certFile) == filepath.Clean(keyFile) {
		return errors.New("IMAPS certificate and private key paths must differ")
	}
	for _, filename := range []string{certFile, keyFile} {
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			return fmt.Errorf("create IMAPS certificate directory: %w", err)
		}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate IMAPS private key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate IMAPS certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serverName, Organization: []string{"icloud-api local IMAPS"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(serverName); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{serverName}
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create IMAPS certificate: %w", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("encode IMAPS private key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
	if err := writeExclusive(keyFile, keyPEM, 0o600); err != nil {
		return fmt.Errorf("persist IMAPS private key: %w", err)
	}
	if err := writeExclusive(certFile, certPEM, 0o644); err != nil {
		_ = os.Remove(keyFile)
		return fmt.Errorf("persist IMAPS certificate: %w", err)
	}
	return nil
}

func writeExclusive(filename string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(contents)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(filename)
		return err
	}
	return nil
}
