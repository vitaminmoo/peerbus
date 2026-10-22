package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"time"
)

// selfCert wraps an Ed25519 key in a self-signed certificate. Nothing checks
// the certificate itself: peers pin the key, and TLS 1.3's CertificateVerify
// proves possession of it.
func selfCert(key ed25519.PrivateKey) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().Add(100 * 365 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func certKey(raw []byte) (ed25519.PublicKey, error) {
	c, err := x509.ParseCertificate(raw)
	if err != nil {
		return nil, err
	}
	pub, ok := c.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("peer key is not Ed25519")
	}
	return pub, nil
}

func pinned(allow func(ed25519.PublicKey) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no peer certificate")
		}
		pub, err := certKey(raw[0])
		if err != nil {
			return err
		}
		if allow == nil || !allow(pub) {
			return errors.New("peer key is not pinned")
		}
		return nil
	}
}

func serverTLS(cert tls.Certificate, allow func(ed25519.PublicKey) bool) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: pinned(allow),
		NextProtos:            []string{alpn},
		MinVersion:            tls.VersionTLS13,
	}
}

func clientTLS(cert tls.Certificate, allow func(ed25519.PublicKey) bool) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		InsecureSkipVerify:    true, // replaced by the pinned-key check
		VerifyPeerCertificate: pinned(allow),
		NextProtos:            []string{alpn},
		ServerName:            "peerbus",
		MinVersion:            tls.VersionTLS13,
	}
}

func peerKey(cs tls.ConnectionState) ed25519.PublicKey {
	if len(cs.PeerCertificates) == 0 {
		return nil
	}
	pub, _ := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	return pub
}
