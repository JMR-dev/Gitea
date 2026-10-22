package relay

import (
	"os"
	"strings"
	"testing"
)

// The relay is deployed as two Cloud Run (Cloud Functions) services, and the
// Go buildpack rejects a module whose first path element has no dot: "the
// module path in the function's go.mod must contain a dot in the first path
// element before a slash, e.g. example.com/module". go build/vet/test do not
// enforce this, so a bad module path (e.g. a bare "gitea-cicd-relay") builds
// and tests clean here but fails only at `make ci-up`, after the whole CI
// stack has been applied. Enforce the buildpack's rule in the gate instead.
func TestModulePathIsDeployableAsAFunction(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	var path string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			path = strings.TrimSpace(rest)
			break
		}
	}
	if path == "" {
		t.Fatal("no module directive in go.mod")
	}
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") {
		t.Errorf("module path %q: first element %q has no dot, so the Cloud "+
			"Functions Go buildpack refuses to build it (needs a domain-like "+
			"path, e.g. example.com/module). This fails at `make ci-up`, not here.",
			path, first)
	}
}
