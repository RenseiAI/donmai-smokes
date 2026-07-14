package interactive

import (
	"os"
	"strings"
	"testing"
)

// TestPlatformFreeBoundary is the platform-free self-guard for this package
// (../AGENTS.md §Boundary; the iron rule "NO platform endpoints"). donmai-smokes
// ships no automated leak guard, so this test IS the guard for the interactive
// lane: it scans every source file in this package and fails if any names a
// banned SaaS-control-plane token — WorkOS auth, Linear orchestration, the
// platform CLI or worker HTTP namespaces, or service-key / platform test-token
// credentials.
//
// Banned tokens are assembled from fragments so the guard file itself does not
// contain a literal match; every OTHER file (including this one) is scanned.
func TestPlatformFreeBoundary(t *testing.T) {
	banned := []struct{ label, needle string }{
		{"WorkOS test email", "WORKOS_" + "TEST_EMAIL"},
		{"WorkOS test password", "WORKOS_" + "TEST_PASSWORD"},
		{"WorkOS API key", "WORKOS_" + "API_KEY"},
		{"WorkOS host", "api." + "workos.com"},
		{"platform auth-config field", "active_" + "auth"},
		{"Linear GraphQL host", "api." + "linear.app/graphql"},
		{"Linear API key", "LINEAR_" + "API_KEY"},
		{"platform CLI namespace", "/api/" + "cli/"},
		{"platform worker register", "/api/" + "workers/register"},
		{"rensei service key prefix", "rsk" + "_"},
		{"rensei test token", "RENSEI_" + "TEST_TOKEN"},
		{"worker scope string", "worker:" + "register"},
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		text := string(src)
		scanned++
		for _, b := range banned {
			if strings.Contains(text, b.needle) {
				t.Errorf("BOUNDARY VIOLATION: %s references banned platform token (%s) — this lane is platform-free; relocate to the internal harness",
					e.Name(), b.label)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("boundary guard scanned zero source files — wrong working dir?")
	}
	t.Logf("platform-free boundary: %d source files clean of %d banned tokens", scanned, len(banned))
}
