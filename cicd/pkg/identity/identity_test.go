package identity

import (
	"slices"
	"strings"
	"testing"

	"gitea-cicd/pkg/config"
)

// cb-infra-preview runs branches nobody has reviewed. Anything here that can
// write turns a preview back into a deploy.
func TestInfraPreviewRolesOnlyRead(t *testing.T) {
	if len(InfraPreviewRoles) == 0 {
		t.Fatal("no roles: pulumi preview cannot read the stack")
	}
	for _, r := range InfraPreviewRoles {
		name := strings.ToLower(strings.TrimPrefix(r, "roles/"))
		if !strings.HasSuffix(name, "viewer") && !strings.HasSuffix(name, "browser") && !strings.HasSuffix(name, "reader") {
			t.Errorf("%s is not a read-only role", r)
		}
		// The primitive roles are read-only (viewer) or worse, but far too
		// broad: viewer makes the holder a projectViewer: member, which every
		// bucket's IAM grants object read -- including the backups bucket. A
		// preview must get narrow, service-specific read roles instead.
		switch name {
		case "viewer", "editor", "owner":
			t.Errorf("%s is a primitive role; too broad for a preview (reads every bucket's objects)", r)
		}
		for _, deploy := range slices.Concat(infraRoles, telemetryInfraRoles) {
			if r == deploy {
				t.Errorf("%s is one of cb-infra's deploy roles", r)
			}
		}
	}
}

// With gitea-cicd:watchtowerReader unset, cb-infra holds what it always did.
func TestTelemetryRolesFollowTheGate(t *testing.T) {
	if got := appInfraRoles(&config.Config{}); !slices.Equal(got, infraRoles) {
		t.Errorf("unset: got %v", got)
	}
	got := appInfraRoles(&config.Config{WatchtowerReader: "r@p.iam.gserviceaccount.com"})
	if !slices.Equal(got, slices.Concat(infraRoles, []string{"roles/logging.configWriter", "roles/pubsub.admin"})) {
		t.Errorf("set: got %v", got)
	}
}
