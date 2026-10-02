package tenant

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The certificate has to actually verify for the name the bridge dials. A SAN that does
// not match produces a TLS failure that reads like a network fault, and it is the kind of
// thing that only shows up at runtime -- so it is asserted here instead.
func TestBrokerTLSCertificateVerifiesForItsOwnNames(t *testing.T) {
	spec := fullSpec()
	now := time.Now()

	m, err := NewBrokerTLS(spec, now)
	if err != nil {
		t.Fatalf("NewBrokerTLS: %v", err)
	}
	block, _ := pem.Decode([]byte(m.CertPEM))
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	// The certificate is its own CA, so the pool is the certificate. This is the whole
	// trust model in one line: the agent pins exactly one certificate.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(m.CertPEM)) {
		t.Fatal("certificate cannot be used as a root")
	}

	for _, name := range []string{
		spec.BrokerTLSDNSName(),
		spec.BrokerServiceName() + "." + spec.PlatformNamespace,
		spec.BrokerServiceName(),
	} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, CurrentTime: now}); err != nil {
			t.Errorf("certificate does not verify for %q: %v", name, err)
		}
	}

	// And a name it was NOT issued for must fail, or the SAN list means nothing.
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "evil.example", Roots: roots, CurrentTime: now}); err == nil {
		t.Error("certificate verified for a name it was not issued for")
	}
}

// A certificate that expires mid-workspace is a workspace that stops working with no
// warning, so the lifetime is asserted rather than assumed.
func TestBrokerTLSCertificateOutlivesAnyPlausibleWorkspace(t *testing.T) {
	spec := fullSpec()
	now := time.Now()
	m, err := NewBrokerTLS(spec, now)
	if err != nil {
		t.Fatalf("NewBrokerTLS: %v", err)
	}
	block, _ := pem.Decode([]byte(m.CertPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if !cert.NotAfter.After(now.Add(365 * 24 * time.Hour)) {
		t.Errorf("certificate expires %s, which is inside a year", cert.NotAfter.Sub(now))
	}
	// Backdated, so a clock a few minutes behind still validates.
	if !cert.NotBefore.Before(now) {
		t.Error("certificate is not backdated; a clock behind would reject it")
	}
}

// Off by default, and the scheme has to follow the switch. A bridge told to use https
// without a CA fails every call, and one left on http against a TLS broker never
// connects -- so these two facts are asserted together.
func TestBrokerTLSIsOffUnlessSwitchedOn(t *testing.T) {
	spec := fullSpec()
	if spec.BrokerTLS {
		t.Fatal("fullSpec must leave BrokerTLS off, or the default path is untested")
	}

	_, d := findClassifiedDeployment(t, fullBundle(t), NameRootDeployment)
	env := d.Spec.Template.Spec.Containers[0].Env
	if _, ok := envValueOK(env, EnvBrokerCA); ok {
		t.Errorf("%s is set with BrokerTLS off", EnvBrokerCA)
	}
	if got := envValue(t, env, "SCARAB_BROKER_URL"); got[:5] != "http:" {
		t.Errorf("SCARAB_BROKER_URL = %q, want the plaintext scheme", got)
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == nameBrokerCA {
			t.Error("the broker CA volume is mounted with BrokerTLS off")
		}
	}
}

// The mount is the boundary: the certificate and the private key live in one Secret, and
// the agent must only ever see the certificate.
func TestBrokerTLSGivesTheAgentTheCAAndNotTheKey(t *testing.T) {
	spec := fullSpec()
	spec.BrokerTLS = true
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}

	_, d := findClassifiedDeployment(t, objs, NameRootDeployment)
	var caVolume *corev1.Volume
	for i := range d.Spec.Template.Spec.Volumes {
		if d.Spec.Template.Spec.Volumes[i].Name == nameBrokerCA {
			caVolume = &d.Spec.Template.Spec.Volumes[i]
		}
	}
	if caVolume == nil {
		t.Fatal("the root agent has no broker CA volume")
	}
	// A ConfigMap, and NOT the Secret. Two independent traps, and this asserts against
	// both: a Secret volume reference is namespace-local, so pointing the agent at the
	// platform namespace's Secret fails to MOUNT and leaves the pod in ContainerCreating;
	// and a plain secretName mount would hand the agent the broker's private key.
	if caVolume.Secret != nil {
		t.Fatalf("the agent's CA volume is a Secret (%q); it must be a ConfigMap, because "+
			"a Secret volume reference is namespace-local and the broker's Secret lives in "+
			"the platform namespace", caVolume.Secret.SecretName)
	}
	if caVolume.ConfigMap == nil {
		t.Fatal("the broker CA volume is neither a ConfigMap nor a Secret")
	}
	if caVolume.ConfigMap.Name != spec.BrokerCAName() {
		t.Errorf("CA volume references %q, want %q", caVolume.ConfigMap.Name, spec.BrokerCAName())
	}
	if len(caVolume.ConfigMap.Items) != 1 || caVolume.ConfigMap.Items[0].Key != TLSKeyCA {
		t.Fatalf("the CA volume projects %+v, want ca.crt alone -- the private key is the "+
			"other half and the mount is what keeps it out of the agent", caVolume.ConfigMap.Items)
	}

	env := d.Spec.Template.Spec.Containers[0].Env
	if got := envValue(t, env, EnvBrokerCA); got != MountBrokerCA+"/"+TLSKeyCA {
		t.Errorf("%s = %q, want the mounted ca.crt", EnvBrokerCA, got)
	}
	if got := envValue(t, env, "SCARAB_BROKER_URL"); got[:6] != "https:" {
		t.Errorf("SCARAB_BROKER_URL = %q, want the TLS scheme", got)
	}
}

// The broker serves the certificate and reads the key from the same mount.
func TestBrokerTLSGivesTheBrokerItsCertificate(t *testing.T) {
	spec := fullSpec()
	spec.BrokerTLS = true
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}

	_, d := findClassifiedDeployment(t, objs, spec.BrokerServiceName())
	var found bool
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == nameBrokerTLS {
			found = true
			if v.Secret == nil || v.Secret.SecretName != spec.BrokerTLSSecretName() {
				t.Errorf("broker TLS volume does not reference %q", spec.BrokerTLSSecretName())
			}
		}
	}
	if !found {
		t.Fatal("the broker has no TLS volume")
	}

	var mounted bool
	for _, mnt := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mnt.Name == nameBrokerTLS {
			mounted = true
			if mnt.MountPath != MountBrokerTLS || !mnt.ReadOnly {
				t.Errorf("broker TLS mount is %q readOnly=%v, want %q read-only", mnt.MountPath, mnt.ReadOnly, MountBrokerTLS)
			}
		}
	}
	if !mounted {
		t.Fatal("the broker does not mount its TLS volume")
	}

	env := d.Spec.Template.Spec.Containers[0].Env
	if got := envValue(t, env, EnvBrokerTLSCert); got != MountBrokerTLS+"/"+TLSKeyCert {
		t.Errorf("%s = %q", EnvBrokerTLSCert, got)
	}
	if got := envValue(t, env, EnvBrokerTLSKey); got != MountBrokerTLS+"/"+TLSKeyKey {
		t.Errorf("%s = %q", EnvBrokerTLSKey, got)
	}
}

// The CA has to be reachable from the tenant namespace, and the only object types that can
// be are ones that live there. This is the assertion that would have caught the mount
// failure: the agent pod sat in ContainerCreating with "secret not found" because the
// volume pointed at the platform namespace.
func TestBrokerCAIsPublishedIntoTheTenantNamespace(t *testing.T) {
	spec := fullSpec()
	spec.BrokerTLS = true
	// The reconciler always populates the material before rendering a bundle, so a
	// bundle with an agent Deployment and no certificate cannot occur in practice.
	spec.Material.TLSCertPEM = "CERT-SENTINEL"
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		cm, ok := c.Object.(*corev1.ConfigMap)
		if !ok || cm.Name != spec.BrokerCAName() {
			continue
		}
		if cm.Namespace != spec.Namespace() {
			t.Errorf("CA ConfigMap is in %q, want the tenant namespace %q -- the agent "+
				"cannot mount an object from another namespace", cm.Namespace, spec.Namespace())
		}
		if cm.Data[TLSKeyCA] == "" {
			t.Error("CA ConfigMap has no ca.crt data")
		}
		return
	}
	t.Fatalf("ConfigMap %q is not in the bundle", spec.BrokerCAName())
}

// And the private key must NOT be published into the tenant namespace by any route.
func TestBrokerPrivateKeyNeverReachesTheTenantNamespace(t *testing.T) {
	spec := fullSpec()
	spec.BrokerTLS = true
	spec.Material.TLSKeyPEM = "PRIVATE-KEY-SENTINEL"
	objs, err := BundleClassified(spec)
	if err != nil {
		t.Fatalf("BundleClassified: %v", err)
	}
	for _, c := range objs {
		acc, ok := c.Object.(metav1.Object)
		if !ok || acc.GetNamespace() != spec.Namespace() {
			continue
		}
		if cm, ok := c.Object.(*corev1.ConfigMap); ok {
			for k, v := range cm.Data {
				if v == spec.Material.TLSKeyPEM {
					t.Errorf("ConfigMap %q carries the broker private key under %q", cm.Name, k)
				}
			}
		}
		if sec, ok := c.Object.(*corev1.Secret); ok {
			for k, v := range sec.Data {
				if string(v) == spec.Material.TLSKeyPEM {
					t.Errorf("Secret %q carries the broker private key under %q", sec.Name, k)
				}
			}
		}
	}
}
