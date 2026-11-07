package dns

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/iam"
)

const (
	vmMember    = "serviceAccount:gitea-vm@mock.iam.gserviceaccount.com"
	caddyMember = "serviceAccount:gitea-caddy-dns@mock.iam.gserviceaccount.com"
	// What the mock returns as the custom role's Name output. "mock", not the
	// stack's project "p": a role name the code built itself would not match.
	acmeRoleName = "projects/mock/roles/giteaAcmeDns01"
)

// The six permissions DNS-01 needs: the googleclouddns plugin reads, writes and
// deletes _acme-challenge TXT records, through ResourceRecordSets and Changes.
var acmePermissions = []string{
	"dns.changes.create",
	"dns.resourceRecordSets.create",
	"dns.resourceRecordSets.delete",
	"dns.resourceRecordSets.get",
	"dns.resourceRecordSets.list",
	"dns.resourceRecordSets.update",
}

// registered is one resource as the program registered it.
type registered struct {
	typ    string
	inputs resource.PropertyMap
	deps   []string
}

// recorder is a Pulumi mock that keeps what the program registers. Unlike
// package telemetry's, it keys by resource name: dns.New registers two
// IAMMembers of one type, and keyed by type the first would be lost.
type recorder struct {
	mu   sync.Mutex
	got  map[string]registered
	dups []string
}

func (r *recorder) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.got[a.Name]; seen {
		r.dups = append(r.dups, a.Name)
	}
	r.got[a.Name] = registered{typ: a.TypeToken, inputs: a.Inputs, deps: a.RegisterRPC.GetDependencies()}
	state := a.Inputs.Copy()
	switch a.TypeToken {
	case "gcp:serviceaccount/account:Account":
		email := str(a.Inputs, "accountId") + "@mock.iam.gserviceaccount.com"
		state["email"] = resource.NewStringProperty(email)
		state["name"] = resource.NewStringProperty("projects/mock/serviceAccounts/" + email)
	case "gcp:projects/iAMCustomRole:IAMCustomRole":
		state["name"] = resource.NewStringProperty("projects/mock/roles/" + str(a.Inputs, "roleId"))
	case "gcp:compute/address:Address":
		state["address"] = resource.NewStringProperty("203.0.113.10")
	}
	return a.Name + "-id", state, nil
}

// The zone lookup: it echoes the zone name back.
func (r *recorder) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

func str(m resource.PropertyMap, key string) string {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsString() {
		return ""
	}
	return v.StringValue()
}

// run calls New as infra/main.go does, with the two accounts made as package iam
// makes them, and returns what was registered by resource name.
func run(t *testing.T) map[string]registered {
	t.Helper()
	r := &recorder{got: map[string]registered{}}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cfg := &config.Config{Project: "p", Domain: "gitea.example.com", DNSZone: "main"}
		vm, err := serviceaccount.NewAccount(ctx, "test-vm", &serviceaccount.AccountArgs{
			AccountId: pulumi.String("gitea-vm"),
		})
		if err != nil {
			return err
		}
		caddy, err := serviceaccount.NewAccount(ctx, "test-caddy-dns", &serviceaccount.AccountArgs{
			AccountId: pulumi.String("gitea-caddy-dns"),
		})
		if err != nil {
			return err
		}
		addr, err := compute.NewAddress(ctx, "test-address", &compute.AddressArgs{})
		if err != nil {
			return err
		}
		_, err = New(ctx, cfg, addr, &iam.Accounts{VM: vm, CaddyDNS: caddy}, nil)
		return err
	}, pulumi.WithMocks("p", "s", r))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dups) > 0 {
		t.Fatalf("resource names registered twice, the recorder lost one of each: %v", r.dups)
	}
	return r.got
}

func urnOf(name string, r registered) string {
	return "urn:pulumi:s::p::" + r.typ + "::" + name
}

// The role is the whole of what Caddy may do in the zone. roles/dns.admin, the
// obvious choice, also holds dns.managedZones.setIamPolicy (grant anyone
// anything on the zone) and .delete (take the domain off the internet).
func TestACMERoleHasTheSixPermissionsOnly(t *testing.T) {
	role, ok := run(t)["gitea-acme-dns01"]
	if !ok || role.typ != "gcp:projects/iAMCustomRole:IAMCustomRole" {
		t.Fatalf("no custom role gitea-acme-dns01: %+v", role)
	}
	if id, p := str(role.inputs, "roleId"), str(role.inputs, "project"); id != "giteaAcmeDns01" || p != "p" {
		t.Errorf("role id %q in project %q, want giteaAcmeDns01 in p", id, p)
	}
	var perms []string
	for _, v := range role.inputs["permissions"].ArrayValue() {
		perms = append(perms, v.StringValue())
	}
	slices.Sort(perms)
	if !slices.Equal(perms, acmePermissions) {
		t.Errorf("permissions:\n got %v\nwant %v", perms, acmePermissions)
	}
	// Implied by the check above; spelled out because these are why the role
	// exists.
	for _, never := range []string{
		"dns.managedZones.setIamPolicy",
		"dns.managedZones.delete",
		"dns.managedZones.update",
	} {
		if slices.Contains(perms, never) {
			t.Errorf("role holds %s", never)
		}
	}
}

// Bound by the role's Name output, as GrantBuckets binds its role: the output
// is what makes the binding wait for the role to exist.
func TestCaddyDNSGetsTheRoleOnTheZone(t *testing.T) {
	got := run(t)
	b, ok := got["gitea-caddy-dns-acme"]
	if !ok || b.typ != "gcp:dns/dnsManagedZoneIamMember:DnsManagedZoneIamMember" {
		t.Fatalf("no zone binding gitea-caddy-dns-acme: %+v", b)
	}
	if z, r, m := str(b.inputs, "managedZone"), str(b.inputs, "role"), str(b.inputs, "member"); z != "main" ||
		r != acmeRoleName || m != caddyMember {
		t.Errorf("zone binding: zone %q role %q member %q, want main, %s (the role's Name output), %s",
			z, r, m, acmeRoleName, caddyMember)
	}
	if role := urnOf("gitea-acme-dns01", got["gitea-acme-dns01"]); !slices.Contains(b.deps, role) {
		t.Errorf("zone binding doesn't depend on the custom role; it depends on %v", b.deps)
	}
}

// The plugin finds the zone by listing the project's zones, which no zone
// binding can allow.
func TestCaddyDNSReadsTheProjectsZones(t *testing.T) {
	m, ok := run(t)["gitea-caddy-dns-reader"]
	if !ok || m.typ != "gcp:projects/iAMMember:IAMMember" {
		t.Fatalf("no project grant gitea-caddy-dns-reader: %+v", m)
	}
	if p, r, who := str(m.inputs, "project"), str(m.inputs, "role"), str(m.inputs, "member"); p != "p" ||
		r != "roles/dns.reader" || who != caddyMember {
		t.Errorf("project grant: project %q role %q member %q, want p, roles/dns.reader, %s", p, r, who, caddyMember)
	}
}

// Those two are all it gets here.
func TestCaddyDNSHasNothingElse(t *testing.T) {
	var grants []string
	for name, r := range run(t) {
		if str(r.inputs, "member") == caddyMember {
			grants = append(grants, name)
		}
	}
	slices.Sort(grants)
	if want := []string{"gitea-caddy-dns-acme", "gitea-caddy-dns-reader"}; !slices.Equal(grants, want) {
		t.Errorf("grants to gitea-caddy-dns@: %v, want %v", grants, want)
	}
}

// A fresh account can briefly be "not found" to IAM, and a failed deploy rolls
// back, which soft-deletes a custom role it created: its ID is then unusable
// from 7 to 37 days later (see package telemetry's tests). The account's first
// grant goes first, so that failure comes before the role exists.
func TestACMERoleWaitsForTheAccountsFirstGrant(t *testing.T) {
	got := run(t)
	reader := urnOf("gitea-caddy-dns-reader", got["gitea-caddy-dns-reader"])
	if deps := got["gitea-acme-dns01"].deps; !slices.Contains(deps, reader) {
		t.Errorf("custom role doesn't wait for %s; it waits for: %s", reader, strings.Join(deps, " "))
	}
}

// Caddy still renews as gitea-vm@ after this change, so gitea-vm@ keeps both
// its DNS grants. The later PR "Take DNS off gitea-vm@" removes them and turns
// this test round: both must then be gone.
func TestVMKeepsItsDNSRolesForNow(t *testing.T) {
	got := run(t)
	for name, want := range map[string]struct{ typ, role string }{
		"gitea-vm-dns-admin":  {"gcp:dns/dnsManagedZoneIamMember:DnsManagedZoneIamMember", "roles/dns.admin"},
		"gitea-vm-dns-reader": {"gcp:projects/iAMMember:IAMMember", "roles/dns.reader"},
	} {
		r, ok := got[name]
		if !ok || r.typ != want.typ || str(r.inputs, "role") != want.role || str(r.inputs, "member") != vmMember {
			t.Errorf("%s: %+v, want %s %s for gitea-vm@", name, r, want.typ, want.role)
		}
	}
	if z := str(got["gitea-vm-dns-admin"].inputs, "managedZone"); z != "main" {
		t.Errorf("gitea-vm@'s dns.admin is on zone %q, want main", z)
	}
}
