package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// TestPlatformContract runs the real scarab reporter and town client against this API.
// Both paths must name local checkouts of the versions under test.
func TestPlatformContract(t *testing.T) {
	scarab := os.Getenv("PLATFORM_CONTRACT_SCARAB_DIR")
	town := os.Getenv("PLATFORM_CONTRACT_TOWN_DIR")
	if scarab == "" && town == "" {
		t.Skip("set PLATFORM_CONTRACT_SCARAB_DIR and PLATFORM_CONTRACT_TOWN_DIR to run")
	}
	if scarab == "" || town == "" {
		t.Fatal("both PLATFORM_CONTRACT_SCARAB_DIR and PLATFORM_CONTRACT_TOWN_DIR are required")
	}
	f := newIngestFixture(t)
	server := httptest.NewServer(f.srv)
	t.Cleanup(server.Close)
	runID := ulid.Make().String()

	writeToken := func(name, token string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	env := append(os.Environ(),
		"PLATFORM_CONTRACT_URL="+server.URL,
		"PLATFORM_CONTRACT_REPORT_TOKEN_FILE="+writeToken("report-token", f.token),
		"PLATFORM_CONTRACT_RUN_ID="+runID,
		"PLATFORM_CONTRACT_WORKSPACE_ID="+f.ws.ID,
	)
	run := func(dir, name string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s in %s: %v\n%s", name, dir, err, out)
		}
		t.Logf("%s in %s: %s", name, dir, out)
		return string(out)
	}
	out := run(scarab, "go", "test", "./internal/report", "-run", "^TestPlatformContractReporter$", "-count=1", "-v")
	if !strings.Contains(out, "--- PASS: TestPlatformContractReporter ") {
		t.Fatal("scarab reporter contract test did not run")
	}
	if got := f.post(t, "invalid", reportBody("bad-token", runID, "running")); got.Code != http.StatusUnauthorized {
		t.Fatalf("invalid reporting token: got %d, want 401", got.Code)
	}
	body := `{"eventId":"unknown-field","runId":"` + runID + `","state":"running","mode":"interactive","occurredAt":"2026-01-01T00:00:00Z","unexpected":true}`
	if got := f.post(t, f.token, body); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown report field: got %d, want 400", got.Code)
	}
	// Mint these after the Go build, so a cold build cannot expire the assertions.
	env = append(env,
		"PLATFORM_CONTRACT_OWNER_TOKEN_FILE="+writeToken("owner-token", assertion(t, testOwner)),
		"PLATFORM_CONTRACT_OTHER_TOKEN_FILE="+writeToken("other-token", assertion(t, "github#999999")),
	)
	run(town, "node", "--test", "src/server/platformContract.test.ts")
}
