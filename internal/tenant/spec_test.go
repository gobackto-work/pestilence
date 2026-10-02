package tenant

import "testing"

func TestLimitsFromTakesDefaultsWhenNothingIsAskedFor(t *testing.T) {
	defaults := DefaultLimits()
	got, err := LimitsFrom("", "", "", 0)
	if err != nil {
		t.Fatalf("LimitsFrom: %v", err)
	}
	if !got.LimitsCPU.Equal(defaults.LimitsCPU) {
		t.Errorf("limits.cpu = %s, want the default %s", got.LimitsCPU.String(), defaults.LimitsCPU.String())
	}
	if got.Pods != defaults.Pods {
		t.Errorf("pods = %d, want the default %d", got.Pods, defaults.Pods)
	}
}

func TestLimitsFromHonoursTheTenantsKnobs(t *testing.T) {
	got, err := LimitsFrom("1", "2Gi", "5Gi", 3)
	if err != nil {
		t.Fatalf("LimitsFrom: %v", err)
	}
	if got.LimitsCPU.String() != "1" {
		t.Errorf("limits.cpu = %s, want 1", got.LimitsCPU.String())
	}
	if got.LimitsMemory.String() != "2Gi" {
		t.Errorf("limits.memory = %s, want 2Gi", got.LimitsMemory.String())
	}
	if got.Storage.String() != "5Gi" {
		t.Errorf("storage = %s, want 5Gi", got.Storage.String())
	}
	if got.Pods != 3 {
		t.Errorf("pods = %d, want 3", got.Pods)
	}
}

// Requests are scheduling policy, not tenant input: the scheduler packs on
// requests, so requesting the full allowance would make one workspace
// unschedulable beside its peers.
func TestLimitsFromDerivesRequestsAsAFraction(t *testing.T) {
	got, err := LimitsFrom("2", "2Gi", "", 0)
	if err != nil {
		t.Fatalf("LimitsFrom: %v", err)
	}
	if got.RequestsCPU.String() != "500m" {
		t.Errorf("requests.cpu = %s, want a quarter of 2 (500m)", got.RequestsCPU.String())
	}
	if got.RequestsMemory.String() != "512Mi" {
		t.Errorf("requests.memory = %s, want a quarter of 2Gi (512Mi)", got.RequestsMemory.String())
	}
	if got.RequestsCPU.Cmp(got.LimitsCPU) >= 0 {
		t.Error("requests must be strictly below limits, or the workspace overcommits")
	}
}

func TestLimitsFromAppliesARequestFloor(t *testing.T) {
	got, err := LimitsFrom("200m", "256Mi", "", 0)
	if err != nil {
		t.Fatalf("LimitsFrom: %v", err)
	}
	if got.RequestsCPU.String() != "100m" {
		t.Errorf("requests.cpu = %s, want the 100m floor", got.RequestsCPU.String())
	}
	if got.RequestsMemory.String() != "128Mi" {
		t.Errorf("requests.memory = %s, want the 128Mi floor", got.RequestsMemory.String())
	}
}

// Asking for more than the node can give is an error, not a silent clamp: the
// tenant asked for something specific and should be told it cannot have it.
func TestLimitsFromRejectsAboveThePlatformMaximum(t *testing.T) {
	cases := map[string]struct {
		cpu, memory, storage string
		pods                 int32
	}{
		"cpu":     {cpu: "8"},
		"memory":  {memory: "64Gi"},
		"storage": {storage: "500Gi"},
		"pods":    {pods: 12},
	}
	for name, c := range cases {
		if _, err := LimitsFrom(c.cpu, c.memory, c.storage, c.pods); err == nil {
			t.Errorf("%s: LimitsFrom accepted a request above the platform maximum", name)
		}
	}
}

func TestLimitsFromRejectsUnparseableQuantities(t *testing.T) {
	for _, c := range []struct{ cpu, memory, storage string }{
		{cpu: "lots"},
		{memory: "heaps"},
		{storage: "plenty"},
	} {
		if _, err := LimitsFrom(c.cpu, c.memory, c.storage, 0); err == nil {
			t.Errorf("LimitsFrom accepted an unparseable quantity (%+v)", c)
		}
	}
}
