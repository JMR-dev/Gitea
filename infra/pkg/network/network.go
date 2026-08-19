// Package network builds the VPC, its firewall rules, and the static address the
// DNS A record points at.
package network

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
)

// Tag is applied to the instance and targeted by every firewall rule below.
const Tag = "gitea"

// IAPRange is the fixed source range Google's IAP TCP forwarding connects from.
// It is the only path to port 22 -- there is no other SSH ingress.
const IAPRange = "35.235.240.0/20"

type Network struct {
	Network    *compute.Network
	Subnetwork *compute.Subnetwork
	Address    *compute.Address
}

func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) (*Network, error) {
	opts := pulumi.DependsOn(deps)

	net, err := compute.NewNetwork(ctx, "gitea-vpc", &compute.NetworkArgs{
		Name: pulumi.String("gitea-vpc"),
		// Custom mode: auto mode would create a subnet in every region, which is
		// a lot of unused surface for a single VM.
		AutoCreateSubnetworks: pulumi.Bool(false),
		Description:           pulumi.String("Gitea deployment network"),
	}, opts)
	if err != nil {
		return nil, err
	}

	subnet, err := compute.NewSubnetwork(ctx, "gitea-subnet", &compute.SubnetworkArgs{
		Name:                  pulumi.String("gitea-subnet"),
		Network:               net.ID(),
		IpCidrRange:           pulumi.String("10.10.0.0/24"),
		Region:                pulumi.String(cfg.Region),
		PrivateIpGoogleAccess: pulumi.Bool(true),
	}, opts)
	if err != nil {
		return nil, err
	}

	addr, err := compute.NewAddress(ctx, "gitea-ip", &compute.AddressArgs{
		Name:        pulumi.String("gitea-ip"),
		Region:      pulumi.String(cfg.Region),
		AddressType: pulumi.String("EXTERNAL"),
		Description: pulumi.String("Static address for gitea-vm; the DNS A record depends on it"),
	}, opts)
	if err != nil {
		return nil, err
	}

	if _, err := compute.NewFirewall(ctx, "gitea-allow-web", &compute.FirewallArgs{
		Name:    pulumi.String("gitea-allow-web"),
		Network: net.ID(),
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{Protocol: pulumi.String("tcp"), Ports: pulumi.ToStringArray([]string{"80", "443"})},
			// HTTP/3.
			&compute.FirewallAllowArgs{Protocol: pulumi.String("udp"), Ports: pulumi.ToStringArray([]string{"443"})},
		},
		SourceRanges: pulumi.ToStringArray([]string{"0.0.0.0/0"}),
		TargetTags:   pulumi.ToStringArray([]string{Tag}),
		// Port 80 exists only for the HTTP->HTTPS redirect. ACME uses DNS-01, so
		// certificates never depend on inbound 80 and it can be dropped if wanted.
		Description: pulumi.String("Public HTTPS (and HTTP redirect) for Gitea"),
	}, opts); err != nil {
		return nil, err
	}

	if _, err := compute.NewFirewall(ctx, "gitea-allow-git-ssh", &compute.FirewallArgs{
		Name:    pulumi.String("gitea-allow-git-ssh"),
		Network: net.ID(),
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{Protocol: pulumi.String("tcp"), Ports: pulumi.ToStringArray([]string{"2222"})},
		},
		SourceRanges: pulumi.ToStringArray([]string{"0.0.0.0/0"}),
		TargetTags:   pulumi.ToStringArray([]string{Tag}),
		Description:  pulumi.String("git over SSH -- Gitea's built-in server, not host sshd"),
	}, opts); err != nil {
		return nil, err
	}

	if _, err := compute.NewFirewall(ctx, "gitea-allow-iap-ssh", &compute.FirewallArgs{
		Name:    pulumi.String("gitea-allow-iap-ssh"),
		Network: net.ID(),
		Allows: compute.FirewallAllowArray{
			&compute.FirewallAllowArgs{Protocol: pulumi.String("tcp"), Ports: pulumi.ToStringArray([]string{"22"})},
		},
		SourceRanges: pulumi.ToStringArray([]string{IAPRange}),
		TargetTags:   pulumi.ToStringArray([]string{Tag}),
		Description:  pulumi.String("Admin SSH via IAP TCP forwarding only"),
	}, opts); err != nil {
		return nil, err
	}

	return &Network{Network: net, Subnetwork: subnet, Address: addr}, nil
}
