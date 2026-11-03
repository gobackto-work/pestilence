package tenant

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Token material is split across three objects, and the split is the security
// design rather than an accident:
//
//	tokenKeySecret        control plane namespace   the private key; NEVER in a tenant namespace
//	rootTokenSecret       tenant namespace          the token the root agent presents
//	tokenPubkeyConfigMap  platform namespace        the public key the broker verifies with
//
// A public key is not secret, so it is a ConfigMap; a private key is, so it never
// leaves the control plane.

// controlPlaneMeta addresses an object in the control plane's own namespace.
//
// Deliberately does NOT set a component label: the contract enumerates the
// component values (root-agent | worker | broker), and inventing a fourth on one
// side is exactly the drift §17 warns about. The workspace label is enough to
// find it.
func (s Spec) controlPlaneMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: s.ControlPlaneNamespace,
		Labels: map[string]string{
			LabelWorkspace: s.Slug,
			LabelManagedBy: ManagedByValue,
		},
	}
}

// TokenKeySecret renders the signing-key Secret with the given material.
//
// Exported because the reconciler must create this Secret BEFORE it can render
// the rest of the bundle: the token and the public key both come from the stored
// key, so the material has to exist first.
func (s Spec) TokenKeySecret(m SigningMaterial) *corev1.Secret {
	withMaterial := s.Normalized()
	withMaterial.Material = m
	return withMaterial.tokenKeySecret()
}

// tokenKeySecret holds the workspace's signing key and the token it minted.
//
// Class: boundary, and it is a CREDENTIAL. It lives in the control plane's
// namespace so that the private key is never readable from inside a workspace,
// and teardown must delete it — a signing key that outlives its workspace is
// exactly the residue Invariant 9 forbids.
//
// The data is empty when the bundle is rendered for a delete plan; only the name
// and namespace matter there.
func (s Spec) tokenKeySecret() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: s.controlPlaneMeta(s.TokenKeySecretName()),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenPrivateKeyKey: []byte(s.Material.PrivateKeyPEM),
			TokenDataKey:       []byte(s.Material.Token),
			TokenReportKey:     []byte(s.Material.ReportToken),
			// The broker's certificate and key live here too, so that ONE object holds
			// everything the workspace's authority derives from and teardown deletes it
			// in one step. They are rendered into the platform namespace from here, so
			// the private key is never readable from inside a workspace.
			TLSKeyCert: []byte(s.Material.TLSCertPEM),
			TLSKeyKey:  []byte(s.Material.TLSKeyPEM),
		},
	}
}

// rootTokenSecret carries the capability token mounted into the root agent.
//
// A Secret rather than a file on /workspace, because /workspace is shared with
// every worker and any of them could read it there (§5.8).
func (s Spec) rootTokenSecret() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: s.meta(s.TokenSecretName, ComponentRoot),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenDataKey: []byte(s.Material.Token),
		},
	}
}

// tokenPubkeyConfigMap publishes the token VERIFICATION key to the broker.
//
// A compromised broker cannot forge a token with this, and a leaked public key
// exposes nothing. The broker mounts it read-only and refuses to start without
// it.
func (s Spec) tokenPubkeyConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: s.platformMeta(s.TokenPubkeyConfigMapName()),
		Data: map[string]string{
			TokenPublicKeyKey: s.Material.PublicKeyPEM,
		},
	}
}

// reportTokenSecret carries the broker's reporting token, in the platform namespace
// beside the broker.
//
// Class: platform, and it is a CREDENTIAL. It is rendered from the material stored in the
// control plane's namespace, so the token has one authoritative home and this is a copy
// that teardown removes along with the broker.
//
// It is NOT the capability token, and that is the point of it. The broker reports run
// state and does nothing else: this token names the control plane as its only audience and
// carries the reporting role, while the capability token names the broker and carries the
// orchestration role. Neither is accepted where the other belongs. The two also have
// different holders in different namespaces, so one compromise is not the other.
func (s Spec) reportTokenSecret() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: s.platformMeta(s.ReportTokenSecretName()),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenDataKey: []byte(s.Material.ReportToken),
		},
	}
}

// brokerTLSSecret carries the broker's certificate and key, in the platform namespace
// beside the broker itself.
//
// Class: platform, and it is a CREDENTIAL. It is rendered from the material stored in the
// control plane's namespace, so the private key has exactly one authoritative home and
// this is a copy that teardown removes along with the broker.
//
// Opaque rather than kubernetes.io/tls, because that type is validated to contain
// tls.crt and tls.key and this also carries ca.crt. The extra key is what the agent needs,
// and the certificate is its own CA -- see NewBrokerTLS for why that is the right shape
// for a per-workspace trust anchor.
//
// Emitted whether or not BrokerTLS is switched on, so scarab has something to develop
// against and, more importantly, so the Secret appears in the DELETE plan. Its mere
// presence changes no behaviour: the mounts, the environment variables and the URL scheme
// are all gated on Spec.BrokerTLS.
func (s Spec) brokerTLSSecret() *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: s.platformMeta(s.BrokerTLSSecretName()),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TLSKeyCert: []byte(s.Material.TLSCertPEM),
			TLSKeyKey:  []byte(s.Material.TLSKeyPEM),
			TLSKeyCA:   []byte(s.Material.TLSCertPEM),
		},
	}
}
