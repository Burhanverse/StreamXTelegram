package server

import (
	"crypto/tls"
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed certs/fullchain.pem
var defaultCertPEM []byte

//go:embed certs/privkey.pem
var defaultKeyPEM []byte

func loadCertificate(certsDir string) (*tls.Certificate, error) {
	if certsDir != "" {
		certFile := filepath.Join(certsDir, "fullchain.pem")
		keyFile := filepath.Join(certsDir, "privkey.pem")
		if _, err1 := os.Stat(certFile); err1 == nil {
			if _, err2 := os.Stat(keyFile); err2 == nil {
				cert, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err == nil {
					return &cert, nil
				}
			}
		}
	}
	if len(defaultCertPEM) > 0 && len(defaultKeyPEM) > 0 {
		cert, err := tls.X509KeyPair(defaultCertPEM, defaultKeyPEM)
		if err == nil {
			return &cert, nil
		}
		return nil, err
	}
	return nil, os.ErrNotExist
}

