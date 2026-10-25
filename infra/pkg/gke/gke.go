// Package gke builds the cluster that runs Gitea Actions jobs: one zonal GKE
// Standard cluster, used for nothing else, with three node pools:
//
//   - base: a fixed number of on-demand nodes for both runners and all of
//     GKE's own pods. Never autoscaled.
//   - burst: tainted, scaled from zero for ubuntu-* job pods and nothing else.
//   - heavy: tainted, scaled from zero for `runs-on: heavy` job pods.
//
// Why the runners and GKE's pods get a pool nothing scales down: every step of
// a pod-runner job is an exec stream from the runner, through the API server
// and a konnectivity-agent pod (GKE's tunnel to the nodes), to the job pod's
// kubelet. The job dies if any of them goes away, including a tunnel pod on a
// node the job never touched. GKE marks the tunnel pods safe to evict and
// spreads them over every node, so with one autoscaled pool the autoscaler
// emptying an idle node killed jobs elsewhere. The taints keep tunnel pods
// off the burst and heavy pools, and package runner pins their count, which
// otherwise follows the number of nodes.
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
	"strconv"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
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

// PoolKey is the node label, and the NoSchedule taint, that marks the burst
// and heavy pools, with the values below. Every job pod tolerates the burst
// taint; `runs-on: heavy` job pods also select and tolerate the heavy one
// (package runner). Nothing else does -- not the runners, not GKE's own
// Deployments -- so between jobs both pools are empty and the autoscaler
// takes them back to zero.
const (
	PoolKey        = "gitea-runner-pool"
	BurstPoolValue = "burst"
	HeavyPoolValue = "heavy"
	// OpsPoolValue marks the pool release-engine Jobs run on (package ops).
	OpsPoolValue = "ops"
	// ArmPoolValue marks the arm64 pool, which only `runs-on: docker-arm64`
	// job pods select and tolerate (package runner).
	ArmPoolValue = "arm64"
)

type Cluster struct {
	Cluster *container.Cluster
	// NodePool is the base pool, where the runners run.
	NodePool *container.NodePool
	// BurstPool is nil when gitea:runnerBurstMaxNodes is 0, HeavyPool when
	// gitea:runnerHeavyMaxNodes is.
	BurstPool *container.NodePool
	HeavyPool *container.NodePool
	// OpsPool holds release-engine Jobs and nothing else (package ops).
	OpsPool *container.NodePool
	// ArmPool is nil when gitea:runnerArmMaxNodes is 0.
	ArmPool *container.NodePool
	NodeSA  *serviceaccount.Account
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

	pool, err := container.NewNodePool(ctx, "gitea-runners-pool", &container.NodePoolArgs{
		// No Name: Pulumi auto-names it, so force-new changes (changing the
		// machine type, say) create the new pool before deleting the old one,
		// and the runners reschedule instead of going dark.
		Cluster:  cluster.ID(),
		Location: pulumi.String(cfg.Zone),

		// Fixed, and never autoscaled: the autoscaler only drains nodes of
		// pools it scales, so nothing here -- the runners, the tunnel pods
		// every job's steps stream through -- is ever evicted to save a node.
		// Node upgrades and auto-repair still drain it.
		NodeCount: pulumi.Int(cfg.RunnerBaseNodes()),
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

		// On demand, always. The pod runner here is the runner for every
		// ubuntu-* job, and every job's steps stream through the tunnel pods
		// here, so preempting this node would fail every running job at once,
		// heavy ones included. The cost side is in docs/runners.md.
		NodeConfig: nodeConfig(nodeSA, cfg.RunnerMachineType, false),
	}, opts,
		// Nodes created before the role binding lands cannot write logs, and
		// GKE marks the pool unhealthy.
		pulumi.DependsOn([]pulumi.Resource{nodeRole}),
	)
	if err != nil {
		return nil, err
	}

	if err := baseCommitment(ctx, cfg, opts); err != nil {
		return nil, err
	}

	// Job pods go Pending when no node has room for their requests, and the
	// autoscaler adds a burst node; it removes one about 10 minutes after the
	// last job pod on it has finished. Spot only if asked for: a preempted
	// burst node fails just the jobs on it.
	var burst *container.NodePool
	if cfg.RunnerBurstMaxNodes > 0 {
		burst, err = taintedPool(ctx, "gitea-runners-burst-pool", BurstPoolValue, cfg.Zone,
			nodeConfig(nodeSA, cfg.RunnerMachineType, cfg.RunnerBurstSpot), cfg.RunnerBurstMaxNodes,
			cluster, opts, nodeRole)
		if err != nil {
			return nil, err
		}
	}

	// On demand, not spot, on purpose: this is where `pulumi up` runs, and a
	// preempted apply leaves a stack with pending operations to clear by hand.
	// A pool that exists only while a job runs makes the price difference
	// small.
	var heavy *container.NodePool
	if cfg.RunnerHeavyMaxNodes > 0 {
		heavy, err = taintedPool(ctx, "gitea-runners-heavy-pool", HeavyPoolValue, cfg.Zone,
			nodeConfig(nodeSA, cfg.RunnerHeavyMachineType, false), cfg.RunnerHeavyMaxNodes,
			cluster, opts, nodeRole)
		if err != nil {
			return nil, err
		}
	}

	// The release engine's disposable host. On demand for the same reason as
	// the heavy pool, and more so: a preempted node mid-release could leave
	// Gitea stopped with its timers paused. One node, because releases are
	// serialized anyway. The taint keeps CI pods off it, so a compromised CI
	// node never shares a kubelet -- or the node authorizer's view of tokens
	// -- with a pod holding the release identity.
	ops, err := taintedPool(ctx, "gitea-runners-ops-pool", OpsPoolValue, cfg.Zone,
		nodeConfig(nodeSA, cfg.OpsMachineType, false), 1, cluster, opts, nodeRole)
	if err != nil {
		return nil, err
	}

	// Native linux/arm64 builds, for `runs-on: docker-arm64`: Axion (C4A), the
	// only Arm series in this zone. Spot: a node exists only while a job runs,
	// and a preempted one fails just that job, which is re-run (nothing else
	// runs here, unlike the burst pool, which hosts no runner either but is on
	// demand by default). C4A takes no Persistent Disk, so the boot disk is Hyperdisk
	// Balanced -- and larger than the other pools', because each job's own
	// dockerd keeps its images and build layers there (an emptyDir). GKE taints
	// Arm nodes kubernetes.io/arch=arm64:NoSchedule on top of the pool's own
	// taint, so GKE's own Deployments can never land here either. A job that
	// escapes to one of these nodes shares a kubelet with other arm64 jobs and
	// GKE's DaemonSets, never with the runners or the ops pool.
	var arm *container.NodePool
	if cfg.RunnerArmMaxNodes > 0 {
		nc := nodeConfig(nodeSA, cfg.RunnerArmMachineType, true)
		nc.DiskType = pulumi.String("hyperdisk-balanced")
		nc.DiskSizeGb = pulumi.Int(50)
		arm, err = taintedPool(ctx, "gitea-runners-arm64-pool", ArmPoolValue, cfg.Zone,
			nc, cfg.RunnerArmMaxNodes, cluster, opts, nodeRole)
		if err != nil {
			return nil, err
		}
	}

	return &Cluster{Cluster: cluster, NodePool: pool, BurstPool: burst, HeavyPool: heavy, OpsPool: ops, ArmPool: arm, NodeSA: nodeSA}, nil
}

// baseCommitment buys the committed use discount gitea:runnerBaseCommitment
// asks for: one base node's worth of E2 vCPUs and memory in the region. It
// covers that much E2 usage in this project, not one particular node, so the
// base node uses all of it and burst, heavy and surge-upgrade nodes bill at
// the on-demand rate.
//
// A commitment is a purchase, not configuration: it cannot be cancelled,
// changed or deleted, and it bills every month of its term, used or not. So
// it is write-once here. Changes to its inputs are ignored -- a different
// machine type later must not mean buying a second commitment -- and Protect
// is the backstop: Pulumi refuses to replace or delete a protected resource
// before it creates anything. RetainOnDelete, because there is no delete to
// call: unprotecting and dropping it only removes it from state.
func baseCommitment(ctx *pulumi.Context, cfg *config.Config, opts pulumi.ResourceOption) error {
	plan := cfg.CommitmentPlan()
	if plan == "" {
		return nil
	}
	vcpus, memoryMB, err := config.E2Shape(cfg.RunnerMachineType)
	if err != nil {
		return err
	}
	_, err = compute.NewRegionCommitment(ctx, "gitea-runners-base-commitment", &compute.RegionCommitmentArgs{
		Name:        pulumi.String("gitea-runners-base"),
		Region:      pulumi.String(cfg.Region),
		Description: pulumi.String("One base node of the Gitea Actions runner cluster (docs/runners.md)"),
		Type:        pulumi.String("GENERAL_PURPOSE_E2"),
		Plan:        pulumi.String(plan),
		Resources: compute.RegionCommitmentResourceArray{
			&compute.RegionCommitmentResourceArgs{Type: pulumi.String("VCPU"), Amount: pulumi.String(strconv.Itoa(vcpus))},
			&compute.RegionCommitmentResourceArgs{Type: pulumi.String("MEMORY"), Amount: pulumi.String(strconv.Itoa(memoryMB))},
		},
		// Whether to keep the discount is decided when the term nears its end
		// (docs/runners.md), and the answer lives in the console, not here.
		AutoRenew: pulumi.Bool(false),
	}, opts,
		pulumi.Protect(true),
		pulumi.RetainOnDelete(true),
		pulumi.IgnoreChanges([]string{"description", "type", "plan", "resources", "autoRenew"}),
	)
	return err
}

// taintedPool is a node pool that scales from zero for the job pods that
// tolerate its taint, and holds nothing else. The taint keeps everything else
// -- the runners, GKE's own Deployments and with them the tunnel pods -- off
// it, so an idle pool really is empty, costs nothing but its configuration,
// and scaling it down never takes anything a running job depends on.
func taintedPool(
	ctx *pulumi.Context,
	name, poolValue, zone string,
	nc *container.NodePoolNodeConfigArgs,
	maxNodes int,
	cluster *container.Cluster,
	opts pulumi.ResourceOption,
	nodeRole pulumi.Resource,
) (*container.NodePool, error) {
	// Labels and taints are part of the pool's node template, which is what
	// lets the autoscaler scale it up from zero for a pod that tolerates or
	// selects them.
	nc.Labels = pulumi.StringMap{PoolKey: pulumi.String(poolValue)}
	nc.Taints = container.NodePoolNodeConfigTaintArray{
		&container.NodePoolNodeConfigTaintArgs{
			Key:    pulumi.String(PoolKey),
			Value:  pulumi.String(poolValue),
			Effect: pulumi.String("NO_SCHEDULE"),
		},
	}

	return container.NewNodePool(ctx, name, &container.NodePoolArgs{
		// Auto-named, like the base pool, so a machine type change creates
		// the new pool before deleting the old one.
		Cluster:  cluster.ID(),
		Location: pulumi.String(zone),

		InitialNodeCount: pulumi.Int(0),
		Autoscaling: &container.NodePoolAutoscalingArgs{
			MinNodeCount: pulumi.Int(0),
			MaxNodeCount: pulumi.Int(maxNodes),
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
		NodeConfig: nc,
	}, opts,
		pulumi.DependsOn([]pulumi.Resource{nodeRole}),
		pulumi.IgnoreChanges([]string{"initialNodeCount"}),
	)
}

// nodeConfig is what every runner node pool's machines share: the minimal
// node identity, COS, Shielded VM, and the GKE metadata server in front of pods.
func nodeConfig(nodeSA *serviceaccount.Account, machineType string, spot bool) *container.NodePoolNodeConfigArgs {
	return &container.NodePoolNodeConfigArgs{
		MachineType: pulumi.String(machineType),
		Spot:        pulumi.Bool(spot),
		ImageType:   pulumi.String("COS_CONTAINERD"),

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
	}
}
