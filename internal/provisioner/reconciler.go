// Package provisioner reconciles a workspace's Kubernetes objects.
//
// It is deliberately idempotent: every operation can be retried, and the
// provisioner never assumes that a previous attempt completed. That is what lets
// the control plane recover from a crash mid-provision rather than leaving a
// half-built workspace behind (design §32).
//
// The treatment of an object depends on its tenant.Class — see class.go. The
// short version:
//
//	boundary  reconciled, drift corrected
//	endpoint  reconciled, because reachability is a platform guarantee
//	runtime   created once, then owned by the tenant; never resurrected
package provisioner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// DefaultFieldManager is the server-side-apply field manager. Using a single
// stable name means the provisioner owns exactly the fields it sets, and a
// conflicting writer is detected rather than silently overwritten.
const DefaultFieldManager = "pestilence"

var (
	namespaceGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	secretsGVR   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

// Reconciler applies and removes workspace objects.
type Reconciler struct {
	dyn dynamic.Interface
	fm  string
}

// New returns a Reconciler. An empty fieldManager uses DefaultFieldManager.
func New(dyn dynamic.Interface, fieldManager string) *Reconciler {
	if fieldManager == "" {
		fieldManager = DefaultFieldManager
	}
	return &Reconciler{dyn: dyn, fm: fieldManager}
}

// Ensure makes the workspace match its desired state.
//
// Boundary and endpoint objects are server-side applied, so drift is corrected
// and repeated calls converge. Runtime objects are created only when absent —
// never updated, never recreated — because the tenant owns them once they exist.
//
// Signing material is loaded (or generated once) first, because it cannot be
// derived: a renderer that invented a key on each call would change the public
// key under the broker and invalidate every token.
func (r *Reconciler) Ensure(ctx context.Context, spec tenant.Spec) error {
	material, err := r.ensureSigningMaterial(ctx, spec)
	if err != nil {
		return err
	}
	if material.Empty() {
		return fmt.Errorf("refusing to apply empty signing material for workspace %s", spec.Slug)
	}
	spec.Material = material

	classified, err := tenant.BundleClassified(spec)
	if err != nil {
		return err
	}
	for _, c := range classified {
		var err error
		if c.Class == tenant.ClassRuntime {
			err = r.createIfAbsent(ctx, c.Object)
		} else {
			err = r.apply(ctx, c.Object)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ensureSigningMaterial loads the workspace's signing key, generating it once if
// it does not exist.
//
// The keypair is state, not derivation: it is created on first provision and
// reused forever after. Regenerating it would silently invalidate the token in
// every running root agent, so a stored key is always preferred, and a corrupt
// one is replaced only because there is no safe alternative.
func (r *Reconciler) ensureSigningMaterial(ctx context.Context, spec tenant.Spec) (tenant.SigningMaterial, error) {
	s := spec.Normalized()
	ri := r.dyn.Resource(secretsGVR).Namespace(s.ControlPlaneNamespace)

	existing, err := ri.Get(ctx, s.TokenKeySecretName(), metav1.GetOptions{})
	switch {
	case err == nil:
		material, changed, perr := materialFromSecret(existing, s, time.Now())
		if perr == nil && !material.Empty() {
			// The drift sweep calls Ensure on every RUNNING workspace, so material that
			// has aged, or that predates a field being added, is brought up to date
			// within one sweep without any separate loop. Persisting is what makes that
			// stick: the tenant and platform Secrets are re-rendered from this material
			// on the same pass, and the bridge reads the token per call, so a change is
			// live within a sweep and without a restart.
			if changed {
				if err := r.persistDerivedMaterial(ctx, s, ri, material); err != nil {
					return tenant.SigningMaterial{}, err
				}
			}
			return material, nil
		}
		// Unreadable or partial: fall through and regenerate. The previous token
		// stops working, which is the safe direction.
	case !apierrors.IsNotFound(err):
		return tenant.SigningMaterial{}, fmt.Errorf("get signing key secret: %w", err)
	}

	material, err := tenant.NewSigningMaterial(s, time.Now())
	if err != nil {
		return tenant.SigningMaterial{}, err
	}
	obj := s.TokenKeySecret(material)
	u, objRI, err := r.resource(obj)
	if err != nil {
		return tenant.SigningMaterial{}, err
	}
	_, err = objRI.Create(ctx, u, metav1.CreateOptions{FieldManager: r.fm})
	if err == nil {
		return material, nil
	}
	// Lost a race with another reconciler: use THEIRS, or the token we just minted
	// is signed by a key whose public half is not the one published to the broker.
	if apierrors.IsAlreadyExists(err) {
		if theirs, ok := r.existingSigningMaterial(ctx, s, ri); ok {
			return theirs, nil
		}
	}
	return tenant.SigningMaterial{}, fmt.Errorf("create signing key secret: %w", err)
}

// persistDerivedMaterial writes the freshly minted derived values back to the signing
// Secret: the capability token and the broker's certificate and key.
//
// A MERGE PATCH of those three fields, deliberately neither an update nor an apply, and
// deliberately NOT including the signing private key. An update rewrites the whole object
// and could clobber that key; an apply would make this reconciler a co-owner of a key it
// must never regenerate; and patching only the derived fields means the key is untouched
// by construction rather than by intention.
func (r *Reconciler) persistDerivedMaterial(ctx context.Context, spec tenant.Spec, ri dynamic.ResourceInterface, m tenant.SigningMaterial) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{
		tenant.TokenDataKey: base64.StdEncoding.EncodeToString([]byte(m.Token)),
		tenant.TLSKeyCert:   base64.StdEncoding.EncodeToString([]byte(m.TLSCertPEM)),
		tenant.TLSKeyKey:    base64.StdEncoding.EncodeToString([]byte(m.TLSKeyPEM)),
	}})
	if err != nil {
		return fmt.Errorf("encode material patch: %w", err)
	}
	if _, err := ri.Patch(ctx, spec.TokenKeySecretName(), types.MergePatchType, patch, metav1.PatchOptions{FieldManager: r.fm}); err != nil {
		return fmt.Errorf("persist workspace material: %w", err)
	}
	return nil
}

// existingSigningMaterial re-reads the signing key after losing a create race.
//
// Returns false when the winner's material is unreadable, so the caller reports the
// original create error rather than a second, more confusing one. A rotation decided
// here is intentionally NOT persisted: the winner owns the Secret, and racing it to
// write a token would be the same bug this function exists to avoid.
func (r *Reconciler) existingSigningMaterial(ctx context.Context, spec tenant.Spec, ri dynamic.ResourceInterface) (tenant.SigningMaterial, bool) {
	got, err := ri.Get(ctx, spec.TokenKeySecretName(), metav1.GetOptions{})
	if err != nil {
		return tenant.SigningMaterial{}, false
	}
	material, _, err := materialFromSecret(got, spec, time.Now())
	if err != nil || material.Empty() {
		return tenant.SigningMaterial{}, false
	}
	return material, true
}

// materialFromSecret reads the stored key, derives the public key from it, and mints
// whatever derived material is missing or aged: the capability token when it has passed
// its rotation threshold, and the broker certificate when it has never been minted. The
// public key is derived rather than stored so the two cannot disagree.
//
// The second return value reports whether anything must be written back, so the caller
// can persist it. Nothing here writes: this function has no client, which keeps it
// testable and keeps the decision separate from the side effect.
func materialFromSecret(u *unstructured.Unstructured, spec tenant.Spec, now time.Time) (tenant.SigningMaterial, bool, error) {
	var secret corev1.Secret
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &secret); err != nil {
		return tenant.SigningMaterial{}, false, fmt.Errorf("decode signing key secret: %w", err)
	}
	privPEM := string(secret.Data[tenant.TokenPrivateKeyKey])
	priv, err := tenant.ParsePrivateKey(privPEM)
	if err != nil {
		return tenant.SigningMaterial{}, false, err
	}
	pubPEM, err := tenant.MarshalPublicKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		return tenant.SigningMaterial{}, false, err
	}

	token := string(secret.Data[tenant.TokenDataKey])
	changed := false
	if tenant.TokenNeedsRotation(token, now, spec.TokenTTL) {
		if token, err = tenant.MintToken(spec, priv, now); err != nil {
			return tenant.SigningMaterial{}, false, err
		}
		changed = true
	}

	// The broker certificate is minted ONCE and never rotated -- see NewBrokerTLS for
	// why. Absent means a workspace provisioned before TLS existed, and minting here is
	// what upgrades it rather than leaving it on plaintext forever.
	certPEM := string(secret.Data[tenant.TLSKeyCert])
	keyPEM := string(secret.Data[tenant.TLSKeyKey])
	if certPEM == "" || keyPEM == "" {
		tlsMaterial, terr := tenant.NewBrokerTLS(spec, now)
		if terr != nil {
			return tenant.SigningMaterial{}, false, terr
		}
		certPEM, keyPEM = tlsMaterial.CertPEM, tlsMaterial.KeyPEM
		changed = true
	}

	return tenant.SigningMaterial{
		PrivateKeyPEM: privPEM,
		PublicKeyPEM:  pubPEM,
		Token:         token,
		TLSCertPEM:    certPEM,
		TLSKeyPEM:     keyPEM,
	}, changed, nil
}

// DeletePlan returns the objects to remove, and the order to remove them in.
//
// It is exported so the ordering can be asserted in tests. The order is a
// security property, not an implementation detail: authority is revoked before
// compute is destroyed (design §31).
//
// Note what is absent. Boundary objects in the tenant namespace are NOT in the
// plan, because deleting the namespace removes them. The broker's identity is
// present precisely because it lives *outside* that namespace and would
// otherwise survive the teardown — the mistake that leaves a workspace
// "deleted" while its authority still exists.
func DeletePlan(spec tenant.Spec) ([]tenant.Classified, error) {
	classified, err := tenant.BundleClassified(spec)
	if err != nil {
		return nil, err
	}

	var plan []tenant.Classified
	seen := map[string]bool{}
	collect := func(match func(tenant.Classified) bool) {
		for _, c := range classified {
			if !match(c) {
				continue
			}
			key := objectKey(c.Object)
			if seen[key] {
				continue
			}
			seen[key] = true
			plan = append(plan, c)
		}
	}

	// 1. Stop accepting new traffic: remove the route and the Services.
	collect(func(c tenant.Classified) bool { return c.Class == tenant.ClassEndpoint })

	// 2. Stop the broker, then revoke its identity. Both live in the platform
	//    namespace, so deleting the tenant namespace does NOT remove them -- and
	//    they would survive teardown unnoticed.
	//
	//    The Deployment is ordered before the ServiceAccount deliberately: stop
	//    the compute first, then revoke the credential it was using, so the
	//    broker cannot restart holding authority after teardown began.
	collect(func(c tenant.Classified) bool {
		return c.Class == tenant.ClassPlatform && isPlatformScoped(spec, c.Object)
	})
	collect(func(c tenant.Classified) bool {
		return c.Class == tenant.ClassBoundary && isPlatformScoped(spec, c.Object)
	})

	// 3. Stop the tenant's workloads.
	collect(func(c tenant.Classified) bool { return c.Class == tenant.ClassRuntime })

	return plan, nil
}

// objectKey identifies an object for de-duplication within a delete plan. The
// broker Service is both an endpoint and platform-scoped, so without this it
// would be collected twice.
func objectKey(obj runtime.Object) string {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Sprintf("%T", obj)
	}
	o := &unstructured.Unstructured{Object: u}
	return o.GroupVersionKind().String() + "/" + o.GetNamespace() + "/" + o.GetName()
}

// Delete removes a workspace, revoking authority before deleting compute.
//
// It is idempotent: every step tolerates an object that is already gone, so a
// retry after a partial failure is safe. The namespace is deleted last, which
// takes the boundary and the storage with it.
//
// Delete does not wait for the namespace to finish terminating; use
// WaitForDeletion for that.
func (r *Reconciler) Delete(ctx context.Context, spec tenant.Spec) error {
	plan, err := DeletePlan(spec)
	if err != nil {
		return err
	}
	for _, c := range plan {
		if err := r.delete(ctx, c.Object); err != nil {
			return err
		}
	}
	return r.deleteNamespace(ctx, tenant.Namespace(spec.Slug))
}

// WaitForDeletion blocks until the workspace's namespace and the broker identity
// are gone, or the timeout elapses.
func (r *Reconciler) WaitForDeletion(ctx context.Context, spec tenant.Spec, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		remaining, err := r.remaining(ctx, spec)
		if err != nil {
			return err
		}
		if len(remaining) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("workspace %s still present after %s: %v", spec.Slug, timeout, remaining)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Missing reports reconciled (boundary and endpoint) objects that are absent.
//
// This is a read-only pre-flight, useful for reporting drift without mutating
// anything. It reports absence only; field-level drift is corrected by Ensure.
func (r *Reconciler) Missing(ctx context.Context, spec tenant.Spec) ([]string, error) {
	classified, err := tenant.BundleClassified(spec)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, c := range classified {
		if c.Class == tenant.ClassRuntime {
			continue // absence is legitimate: the tenant may have removed it
		}
		u, ri, err := r.resource(c.Object)
		if err != nil {
			return nil, err
		}
		if _, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, fmt.Sprintf("%s/%s (namespace %s)", u.GetKind(), u.GetName(), u.GetNamespace()))
		} else if err != nil {
			return nil, fmt.Errorf("get %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return missing, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// remaining lists anything that should not exist after deletion: the tenant
// namespace, and every object that lives OUTSIDE it (the broker's identity and
// address, which a namespace deletion does not touch).
func (r *Reconciler) remaining(ctx context.Context, spec tenant.Spec) ([]string, error) {
	var remaining []string

	ri := r.dyn.Resource(namespaceGVR)
	if _, err := ri.Get(ctx, tenant.Namespace(spec.Slug), metav1.GetOptions{}); err == nil {
		remaining = append(remaining, "namespace "+tenant.Namespace(spec.Slug))
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get namespace: %w", err)
	}

	classified, err := tenant.BundleClassified(spec)
	if err != nil {
		return nil, err
	}
	for _, c := range classified {
		acc, ok := c.Object.(metav1.Object)
		if !ok {
			continue
		}
		ns := acc.GetNamespace()
		if ns == "" || ns == tenant.Namespace(spec.Slug) {
			continue // cluster-scoped, or removed along with the namespace
		}
		u, objRI, err := r.resource(c.Object)
		if err != nil {
			return nil, err
		}
		if _, err := objRI.Get(ctx, u.GetName(), metav1.GetOptions{}); err == nil {
			remaining = append(remaining, fmt.Sprintf("%s %s/%s", strings.ToLower(u.GetKind()), ns, u.GetName()))
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}

	return remaining, nil
}

// apply performs a server-side apply, taking ownership of the fields it sets.
func (r *Reconciler) apply(ctx context.Context, obj runtime.Object) error {
	u, ri, err := r.resource(obj)
	if err != nil {
		return err
	}
	if _, err := ri.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{
		FieldManager: r.fm,
		Force:        true,
	}); err != nil {
		return fmt.Errorf("apply %s/%s: %w", u.GetKind(), u.GetName(), err)
	}
	return nil
}

// createIfAbsent creates the object only when it does not already exist, and
// never updates it. Used for ClassRuntime, which the tenant owns.
func (r *Reconciler) createIfAbsent(ctx context.Context, obj runtime.Object) error {
	u, ri, err := r.resource(obj)
	if err != nil {
		return err
	}

	// Present already: the tenant owns it now, so leave it alone. That is the whole
	// point of a create-once class.
	_, err = ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s/%s: %w", u.GetKind(), u.GetName(), err)
	}

	_, err = ri.Create(ctx, u, metav1.CreateOptions{FieldManager: r.fm})
	switch {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		return nil // lost a race, which is fine
	default:
		return fmt.Errorf("create %s/%s: %w", u.GetKind(), u.GetName(), err)
	}
}

func (r *Reconciler) delete(ctx context.Context, obj runtime.Object) error {
	u, ri, err := r.resource(obj)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s/%s: %w", u.GetKind(), u.GetName(), err)
	}
	return nil
}

func (r *Reconciler) deleteNamespace(ctx context.Context, name string) error {
	if err := r.dyn.Resource(namespaceGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

// resource converts a typed object to unstructured and resolves its client.
func (r *Reconciler) resource(obj runtime.Object) (*unstructured.Unstructured, dynamic.ResourceInterface, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, nil, fmt.Errorf("convert %T to unstructured: %w", obj, err)
	}
	u := &unstructured.Unstructured{Object: m}
	gvk := u.GroupVersionKind()
	if gvk.Kind == "" || gvk.Version == "" {
		return nil, nil, fmt.Errorf("object %T is missing apiVersion or kind; server-side apply requires both", obj)
	}
	// UnsafeGuessKindToResource returns both a GVR and a corrected GVK; we only
	// want the resource. The guess is heuristic, which is why every kind the
	// bundle emits is pinned in a test.
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	var ri dynamic.ResourceInterface = r.dyn.Resource(gvr)
	if ns := u.GetNamespace(); ns != "" {
		ri = r.dyn.Resource(gvr).Namespace(ns)
	}
	return u, ri, nil
}

// isPlatformScoped reports whether an object lives outside the tenant namespace.
func isPlatformScoped(spec tenant.Spec, obj runtime.Object) bool {
	acc, ok := obj.(metav1.Object)
	if !ok {
		return false
	}
	ns := acc.GetNamespace()
	return ns != "" && ns != tenant.Namespace(spec.Slug)
}
