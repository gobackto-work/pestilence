# pestilence

The control plane for a self-hosted Pi agent platform. Pestilence creates and removes isolated workspaces on a Kubernetes cluster.

Each workspace gets its own namespace, bounded resources, persistent storage, and authenticated endpoint. Pestilence is the only component with permission to manage resources across workspace namespaces.

## How it fits together

- **town** signs users in and provides the management interface.
- **pestilence** checks requests and manages workspace lifecycles.
- **scarab** runs the agent and brokers its requests for additional agents.

Agents do not receive Kubernetes credentials. They ask scarab to perform supported actions, while Kubernetes enforces workspace boundaries and resource limits.

## Development

```sh
go test ./...
go vet ./...
```

To render a workspace’s Kubernetes resources for review:

```sh
go run ./cmd/render-bundle -slug demo
```

See [Security hardening](docs/security-hardening.md) for the security model and remaining work.
