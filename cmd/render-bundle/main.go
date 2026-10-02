// Command render-bundle prints the Kubernetes objects for one workspace as a
// multi-document YAML stream.
//
// It exists so the security bundle can be reviewed and validated against a real
// cluster without running the whole control plane:
//
//	go run ./cmd/render-bundle -slug demo | kubectl apply --dry-run=server -f -
//
// The same output is what the provisioner will apply, so any drift between the
// two is a bug in one of them rather than a configuration difference.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gobackto-work/pestilence/internal/tenant"
	"sigs.k8s.io/yaml"
)

// port16 converts a flag value to a port, refusing anything out of range.
//
// A checked conversion rather than a bare int32(): a truncated port would be a
// silently wrong endpoint, and a flag is the one place a human types a number.
func port16(name string, v int) int32 {
	if v < 0 || v > 65535 {
		fmt.Fprintf(os.Stderr, "render-bundle: -%s must be 0..65535, got %d\n", name, v)
		os.Exit(2)
	}
	//nolint:gosec // G115 cannot see the range check above; the conversion is safe.
	return int32(v)
}

func main() {
	var (
		slug        = flag.String("slug", "", "workspace slug (required)")
		owner       = flag.String("owner", "self-test", "owner identifier")
		hostname    = flag.String("hostname", "", "public hostname (default <slug>.gobackto.work)")
		httpPort    = flag.Int("http-port", 8000, "workspace HTTP port; 0 omits the Service")
		brokerNS    = flag.String("broker-namespace", "scarab", "namespace holding the per-workspace broker")
		brokerPort  = flag.Int("broker-port", 8443, "broker listen port")
		withMemory  = flag.Bool("memory-pvc", false, "also provision the /memory volume")
		brokerImage = flag.String("broker-image", "", "broker image ref; empty omits the broker Deployment")
		agentImage  = flag.String("agent-image", "", "agent image ref; empty omits the root agent Deployment")
	)
	flag.Parse()

	if *slug == "" {
		fmt.Fprintln(os.Stderr, "render-bundle: -slug is required")
		flag.Usage()
		os.Exit(2)
	}

	host := *hostname
	if host == "" {
		host = *slug + ".gobackto.work"
	}

	spec := tenant.Spec{
		Slug:              *slug,
		OwnerID:           *owner,
		Hostname:          host,
		PlatformNamespace: *brokerNS,
		BrokerPort:        port16("broker-port", *brokerPort),
		HTTPPort:          port16("http-port", *httpPort),
		WithMemoryPVC:     *withMemory,
		BrokerImage:       *brokerImage,
		AgentImage:        *agentImage,
	}

	// Signing material is generated here so the token Secret and the public-key
	// ConfigMap are reviewable rather than empty. The reconciler persists ONE
	// keypair and reuses it; a fresh key per render is expected, and is why this
	// output is for review rather than for diffing.
	material, err := tenant.NewSigningMaterial(spec, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-bundle: generate signing material: %v\n", err)
		os.Exit(1)
	}
	spec.Material = material

	objs, err := tenant.BundleClassified(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render-bundle: %v\n", err)
		os.Exit(1)
	}

	for _, c := range objs {
		b, err := yaml.Marshal(c.Object)
		if err != nil {
			fmt.Fprintf(os.Stderr, "render-bundle: marshal %T: %v\n", c.Object, err)
			os.Exit(1)
		}
		fmt.Printf("---\n%s", b)
	}
}
