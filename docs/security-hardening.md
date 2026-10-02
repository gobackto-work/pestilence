# Security hardening: deferred items

Real weaknesses, understood. Each entry records enough to pick it up without
re-deriving the analysis. Status is per item: some are deferred, some are done.

The bar for adding to this file: the item is a genuine weakness, and deferring it does
not contradict the design. Anything that would *break* a stated invariant gets fixed
instead of recorded.

---

## 1. Confine pestilence's standing privilege to tenant namespaces

**Status: DONE.** Implemented across `my-opps` and `pestilence`; the exact objects
and the reasoning the code depends on are below.

### The weakness

A compromise of the control plane **was** a compromise of the whole cluster, not of the
tenant namespaces it manages. Its ClusterRole granted, cluster-wide:

- read/write on Secrets in every namespace
- create and delete any namespace
- create Deployments in any namespace — which is code execution on the node, since a
  Deployment in `kube-system` can request a privileged container and PSA does not
  restrict `kube-system`

### Why it is not a boundary failure

It was not reachable by narrowing a verb list. RBAC cannot scope a ClusterRole grant to
"namespaces matching a label", so a provisioner that creates arbitrary namespaces needs
cluster-wide rights on what it writes — unless it *binds* a pre-created role, which is
what the reduction below does. The privilege follows from being the provisioner at all.

### The reduction, as built

The tenant-namespace powers were moved out of pestilence's own ClusterRole into a
pre-created ClusterRole it can only *bind*:

1. `my-opps/cluster/manifests/rbac-pestilence-scoped.yaml` creates
   `ClusterRole/pestilence-tenant` (the tenant-namespace powers: deployments, pods,
   jobs, services, configmaps, secrets, networkpolicies, ingresses, resourcequotas,
   limitranges, serviceaccounts, roles, rolebindings) and
   `ClusterRole/pestilence-platform` (the smaller `scarab`-namespace set). my-opps owns
   them, so pestilence cannot edit or widen them.
2. `my-opps/cluster/manifests/rbac-pestilence.yaml` reduces pestilence's ClusterRole to
   `namespaces` (create/get/list/watch/patch/update/delete), `rolebindings`
   (create/update/patch), and `bind` on the two pre-created roles, scoped by
   `resourceName`. The signing key is a namespaced `Role` in pestilence's own namespace.
3. `my-opps/cluster/manifests/validatingadmissionpolicy-pestilence-rbac.yaml` makes the
   `bind` safe, because `bind` cannot be scoped to RoleBindings rather than
   ClusterRoleBindings:
   - deny ClusterRoleBindings created by the pestilence ServiceAccount
   - deny RoleBindings created by it outside namespaces labeled
     `agents.gobackto.work/workspace`
   - pin the `roleRef` to `pestilence-tenant` and the subject to the control-plane
     ServiceAccount, so the role cannot be handed to another identity
   - **require that any namespace it creates carries the `restricted` PSA labels.**
     Without this last one the exercise is void: it simply creates a fresh unlabeled
     namespace and runs a privileged pod there.

`pestilence/internal/tenant/bundle.go` emits the per-workspace
`RoleBinding/pestilence-tenant`, binding the pre-created ClusterRole to the control-plane
ServiceAccount. It is applied **first** among the namespaced objects, because until it
exists the provisioner has no permission in the namespace it just created;
`TestTenantGrantIsFirstAndBindsTheControlPlane` pins both the position and the subject.

That converts cluster compromise into tenant-namespace compromise.

### The two properties that still matter

The reduction does not replace the two properties that actually defend the platform,
and they are unchanged: the control plane never runs tenant code, and it is never
reachable from a tenant namespace. Item 2 covers the second.

### Verified live

Applied to the real cluster on 2026-09-29, in order (pre-created roles, then policies,
then the reduced ClusterRole), and exercised by impersonating the pestilence
ServiceAccount:

- the per-workspace self-binding is **allowed** in a labelled namespace — the exact
  bootstrap operation — and the code performs it: deleting the binding and waiting for
  a drift sweep recreated it with field manager `pestilence`
- the broker `Role/broker` binding is allowed; a RoleBinding in an unlabelled namespace
  is **denied** by `pestilence-rolebinding-only-workspace-namespaces`
- binding `pestilence-tenant` to another identity is **denied** by
  `pestilence-rolebinding-shapes`
- a namespace without the restricted PSA labels is **denied** by
  `pestilence-namespace-invariants`
- with the reduced ClusterRole, pestilence can no longer read `pods` or `secrets` in
  `default`/`kube-system`, cannot create ClusterRoles or ClusterRoleBindings, and cannot
  `bind` `cluster-admin`; `pods/exec`, `attach` and `portforward` are all refused (checked
  with `kubectl auth can-i ... --subresource=exec`)
- the control plane reconciled the existing workspace across a drift sweep with no 403s

Two things the live apply caught that static validation could not:

1. `has(object.metadata.labels['key'])` is not valid CEL — `has()` takes a field
   selection, not a map index. The namespace-label check is `'key' in labels`.
2. `pestilence-no-clusterrolebindings` is redundant while the reduced ClusterRole grants
   no `clusterrolebindings` verb: RBAC refuses first and the policy never fires. It is
   kept as a guard against a future grant accidentally restoring the verb, not because it
   is currently load-bearing.

---

## 2. Give the pestilence namespace an ingress NetworkPolicy

**Status: DONE.** See `my-opps/cluster/manifests/networkpolicy-pestilence.yaml`.

pestilence had **no ingress restriction of its own**. It was unreachable from tenant
namespaces only because tenant namespaces default-deny egress and their one broad allow
excludes RFC1918 (`10.0.0.0/8`, which contains the ClusterIP range) — protected by the
*client's* policy, the wrong direction. Any namespace with permissive egress could manage
workspaces, and at the time the API had no authentication of its own either.

The policy now denies ingress by default and allows port 8080 from the `town` namespace
only, and the API verifies an assertion on every route except `/healthz`. Verified: a pod
in `default` gets nothing from pestilence or from town, and town's `/readyz` — which calls
pestilence's unauthenticated `/healthz` — answers 200.

This is also the prerequisite for trusting anything town sends. An identity asserted by
a caller is only as good as the guarantee that the caller is town.

---

## 3. Broker TLS — **DONE** (was deferred)

**Status: enabled and verified.** `Spec.BrokerTLS` (`-broker-tls`) is on in
`my-opps/cluster/manifests/deployment-pestilence.yaml`. scarab serves TLS and the
agent trusts the CA, so the switch could be flipped without breaking broker calls.

Enabling it surfaced one bug, fixed in the same sequence: a Secret volume reference
is namespace-local, so the agent could not mount the broker's cert from the platform
namespace. The certificate is now published twice — `Secret/broker-<slug>-tls` (cert
+ key) for the broker, and `ConfigMap/broker-<slug>-ca` (cert only) for the agent.
Verified end to end from inside a root agent: `GET /agents` over TLS returns 200,
and the same call with no CA is refused. Red-team finding 5 is closed.

Enabling it also required **recreating** the workspaces that existed, not restarting
them: the root agent Deployment is `ClassRuntime`, so a restarted pod would have
kept `http` while its broker moved to TLS. That is the same property that made
`SCARAB_HOSTNAME` reach only new workspaces, and it applies to every pod-spec
hardening.

### The certificate expires in five years

Minted once and never rotated — the reasoning is in `NewBrokerTLS`, and it amounts to: the
certificate provides confidentiality for an in-cluster hop while the rotating capability
token provides authorization, and rotation would require a coordinated reload across two
Secret mounts where a mismatch breaks the workspace outright.

The consequence is a **dated operational item**. These certificates expire in five years,
and renewal is not automatic: it needs the same coordinated reload that made rotation
unattractive. If the platform is still running then, either the reload path will have been
built or workspaces will need recreating.

---

## 4. The remaining isolation items — two done, two rejected with reasons

**Status: node sysctls done; broker namespace done; uid split blocked; `hostUsers`
not used.**

### Node sysctls — **DONE**

`my-opps/cluster/node/60-platform-hardening.conf`, installed by
`cluster/node-hardening.sh` and applied at boot by systemd-sysctl. Verified on the
node:

- `kernel.io_uring_disabled = 2` — io_uring disabled for everyone; a recurring
escalation surface nothing here uses.
- `kernel.unprivileged_userns_clone = 0` and `user.max_user_namespaces = 0` —
unprivileged user namespaces refused.
- `kernel.yama.ptrace_scope = 1` — ptrace restricted to descendants. This blunts the
same-uid worker→root path at no cost to either image, and it is the cheap half of
finding 6.

### The broker namespace — **DONE**

`my-opps/cluster/manifests/networkpolicy-scarab.yaml`. Default-deny both directions,
with the broker's two legitimate needs named: ingress from workspace-labelled
namespaces on 8443, and egress to cluster DNS and the API server (a Cilium entity, for
the same kube-proxy reason as the pestilence namespace's own policy). Verified: the
brokers stayed Ready and a root agent completed a TLS handshake with its broker
through the new ingress rule.

### The uid split — **BLOCKED, and the initial assumption was wrong about why**

The initial assumption was that the credential store is "`0640` with the pod's group". It
is not. On the live cluster:

    drwx--S---  /workspace/.pi/agent               (2700 — no group or other access)
    -rw-------  /workspace/.pi/agent/auth.json     (0600, owned by uid 1000)

Pi writes its own credential `0600` and the directory `2700`. Workers can read it
**only because they are uid 1000**, the same as the root agent. Splitting the uid would
not merely fail to protect file access — it would **stop workers reading the model
credential at all**, so they could not start. The fix is not in pestilence or scarab
as they stand: it needs the bridge to widen the credential store to `0640`/`2750` after
Pi writes it, or Pi to support a group-readable store. Until then the pid/proc half of
finding 6 is covered by `ptrace_scope=1`, and the uid split is deferred rather than
half-built.

### `hostUsers` — **NOT USED, by choice**

`spec.hostUsers: false` gives each pod its own user namespace. It is incompatible with
`max_user_namespaces = 0`, and the two are alternatives: either user namespaces exist
(and are an escalation surface) or they do not. For a single-node platform whose pods
all run the platform's own images, closing the surface is the stronger choice, so the
sysctls win and `hostUsers` stays at its default.
