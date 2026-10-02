package tenant

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Limits is the per-workspace resource envelope, rendered into a ResourceQuota.
//
// These values are deliberately small. The deployment target is a single-node
// k0s cluster that also carries the control plane, CNI,
// ingress, and a kube-prometheus-stack. The design caps concurrent workspaces at
// three; a workspace whose *requests* were any larger could not be scheduled
// alongside the others.
type Limits struct {
	RequestsCPU    resource.Quantity
	LimitsCPU      resource.Quantity
	RequestsMemory resource.Quantity
	LimitsMemory   resource.Quantity
	Pods           int32
	Storage        resource.Quantity
	PVCs           int32
	Services       int32
	Jobs           int32

	// ContainerCPUDefault / ContainerMemoryDefault are the LimitRange defaults
	// applied to containers that do not declare their own resources.
	ContainerCPUDefault    resource.Quantity
	ContainerMemoryDefault resource.Quantity
	ContainerCPURequest    resource.Quantity
	ContainerMemoryRequest resource.Quantity

	// ContainerCPUMax / ContainerMemoryMax cap any single container, so one
	// workload cannot consume a whole workspace's budget and starve the peer
	// agents that share its persistent storage.
	ContainerCPUMax    resource.Quantity
	ContainerMemoryMax resource.Quantity
}

// Platform maximums. A tenant may ask for less, never more: these are the
// numbers the single node can actually honour alongside its peers.
const (
	MaxCPU     = "2"
	MaxMemory  = "3Gi"
	MaxStorage = "20Gi"
	MaxPods    = 6
)

// LimitsFrom derives the effective workspace limits from the tenant's request.
//
// The four arguments are the tenant's knobs. Everything else in Limits is
// PLATFORM POLICY: how a CPU or memory allowance splits between requests and
// limits, how many PVCs and Services a workspace may hold, and the per-container
// ceiling. Keeping the split explicit means policy can be corrected later
// without redefining what the tenant asked for.
//
// Empty strings and a zero pod count take the platform default. A request above
// the platform maximum is an error rather than a silent clamp, because the tenant
// asked for something this node cannot deliver and should be told.
func LimitsFrom(cpu, memory, storage string, maxPods int32) (Limits, error) {
	l := DefaultLimits()

	if cpu != "" {
		q, err := resource.ParseQuantity(cpu)
		if err != nil {
			return Limits{}, fmt.Errorf("cpu %q: %w", cpu, err)
		}
		if q.Cmp(resource.MustParse(MaxCPU)) > 0 {
			return Limits{}, fmt.Errorf("cpu %q exceeds the platform maximum %s", cpu, MaxCPU)
		}
		l.LimitsCPU = q
		l.RequestsCPU = requestsFor(q, "100m")
	}
	if memory != "" {
		q, err := resource.ParseQuantity(memory)
		if err != nil {
			return Limits{}, fmt.Errorf("memory %q: %w", memory, err)
		}
		if q.Cmp(resource.MustParse(MaxMemory)) > 0 {
			return Limits{}, fmt.Errorf("memory %q exceeds the platform maximum %s", memory, MaxMemory)
		}
		l.LimitsMemory = q
		l.RequestsMemory = requestsFor(q, "128Mi")
	}
	if storage != "" {
		q, err := resource.ParseQuantity(storage)
		if err != nil {
			return Limits{}, fmt.Errorf("storage %q: %w", storage, err)
		}
		if q.Cmp(resource.MustParse(MaxStorage)) > 0 {
			return Limits{}, fmt.Errorf("storage %q exceeds the platform maximum %s", storage, MaxStorage)
		}
		l.Storage = q
	}
	if maxPods > 0 {
		if maxPods > MaxPods {
			return Limits{}, fmt.Errorf("maxAgents %d exceeds the platform maximum %d", maxPods, MaxPods)
		}
		l.Pods = maxPods
	}
	return l, nil
}

// requestsFor is the scheduling policy: requests are a quarter of the allowance,
// with a floor. The scheduler packs on requests, so requesting the full allowance
// would make one workspace unschedulable beside its peers.
func requestsFor(limits resource.Quantity, floor string) resource.Quantity {
	q := limits.DeepCopy()
	q.SetMilli(q.MilliValue() / 4)
	floorQuantity := resource.MustParse(floor)
	if q.Cmp(floorQuantity) < 0 {
		return floorQuantity
	}
	return q
}

// DefaultLimits is sized for the reference single-node cluster.
func DefaultLimits() Limits {
	return Limits{
		RequestsCPU:    resource.MustParse("500m"),
		LimitsCPU:      resource.MustParse("2"),
		RequestsMemory: resource.MustParse("1Gi"),
		LimitsMemory:   resource.MustParse("3Gi"),
		// The broker runs in the platform namespace and is NOT charged against
		// this quota, so the budget is the root agent plus up to five workers.
		//
		// Note that the pod count is not the binding dimension: memory REQUESTS
		// are. At 256Mi per worker, 1Gi of requests admits three concurrent
		// workers, not five. The broker is told both budgets via env (§6.4), and
		// they must stay in step with this value or its accounting silently
		// diverges from what the API server will admit.
		Pods:     6,
		Storage:  resource.MustParse("20Gi"),
		PVCs:     2, // /workspace and optional /memory
		Services: 3,
		Jobs:     20,

		ContainerCPUDefault:    resource.MustParse("300m"),
		ContainerMemoryDefault: resource.MustParse("512Mi"),
		ContainerCPURequest:    resource.MustParse("100m"),
		ContainerMemoryRequest: resource.MustParse("256Mi"),
		ContainerCPUMax:        resource.MustParse("1"),
		ContainerMemoryMax:     resource.MustParse("2Gi"),
	}
}

// Spec describes one workspace. It is the input to Bundle.
type Spec struct {
	Slug    string
	OwnerID string

	// Hostname is the public endpoint for the workspace, e.g.
	// "fuzzy-wombat-x7q3.gobackto.work".
	Hostname string

	// PlatformNamespace is where the per-workspace broker runs. It is
	// deliberately NOT the tenant namespace: keeping the broker's
	// ServiceAccount outside the tenant namespace means the tenant cannot
	// reference it at all, because ServiceAccount references are
	// namespace-local in Kubernetes.
	PlatformNamespace string

	// BrokerPort is the port the broker listens on.
	BrokerPort int32

	// HTTPPort is the port the workspace's own HTTP endpoint listens on. A
	// Service is emitted only when this is greater than zero.
	HTTPPort int32

	// IngressNamespace is the namespace of the ingress controller that is
	// permitted to reach the workspace endpoint.
	IngressNamespace string

	Limits Limits

	// BrokerImage and AgentImage are the two scarab images (§8.6).
	//
	// An empty value DISABLES the corresponding object: no registry or image
	// exists yet, and emitting a Deployment that can never pull would be worse
	// than omitting it. Set both to apply the full workspace shape.
	BrokerImage string
	AgentImage  string

	// TokenSecretName holds the root agent's capability token (§8.7).
	//
	// The token is delivered as a mounted Secret and never as a file on
	// /workspace, which every worker can read (§5.8).
	TokenSecretName string

	// ControlPlaneNamespace is where pestilence keeps each workspace's
	// token-signing key. The private key must never reach a tenant namespace.
	ControlPlaneNamespace string

	// TokenTTL bounds the capability token (§8.7). Zero uses DefaultTokenTTL.
	TokenTTL time.Duration

	// Material is the workspace's signing keypair and minted token.
	//
	// It is supplied by the reconciler and never generated here: a pure renderer
	// that generated a key would produce a different one on every call, and the
	// public key the broker reads would change under it.
	Material SigningMaterial

	// AssertionPublicKeyPEM is town's public assertion key, PUBLISHED into this
	// workspace's namespace so the bridge can verify town for itself.
	//
	// A copy, not the original: the original lives in the pestilence namespace and is
	// what the control plane verifies against. A ConfigMap cannot be mounted across
	// namespaces, so the consumer needs its own. The control plane is the only party
	// that can distribute it -- town holds no Kubernetes credential at all, by design,
	// which is exactly why it cannot write the copy its own consumer needs.
	//
	// Empty omits the ConfigMap and the mount, so a bundle renders without one.
	AssertionPublicKeyPEM string

	// WithMemoryPVC provisions a second volume, conventionally mounted at
	// /memory, for agent-managed extended memory.
	WithMemoryPVC bool

	// BrokerTLS switches the workspace's broker hop from plaintext to TLS.
	//
	// ONE switch for both sides, deliberately: it gates the broker's certificate mount,
	// the agent's trust anchor, the environment variables, AND the scheme in
	// SCARAB_BROKER_URL. A switch that flipped the URL without the trust anchor would
	// produce a bridge that fails every call, and one that served TLS without the bridge
	// knowing would produce a bridge that never connects.
	//
	// Off by default because it cannot be turned on from this side alone: scarab must
	// serve TLS and verify the CA first. See docs/security-hardening.md -- and note that
	// enabling it requires RECREATING workspaces, because the root agent Deployment is
	// ClassRuntime and so is created once and never updated.
	BrokerTLS bool
}

// Namespace returns the tenant namespace for this workspace.
func (s Spec) Namespace() string { return Namespace(s.Slug) }

// BrokerServiceAccountName is the per-workspace broker identity. It lives in
// Spec.PlatformNamespace, not in the tenant namespace.
func (s Spec) BrokerServiceAccountName() string { return s.Slug + "-broker" }

// BrokerServiceName is the Service the root agent dials to reach its broker,
// in Spec.PlatformNamespace. A per-workspace name is required because each
// broker serves exactly one workspace.
func (s Spec) BrokerServiceName() string { return "broker-" + s.Slug }

// TokenKeySecretName is where the control plane stores this workspace's
// token-signing key. It lives in Spec.ControlPlaneNamespace, never in a tenant
// namespace.
func (s Spec) TokenKeySecretName() string { return s.Slug + "-token-key" }

// TokenPubkeyConfigMapName is the ConfigMap the broker reads the token
// verification key from. A public key is not secret, so a ConfigMap is correct;
// the private key never leaves the control plane.
func (s Spec) TokenPubkeyConfigMapName() string { return s.BrokerServiceName() + "-token-pubkey" }

// Normalized applies defaults so callers can construct a Spec with only Slug
// set. It is exported because the provisioner needs the effective platform
// namespace, which is empty until defaulted.
func (s Spec) Normalized() Spec {
	if s.Limits.RequestsCPU.IsZero() {
		s.Limits = DefaultLimits()
	}
	if s.PlatformNamespace == "" {
		s.PlatformNamespace = "scarab"
	}
	if s.BrokerPort == 0 {
		s.BrokerPort = DefaultBrokerPort
	}
	if s.TokenSecretName == "" {
		s.TokenSecretName = NameRootTokenSecret
	}
	if s.ControlPlaneNamespace == "" {
		s.ControlPlaneNamespace = DefaultControlPlaneNamespace
	}
	if s.IngressNamespace == "" {
		s.IngressNamespace = "traefik"
	}
	return s
}
