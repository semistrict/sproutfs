package testnet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
)

// Authority issues the certificates a test's hosts present, the way a
// deployment's own certificate authority would.
type Authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pool        *x509.CertPool
}

// NewAuthority is a certificate authority of its own, which no other trusts.
func NewAuthority(name string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := certificateTemplate(name)
	template.IsCA, template.BasicConstraintsValid = true, true
	template.KeyUsage = x509.KeyUsageCertSign
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &Authority{certificate: certificate, key: key, pool: pool}, nil
}

// issue is a certificate for one loopback host, good for both ends of a
// connection.
func (a *Authority) issue(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := certificateTemplate(name)
	template.KeyUsage = x509.KeyUsageDigitalSignature
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	template.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func certificateTemplate(name string) *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:   pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
}

// MutualTLS is mutual TLS over plain TCP. It presents a certificate identity
// issued and admits only a peer whose certificate trusted issued, whichever end
// dialed. refused is called with every peer it turns away, and why.
//
// It checks the chain and nothing else, which is a host of the deployment and
// all a page server needs: which VM a peer may read is the handoff's business.
func MutualTLS(name string, identity, trusted *Authority, refused func(error)) (platform.Transport, error) {
	certificate, err := identity.issue(name)
	if err != nil {
		return nil, err
	}
	verify := func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			err := errors.New("testnet: the peer presented no certificate")
			refused(err)
			return err
		}
		intermediates := x509.NewCertPool()
		for _, certificate := range state.PeerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: trusted.pool,
			Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			err = fmt.Errorf("testnet: %s refused %s: %w", name, state.PeerCertificates[0].Subject.CommonName, err)
			refused(err)
			return err
		}
		return nil
	}
	return &mutualTLS{config: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// Both ends verify the chain themselves, in verify, so that a refusal
		// is reported whichever end made it.
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true,
		VerifyConnection:   verify,
	}}, nil
}

type mutualTLS struct{ config *tls.Config }

func (m *mutualTLS) Listen(address platform.Address) (net.Listener, error) {
	listener, err := adapters.TCP().Listen(address)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(listener, m.config), nil
}

func (m *mutualTLS) Dial(ctx context.Context, address platform.Address) (net.Conn, error) {
	raw, err := adapters.TCP().Dial(ctx, address)
	if err != nil {
		return nil, err
	}
	connection := tls.Client(raw, m.config)
	if err := connection.HandshakeContext(ctx); err != nil {
		return nil, errors.Join(err, raw.Close())
	}
	return connection, nil
}
