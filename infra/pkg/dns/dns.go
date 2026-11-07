// Package dns manages records in the PRE-EXISTING Cloud DNS zone.
//
// The zone itself is deliberately not a Pulumi resource: it already exists and
// is delegated, so importing it would put a `pulumi destroy` one keystroke away
// from deleting live DNS.
package dns

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/dns"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/iam"
)

type DNS struct {
	Zone   *dns.LookupManagedZoneResult
	Record *dns.RecordSet
}

// acmeRoleID is the custom role for DNS-01. A custom role's ID stays reserved
// for 37 days after it is deleted, so never rename it casually.
const acmeRoleID = "giteaAcmeDns01"

// New looks up the existing managed zone, points an A record at the static
// address, and grants permission to write ACME challenge records -- scoped to
// this one zone rather than the whole project -- to the VM service account,
// which Caddy uses today, and to the Caddy DNS account it will move to.
func New(
	ctx *pulumi.Context,
	cfg *config.Config,
	addr *compute.Address,
	accounts *iam.Accounts,
	deps []pulumi.Resource,
) (*DNS, error) {
	zone, err := dns.LookupManagedZone(ctx, &dns.LookupManagedZoneArgs{
		Name:    cfg.DNSZone,
		Project: &cfg.Project,
	}, pulumi.DependsOn(deps))
	if err != nil {
		return nil, err
	}

	rec, err := dns.NewRecordSet(ctx, "gitea-a", &dns.RecordSetArgs{
		Name:        pulumi.String(cfg.FQDN()),
		ManagedZone: pulumi.String(zone.Name),
		Type:        pulumi.String("A"),
		// Short enough that a VM rebuild behind a new address is not a long
		// outage, long enough not to hammer the resolvers.
		Ttl:     pulumi.Int(300),
		Rrdatas: pulumi.StringArray{addr.Address},
	}, pulumi.DependsOn(deps))
	if err != nil {
		return nil, err
	}

	// Caddy writes and deletes _acme-challenge TXT records here for DNS-01.
	// Zone-scoped rather than project-wide: a compromised VM should not be able
	// to repoint unrelated domains.
	if _, err := dns.NewDnsManagedZoneIamMember(ctx, "gitea-vm-dns-admin", &dns.DnsManagedZoneIamMemberArgs{
		ManagedZone: pulumi.String(zone.Name),
		Role:        pulumi.String("roles/dns.admin"),
		Member:      pulumi.Sprintf("serviceAccount:%s", accounts.VM.Email),
	}, pulumi.DependsOn(deps)); err != nil {
		return nil, err
	}

	// The zone-scoped grant is not enough on its own: the googleclouddns plugin
	// maps the domain to a zone by LISTING the project's managed zones, and a
	// list is a project-level permission that no zone binding can confer. Without
	// this, presenting the challenge fails with a bare 403. dns.reader adds
	// read-only access and nothing else, and every write stays scoped to the zone
	// above.
	if _, err := projects.NewIAMMember(ctx, "gitea-vm-dns-reader", &projects.IAMMemberArgs{
		Project: pulumi.String(cfg.Project),
		Role:    pulumi.String("roles/dns.reader"),
		Member:  pulumi.Sprintf("serviceAccount:%s", accounts.VM.Email),
	}, pulumi.DependsOn(deps)); err != nil {
		return nil, err
	}

	// The same two grants for the Caddy DNS account (package iam), which Caddy
	// does not use yet; gitea-vm@ keeps the two above until it does.
	//
	// First the project-level read, for the zone listing, as above. It is
	// also the fresh account's first grant, and the role below waits for it:
	// IAM can briefly report a new account as not found, and if that failed
	// the deploy after the role was made, the rollback would soft-delete the
	// role and its ID would be unusable from 7 to 37 days later.
	reader, err := projects.NewIAMMember(ctx, "gitea-caddy-dns-reader", &projects.IAMMemberArgs{
		Project: pulumi.String(cfg.Project),
		Role:    pulumi.String("roles/dns.reader"),
		Member:  pulumi.Sprintf("serviceAccount:%s", accounts.CaddyDNS.Email),
	}, pulumi.DependsOn(deps))
	if err != nil {
		return nil, err
	}

	// Then the writes, through a custom role rather than roles/dns.admin: that
	// also holds dns.managedZones.setIamPolicy (grant anyone anything on the
	// zone), .update and .delete. These six are what the googleclouddns plugin
	// calls to create, read and remove the _acme-challenge TXT records. They
	// reach every record in the zone, as IAM cannot narrow them to one name,
	// but not the zone itself or who may change it.
	role, err := projects.NewIAMCustomRole(ctx, "gitea-acme-dns01", &projects.IAMCustomRoleArgs{
		Project:     pulumi.String(cfg.Project),
		RoleId:      pulumi.String(acmeRoleID),
		Title:       pulumi.String("Gitea ACME DNS-01"),
		Description: pulumi.String("Write ACME challenge records in the zone; no zone or IAM changes"),
		Permissions: pulumi.ToStringArray([]string{
			"dns.changes.create",
			"dns.resourceRecordSets.create",
			"dns.resourceRecordSets.delete",
			"dns.resourceRecordSets.get",
			"dns.resourceRecordSets.list",
			"dns.resourceRecordSets.update",
		}),
	}, pulumi.DependsOn(deps), pulumi.DependsOn([]pulumi.Resource{reader}))
	if err != nil {
		return nil, err
	}
	// role.Name, not a name spelled out: the output makes the binding wait
	// for the role.
	if _, err := dns.NewDnsManagedZoneIamMember(ctx, "gitea-caddy-dns-acme", &dns.DnsManagedZoneIamMemberArgs{
		ManagedZone: pulumi.String(zone.Name),
		Role:        role.Name,
		Member:      pulumi.Sprintf("serviceAccount:%s", accounts.CaddyDNS.Email),
	}, pulumi.DependsOn(deps)); err != nil {
		return nil, err
	}

	return &DNS{Zone: zone, Record: rec}, nil
}
