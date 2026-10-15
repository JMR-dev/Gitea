// Package network builds the VPC, its firewall rules, and the static address the
// DNS A record points at -- plus, when gitea:runnersEnabled is set, the subnet
// and NAT egress for the GKE runner cluster.
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

// Runner cluster address plan. None of these overlap gitea-subnet (10.10.0.0/24)
// or the VM's pinned podman subnet (10.89.10.0/24).
const (
	RunnerNodeCIDR = "10.20.0.0/24"

	RunnerPodsRange     = "gitea-runners-pods"
	RunnerPodsCIDR      = "10.24.0.0/20"
	RunnerServicesRange = "gitea-runners-services"
	RunnerServicesCIDR  = "10.28.0.0/22"
)

type Network struct {
	Network    *compute.Network
	Subnetwork *compute.Subnetwork
	Address    *compute.Address

	// Runners is nil unless gitea:runnersEnabled is set.
	Runners *RunnerNetwork
}

// RunnerNetwork is the GKE runner cluster's slice of the VPC. It lives here
// rather than in package gke so compute.New -- which already takes a *Network --
// can hand the egress IP to the VM without a new dependency edge.
type RunnerNetwork struct {
	Subnetwork *compute.Subnetwork
	Router     *compute.Router
	NAT        *compute.RouterNat
	// EgressIP is the single address every runner and every CI job reaches the
	// internet -- and Gitea -- from. The VM puts it in fail2ban's ignoreip.
	EgressIP *compute.Address
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

	out := &Network{Network: net, Subnetwork: subnet, Address: addr}
	if cfg.RunnersEnabled {
		if out.Runners, err = runnerNetwork(ctx, cfg, net, opts); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// runnerNetwork builds a separate subnet for the runner cluster, so gitea-subnet
// is untouched and NAT can be scoped to the runners alone, plus Cloud NAT on a
// reserved address. The nodes are private; this is their only way out.
func runnerNetwork(ctx *pulumi.Context, cfg *config.Config, net *compute.Network, opts pulumi.ResourceOption) (*RunnerNetwork, error) {
	subnet, err := compute.NewSubnetwork(ctx, "gitea-runners-subnet", &compute.SubnetworkArgs{
		Name:        pulumi.String("gitea-runners-subnet"),
		Network:     net.ID(),
		IpCidrRange: pulumi.String(RunnerNodeCIDR),
		Region:      pulumi.String(cfg.Region),
		// Image pulls from Artifact Registry and the Secret Manager CSI add-on
		// reach Google APIs without going through NAT.
		PrivateIpGoogleAccess: pulumi.Bool(true),
		SecondaryIpRanges: compute.SubnetworkSecondaryIpRangeArray{
			&compute.SubnetworkSecondaryIpRangeArgs{
				RangeName:   pulumi.String(RunnerPodsRange),
				IpCidrRange: pulumi.String(RunnerPodsCIDR),
			},
			&compute.SubnetworkSecondaryIpRangeArgs{
				RangeName:   pulumi.String(RunnerServicesRange),
				IpCidrRange: pulumi.String(RunnerServicesCIDR),
			},
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	// Reserved rather than auto-allocated, because the VM's fail2ban needs a
	// stable address to exempt. An auto-allocated NAT IP can change, and the
	// first sign would be every runner banned at once.
	egress, err := compute.NewAddress(ctx, "gitea-runners-egress-ip", &compute.AddressArgs{
		Name:        pulumi.String("gitea-runners-egress-ip"),
		Region:      pulumi.String(cfg.Region),
		AddressType: pulumi.String("EXTERNAL"),
		Description: pulumi.String("Cloud NAT egress for the Gitea Actions runner cluster"),
	}, opts)
	if err != nil {
		return nil, err
	}

	router, err := compute.NewRouter(ctx, "gitea-runners-router", &compute.RouterArgs{
		Name:    pulumi.String("gitea-runners-router"),
		Network: net.ID(),
		Region:  pulumi.String(cfg.Region),
	}, opts)
	if err != nil {
		return nil, err
	}

	nat, err := compute.NewRouterNat(ctx, "gitea-runners-nat", &compute.RouterNatArgs{
		Name:                pulumi.String("gitea-runners-nat"),
		Router:              router.Name,
		Region:              pulumi.String(cfg.Region),
		NatIpAllocateOption: pulumi.String("MANUAL_ONLY"),
		NatIps:              pulumi.StringArray{egress.SelfLink},
		// Only the runner subnet. The VM has its own external address and must
		// not start egressing from the NAT IP.
		SourceSubnetworkIpRangesToNat: pulumi.String("LIST_OF_SUBNETWORKS"),
		Subnetworks: compute.RouterNatSubnetworkArray{
			&compute.RouterNatSubnetworkArgs{
				Name:                 subnet.SelfLink,
				SourceIpRangesToNats: pulumi.ToStringArray([]string{"ALL_IP_RANGES"}),
			},
		},
		// CI is bursty: a single npm or pip install opens hundreds of parallel
		// connections, all from one IP. Dynamic allocation grows a node's port
		// budget on demand instead of failing at the static default of 64.
		// It requires endpoint-independent mapping to be off.
		EnableDynamicPortAllocation:      pulumi.Bool(true),
		EnableEndpointIndependentMapping: pulumi.Bool(false),
		MinPortsPerVm:                    pulumi.Int(256),
		MaxPortsPerVm:                    pulumi.Int(8192),
		LogConfig: &compute.RouterNatLogConfigArgs{
			Enable: pulumi.Bool(true),
			// Port exhaustion shows up here and nowhere else.
			Filter: pulumi.String("ERRORS_ONLY"),
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	return &RunnerNetwork{Subnetwork: subnet, Router: router, NAT: nat, EgressIP: egress}, nil
}
