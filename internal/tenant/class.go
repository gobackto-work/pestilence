package tenant

import "k8s.io/apimachinery/pkg/runtime"

// Class describes how the provisioner treats an object over the lifetime of a
// workspace.
//
// The distinction exists because the provisioner is idempotent: anything it
// reconciles is recreated if it disappears. Some objects must be *corrected*,
// some must be *guaranteed*, and some must be created once and then left alone,
// because the tenant is allowed to replace them.
type Class string

const (
	// ClassBoundary is the tenant security boundary: the namespace and its Pod
	// Security labels, the quota, the limit range, the network policies, the
	// identities, and the RBAC that constrains the broker.
	//
	// Continuously reconciled. A tenant must never be able to alter these, so
	// drift is corrected rather than tolerated.
	ClassBoundary Class = "boundary"

	// ClassEndpoint is what makes the workspace reachable: the Service in front
	// of the runtime, and the Ingress that publishes it.
	//
	// Reconciled, because the platform guarantees that a workspace has a
	// reachable HTTPS endpoint. If the tenant deletes the route it comes back.
	ClassEndpoint Class = "endpoint"

	// ClassRuntime is the workload the tenant actually talks to.
	//
	// Created once at provisioning and then owned by the tenant. It is NEVER
	// reconciled back: a tenant may deliberately replace the default landing
	// page, and a reconciler that recreates it would fight the user forever.
	//
	// The rule of thumb: the provisioner guarantees the workspace is reachable
	// and confined; it does not guarantee what the tenant runs inside it.
	ClassRuntime Class = "runtime"

	// ClassPlatform is platform infrastructure that must exist for the workspace
	// to function, and that lives OUTSIDE the tenant namespace -- currently the
	// per-workspace broker Deployment in `scarab`.
	//
	// Reconciled, and deliberately NOT ClassRuntime: the tenant cannot write the
	// platform namespace, so a deleted broker would never come back and the
	// workspace would be left silently unable to spawn agents.
	ClassPlatform Class = "platform"
)

// Classified pairs an object with the provisioner's treatment of it.
type Classified struct {
	Class  Class
	Object runtime.Object
}
