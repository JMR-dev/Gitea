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
)

type DNS struct {
	Zone   *dns.LookupManagedZoneResult
	Record *dns.RecordSet
}

// New looks up the existing managed zone, points an A record at the static
// address, and grants the VM service account permission to write ACME challenge
// records -- scoped to this one zone rather than the whole project.
func New(
	ctx *pulumi.Context,
	cfg *config.Config,
	addr *compute.Address,
	vmServiceAccountEmail pulumi.StringOutput,
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
		Member:      pulumi.Sprintf("serviceAccount:%s", vmServiceAccountEmail),
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
		Member:  pulumi.Sprintf("serviceAccount:%s", vmServiceAccountEmail),
	}, pulumi.DependsOn(deps)); err != nil {
		return nil, err
	}

	return &DNS{Zone: zone, Record: rec}, nil
}
