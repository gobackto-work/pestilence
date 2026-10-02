package tenant

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Broker TLS material.
//
// These names are a CONTRACT with scarab. The Secret keys, the mount paths and the
// environment variables are all read on their side, so changing one here without
// changing it there produces a broker that cannot start or a bridge that cannot verify
// -- and the second failure looks like a network problem.
const (
	TLSKeyCert = "tls.crt"
	TLSKeyKey  = "tls.key"
	TLSKeyCA   = "ca.crt"

	// MountBrokerTLS is where the broker finds its own certificate and key.
	MountBrokerTLS = "/var/run/scarab/broker-tls"
	// MountBrokerCA is where the root agent finds the certificate to verify the broker
	// with. A separate mount because the agent must NOT have the broker's private key.
	MountBrokerCA = "/var/run/scarab/broker-ca"

	// Volume names, shared by the builders that emit them and the mounts that use them.
	nameBrokerTLS = "broker-tls"
	nameBrokerCA  = "broker-ca"

	// EnvBrokerTLSCert and EnvBrokerTLSKey are set on the BROKER.
	EnvBrokerTLSCert = "SCARAB_BROKER_TLS_CERT"
	EnvBrokerTLSKey  = "SCARAB_BROKER_TLS_KEY"
	// EnvBrokerCA is set on the ROOT AGENT, and its presence is also the signal to use
	// https rather than http. One variable, so the scheme and the trust anchor cannot
	// disagree -- a bridge that switched to https without a CA would fail every call.
	EnvBrokerCA = "SCARAB_BROKER_CA"

	// BrokerTLSLifetime is deliberately long, and the certificate is minted once
	// rather than rotated. See NewBrokerTLS for why that is the safer choice here.
	BrokerTLSLifetime = 5 * 365 * 24 * time.Hour
)

// TLSMaterial is one workspace's broker certificate and the matching private key.
type TLSMaterial struct {
	CertPEM string
	KeyPEM  string
}

// BrokerTLSDNSName is the fully qualified name the certificate is issued for.
func (s Spec) BrokerTLSDNSName() string {
	return s.BrokerServiceName() + "." + s.PlatformNamespace + ".svc.cluster.local"
}

// BrokerTLSSecretName is the Secret holding the broker's certificate AND PRIVATE KEY, in
// the platform namespace beside the broker itself.
func (s Spec) BrokerTLSSecretName() string { return s.BrokerServiceName() + "-tls" }

// BrokerCAName is the ConfigMap carrying the certificate for the AGENT to trust, in the
// TENANT namespace.
//
// A ConfigMap, and in the tenant namespace, for two separate reasons that both matter:
//
//  1. A Secret volume reference is NAMESPACE-LOCAL. The agent runs in ws-<slug> and the
//     Secret lives in the platform namespace, so pointing the agent's volume at that
//     Secret does not fail to verify -- it fails to MOUNT, and the pod sits in
//     ContainerCreating with "secret not found" until someone reads the events. That is
//     exactly how this was found.
//  2. A certificate is not a secret. Only the private key is, and it stays in the platform
//     namespace. Publishing the certificate where the agent can reach it is the same
//     decision already made for town's assertion key, and for the same reason: the thing
//     that must not move is the private half.
func (s Spec) BrokerCAName() string { return s.BrokerServiceName() + "-ca" }

// brokerCAConfigMap publishes the broker's certificate into the tenant namespace.
//
// Class: boundary, so it is re-applied on the drift sweep and removed with the namespace.
// The data key is the same "ca.crt" the agent's mount and the contract use, so the path
// the bridge is told to trust is the path this object creates.
func (s Spec) brokerCAConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: s.meta(s.BrokerCAName(), ComponentBroker),
		Data:       map[string]string{TLSKeyCA: s.Material.TLSCertPEM},
	}
}

// NewBrokerTLS mints a self-signed certificate for one workspace's broker.
//
// ────────────────────────────────────────────────────────────────────────────
// WHY SELF-SIGNED, AND WHY IT IS THE CA
// ────────────────────────────────────────────────────────────────────────────
//
// A platform-wide CA would mean a long-lived CA private key held by the control plane,
// valid for every workspace, whose compromise forges a broker to every agent. A
// per-workspace self-signed certificate needs no such key: the agent pins exactly one
// certificate, and that certificate only ever speaks for one broker. The isolation
// model is per-namespace, so the trust anchor is too.
//
// The certificate is therefore its own CA and `ca.crt` is the same bytes as `tls.crt`.
// IsCA and KeyUsageCertSign are set so a verifier accepts it as a root -- a self-signed
// leaf in a root pool is accepted by some verifiers and not others, and being explicit
// costs nothing.
//
// ────────────────────────────────────────────────────────────────────────────
// WHY IT IS NOT ROTATED
// ────────────────────────────────────────────────────────────────────────────
//
// The token rotates; this does not, and the asymmetry is deliberate.
//
// The certificate provides CONFIDENTIALITY for an in-cluster hop. The AUTHORIZATION is
// the capability token, which is now short-lived and re-minted on the drift sweep. So a
// leaked certificate key lets someone decrypt traffic they would have to already be able
// to intercept; it does not let them call the broker.
//
// Rotating it would be actively worse. The broker would have to reload its certificate
// and the agent's trust anchor would have to update in lockstep, through two different
// Secret mounts with the same sub-minute propagation delay the token already has. A
// mismatch between them is not a degraded connection, it is a workspace that stops
// working. Trading a bounded, low-value confidentiality key for a new way to break every
// running workspace is a bad trade.
//
// The cost is a dated operational item: these expire in five years, and renewal needs
// the coordinated reload described above. That is written down in
// docs/security-hardening.md rather than left to be discovered.
func NewBrokerTLS(spec Spec, now time.Time) (TLSMaterial, error) {
	dns := spec.BrokerTLSDNSName()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("generate broker tls key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("generate serial: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dns},
		// Backdated so a clock a few minutes behind on either side still validates.
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(BrokerTLSLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		// All three forms, because the bridge may be configured with any of them and a
		// hostname that is not in here fails verification with an error that reads like
		// a network fault.
		DNSNames: []string{
			dns,
			spec.BrokerServiceName() + "." + spec.PlatformNamespace,
			spec.BrokerServiceName(),
		},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("create broker certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return TLSMaterial{}, fmt.Errorf("marshal broker tls key: %w", err)
	}

	return TLSMaterial{
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, nil
}
