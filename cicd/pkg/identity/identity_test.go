package identity

import (
	"strings"
	"testing"
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
		for _, deploy := range infraRoles {
			if r == deploy {
				t.Errorf("%s is one of cb-infra's deploy roles", r)
			}
		}
	}
}
