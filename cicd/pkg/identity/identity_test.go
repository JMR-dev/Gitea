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
		for _, deploy := range infraRoles {
			if r == deploy {
				t.Errorf("%s is one of cb-infra's deploy roles", r)
			}
		}
	}
}
