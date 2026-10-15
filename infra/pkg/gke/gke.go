// Package gke builds the cluster that runs Gitea Actions jobs: one zonal GKE
// Standard cluster, used for nothing else, with a single autoscaled node pool.
//
// Why GKE at all: the runner must not live on gitea-vm. The 2 GB box has no
// headroom for it (docs/runbook.md), and a runner on the host it deploys is the
// bootstrapping loop docs/migrate-to-gitea-scm.md warns about.
//
// Why Standard and not Autopilot: workflows need a Docker daemon, which means a
// privileged Docker-in-Docker pod, and Autopilot refuses privileged pods. The
// cluster is dedicated to the runners so that the cluster boundary -- not a
// taint or a namespace -- is what a job escaping its container runs into.
//
// Why zonal: GKE's free tier credits the management fee of one zonal (or
// Autopilot) cluster per billing account. A regional cluster would bill it.
package gke

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/container"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/network"
)

// Name is the cluster's name. Fixed, so `gcloud container clusters
// get-credentials` and the runbook never have to look it up.
const Name = "gitea-runners"

type Cluster struct {
	Cluster  *container.Cluster
	NodePool *container.NodePool
	NodeSA   *serviceaccount.Account
}

// New returns nil (without error) when gitea:runnersEnabled is not set, the same
// way build.New tolerates GitHub being unconfigured.
func New(ctx *pulumi.Context, cfg *config.Config, net *network.Network, deps []pulumi.Resource) (*Cluster, error) {
	if !cfg.RunnersEnabled || net.Runners == nil {
		ctx.Log.Warn("gitea:runnersEnabled not set -- skipping the GKE runner cluster", nil)
		return nil, nil
	}
	opts := pulumi.DependsOn(deps)

	// A dedicated node identity instead of the Compute Engine default account,
	// which on most projects holds roles/editor. A job that escapes to the node
	// gets this account's token from the node's own metadata server, so it
	// holds exactly what a node needs to report logs and metrics, and nothing
	// else -- not even Artifact Registry read, since job images come from
	// public registries.
	nodeSA, err := serviceaccount.NewAccount(ctx, "gitea-gke-nodes-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("gitea-gke-nodes"),
		DisplayName: pulumi.String("Gitea runner GKE nodes"),
		Description: pulumi.String("Node identity for the Gitea Actions runner cluster"),
	}, opts)
	if err != nil {
		return nil, err
	}
	nodeRole, err := projects.NewIAMMember(ctx, "gke-nodes-default", &projects.IAMMemberArgs{
		Project: pulumi.String(cfg.Project),
		Role:    pulumi.String("roles/container.defaultNodeServiceAccount"),
		Member:  pulumi.Sprintf("serviceAccount:%s", nodeSA.Email),
	}, opts)
	if err != nil {
		return nil, err
	}

	cluster, err := container.NewCluster(ctx, "gitea-runners", &container.ClusterArgs{
		Name:        pulumi.String(Name),
		Location:    pulumi.String(cfg.Zone),
		Description: pulumi.String("Gitea Actions runners (Docker-in-Docker)"),

		Network:        net.Network.ID(),
		Subnetwork:     net.Runners.Subnetwork.ID(),
		NetworkingMode: pulumi.String("VPC_NATIVE"),
		IpAllocationPolicy: &container.ClusterIpAllocationPolicyArgs{
			ClusterSecondaryRangeName:  pulumi.String(network.RunnerPodsRange),
			ServicesSecondaryRangeName: pulumi.String(network.RunnerServicesRange),
		},
		// A /26 per node instead of the default /24, so the /20 pod range
		// covers 64 nodes rather than 16. A runner node hosts a handful of pods:
		// GKE's own, a runner or two, and a few job pods.
		DefaultMaxPodsPerNode: pulumi.Int(32),

		// The default pool cannot be configured the way the real one is, so it
		// is created and immediately deleted. Its node config still names the
		// dedicated SA, so nothing ever runs as the Compute Engine default
		// account, not even for those few minutes.
		RemoveDefaultNodePool: pulumi.Bool(true),
		InitialNodeCount:      pulumi.Int(1),
		NodeConfig: &container.ClusterNodeConfigArgs{
			ServiceAccount: nodeSA.Email,
			OauthScopes:    pulumi.ToStringArray([]string{"https://www.googleapis.com/auth/cloud-platform"}),
		},

		ReleaseChannel: &container.ClusterReleaseChannelArgs{Channel: pulumi.String("REGULAR")},

		// Workload Identity is how the runner pod reads the registration token
		// without Pulumi ever handling it. It also replaces the node's metadata
		// server with GKE's, so a pod cannot simply ask for the node SA's token.
		WorkloadIdentityConfig: &container.ClusterWorkloadIdentityConfigArgs{
			WorkloadPool: pulumi.Sprintf("%s.svc.id.goog", cfg.Project),
		},
		// Dataplane V2, which is what enforces the runner NetworkPolicy.
		// NetworkPolicy{Enabled} must NOT also be set: that block is the legacy
		// Calico path and conflicts with ADVANCED_DATAPATH.
		DatapathProvider: pulumi.String("ADVANCED_DATAPATH"),
		// The Secret Manager CSI add-on mounts the registration token into the
		// runner pod. No rotation: the token is only read at registration.
		SecretManagerConfig: &container.ClusterSecretManagerConfigArgs{Enabled: pulumi.Bool(true)},
		EnableShieldedNodes: pulumi.Bool(true),

		// Private nodes: no external IPs, all egress through the NAT in
		// package network. EnablePrivateEndpoint is not a choice here: with IP
		// endpoints disabled below, the provider reads it back as true, so
		// leaving it unset is a diff on every run that never converges.
		PrivateClusterConfig: &container.ClusterPrivateClusterConfigArgs{
			EnablePrivateNodes:    pulumi.Bool(true),
			EnablePrivateEndpoint: pulumi.Bool(true),
		},
		// The control plane is reachable only through its DNS endpoint, which
		// is authorised by IAM alone. That works from Cloud Build's default
		// pool and from a workstation, neither of which has a stable IP to put
		// in an authorised-networks list. IP endpoints are switched off outright
		// rather than left public with an empty allowlist.
		ControlPlaneEndpointsConfig: &container.ClusterControlPlaneEndpointsConfigArgs{
			DnsEndpointConfig: &container.ClusterControlPlaneEndpointsConfigDnsEndpointConfigArgs{
				AllowExternalTraffic: pulumi.Bool(true),
			},
			IpEndpointsConfig: &container.ClusterControlPlaneEndpointsConfigIpEndpointsConfigArgs{
				Enabled: pulumi.Bool(false),
			},
		},

		// The provider defaults this to true, which would make turning
		// gitea:runnersEnabled off -- or any replacement -- fail at delete time.
		// The cluster holds nothing that is not recreated by the next `up`.
		DeletionProtection: pulumi.Bool(false),

		// Node upgrades interrupt running jobs. Keep them in the same Sunday
		// slot as the VM's reboot window, when CI is least likely to be busy.
		MaintenancePolicy: &container.ClusterMaintenancePolicyArgs{
			RecurringWindow: &container.ClusterMaintenancePolicyRecurringWindowArgs{
				StartTime:  pulumi.String("2026-01-04T05:00:00Z"),
				EndTime:    pulumi.String("2026-01-04T09:00:00Z"),
				Recurrence: pulumi.String("FREQ=WEEKLY;BYDAY=SU"),
			},
		},

		ResourceLabels: pulumi.StringMap{"app": pulumi.String("gitea")},
	}, opts,
		// The name is fixed, so a replacement cannot create the new cluster
		// before deleting the old one.
		pulumi.DeleteBeforeReplace(true),
		// These describe the default pool, which no longer exists after
		// creation. Changes to them are force-new and would replace the cluster
		// for nothing.
		pulumi.IgnoreChanges([]string{"nodeConfig", "initialNodeCount"}),
	)
	if err != nil {
		return nil, err
	}

	minNodes := max(1, cfg.RunnerReplicas)
	pool, err := container.NewNodePool(ctx, "gitea-runners-pool", &container.NodePoolArgs{
		// No Name: Pulumi auto-names it, so force-new changes (toggling spot,
		// changing machine type) create the new pool before deleting the old
		// one, and the runners reschedule instead of going dark.
		Cluster:  cluster.ID(),
		Location: pulumi.String(cfg.Zone),

		// The floor is one node per DinD runner replica (pod anti-affinity
		// allows no more per node), and never fewer than one: the runners have
		// to stay up to poll Gitea. Starting the pool at that size means a
		// first apply with several replicas does not hang the StatefulSet await
		// on node provisioning. Above the floor the pool follows the job queue:
		// the pod runner's job pods go Pending when no node has room for their
		// requests, and the autoscaler adds a node; it removes one once no job
		// pod is left on it.
		InitialNodeCount: pulumi.Int(minNodes),
		Autoscaling: &container.NodePoolAutoscalingArgs{
			MinNodeCount: pulumi.Int(minNodes),
			MaxNodeCount: pulumi.Int(cfg.RunnerMaxNodes),
		},
		Management: &container.NodePoolManagementArgs{
			AutoRepair:  pulumi.Bool(true),
			AutoUpgrade: pulumi.Bool(true),
		},
		MaxPodsPerNode: pulumi.Int(32),
		UpgradeSettings: &container.NodePoolUpgradeSettingsArgs{
			Strategy:       pulumi.String("SURGE"),
			MaxSurge:       pulumi.Int(1),
			MaxUnavailable: pulumi.Int(0),
		},

		NodeConfig: &container.NodePoolNodeConfigArgs{
			MachineType: pulumi.String(cfg.RunnerMachineType),
			// Spot by default: roughly a third of the on-demand price, at the
			// cost of the occasional preempted job, which can simply be re-run.
			Spot:      pulumi.Bool(cfg.RunnerSpot),
			ImageType: pulumi.String("COS_CONTAINERD"),

			ServiceAccount: nodeSA.Email,
			OauthScopes:    pulumi.ToStringArray([]string{"https://www.googleapis.com/auth/cloud-platform"}),

			// Job images and build layers live on the runner's PVCs, not here.
			DiskType:   pulumi.String("pd-balanced"),
			DiskSizeGb: pulumi.Int(30),

			// GKE_METADATA is what puts the GKE metadata server in front of
			// pods. Without it, pods would see the node's real metadata server
			// and could take the node SA's token.
			WorkloadMetadataConfig: &container.NodePoolNodeConfigWorkloadMetadataConfigArgs{
				Mode: pulumi.String("GKE_METADATA"),
			},
			ShieldedInstanceConfig: &container.NodePoolNodeConfigShieldedInstanceConfigArgs{
				EnableSecureBoot:          pulumi.Bool(true),
				EnableIntegrityMonitoring: pulumi.Bool(true),
			},
			KubeletConfig: &container.NodePoolNodeConfigKubeletConfigArgs{
				// A string, not a bool, in this provider.
				InsecureKubeletReadonlyPortEnabled: pulumi.String("FALSE"),
			},
			ResourceLabels: pulumi.StringMap{"app": pulumi.String("gitea")},
		},
	}, opts,
		// Nodes created before the role binding lands cannot write logs, and
		// GKE marks the pool unhealthy.
		pulumi.DependsOn([]pulumi.Resource{nodeRole}),
		// The autoscaler owns the node count after creation.
		pulumi.IgnoreChanges([]string{"initialNodeCount"}),
	)
	if err != nil {
		return nil, err
	}

	return &Cluster{Cluster: cluster, NodePool: pool, NodeSA: nodeSA}, nil
}
