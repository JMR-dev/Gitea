// Package compute builds the data disk, its snapshot schedule, and the instance.
package compute

import (
	"fmt"
	"os"

	gcpcompute "github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/network"
	"gitea-infra/pkg/registry"
)

// DataDiskDeviceName is what the disk shows up as under /dev/disk/by-id/google-*
// inside the guest. vm/bootstrap.sh mounts it by UUID, but it needs this name to
// find the device the first time, before there is a filesystem to have a UUID.
const DataDiskDeviceName = "gitea-data"

type Instance struct {
	VM   *gcpcompute.Instance
	Disk *gcpcompute.Disk
}

func New(
	ctx *pulumi.Context,
	cfg *config.Config,
	net *network.Network,
	sa *serviceaccount.Account,
	buckets *storage.Bucket,
	backupBucket *storage.Bucket,
	configHash string,
	bootstrapPath string,
	deps []pulumi.Resource,
) (*Instance, error) {
	opts := pulumi.DependsOn(deps)

	// A separate disk from the boot disk, so replacing the instance -- for a
	// machine-type change, an image refresh, anything -- never touches the
	// repositories. Protect(true) makes an accidental delete a two-step mistake.
	//
	// This is the disk that holds everything you would miss: repositories, the
	// SQLite database, and LFS objects. gitea:dataDiskGb sizes it. The boot disk
	// is separate and sized by gitea:bootDiskGb; growing that one buys nothing.
	// A pd-balanced disk can be grown in place later, never shrunk.
	disk, err := gcpcompute.NewDisk(ctx, "gitea-data", &gcpcompute.DiskArgs{
		Name:        pulumi.String("gitea-data"),
		Zone:        pulumi.String(cfg.Zone),
		Size:        pulumi.Int(cfg.DataDiskGB),
		Type:        pulumi.String("pd-balanced"),
		Description: pulumi.String("Gitea repositories, SQLite database, and LFS objects"),
	}, opts, pulumi.Protect(true))
	if err != nil {
		return nil, err
	}

	// The safety net for the weekly unattended-reboot window, and for everything
	// else that goes wrong at 3am.
	policy, err := gcpcompute.NewResourcePolicy(ctx, "gitea-snapshots", &gcpcompute.ResourcePolicyArgs{
		Name:   pulumi.String("gitea-daily-snapshots"),
		Region: pulumi.String(cfg.Region),
		SnapshotSchedulePolicy: &gcpcompute.ResourcePolicySnapshotSchedulePolicyArgs{
			Schedule: &gcpcompute.ResourcePolicySnapshotSchedulePolicyScheduleArgs{
				DailySchedule: &gcpcompute.ResourcePolicySnapshotSchedulePolicyScheduleDailyScheduleArgs{
					DaysInCycle: pulumi.Int(1),
					StartTime:   pulumi.String("03:00"),
				},
			},
			RetentionPolicy: &gcpcompute.ResourcePolicySnapshotSchedulePolicyRetentionPolicyArgs{
				MaxRetentionDays: pulumi.Int(14),
				// Keep the history even if the disk itself is deleted.
				OnSourceDiskDelete: pulumi.String("KEEP_AUTO_SNAPSHOTS"),
			},
			SnapshotProperties: &gcpcompute.ResourcePolicySnapshotSchedulePolicySnapshotPropertiesArgs{
				StorageLocations: pulumi.String(cfg.Region),
				Labels:           pulumi.StringMap{"app": pulumi.String("gitea")},
			},
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	if _, err := gcpcompute.NewDiskResourcePolicyAttachment(ctx, "gitea-data-snapshots", &gcpcompute.DiskResourcePolicyAttachmentArgs{
		Name: policy.Name,
		Disk: disk.Name,
		Zone: pulumi.String(cfg.Zone),
	}, opts); err != nil {
		return nil, err
	}

	startupScript, err := os.ReadFile(bootstrapPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", bootstrapPath, err)
	}

	imageGitea := fmt.Sprintf("%s/%s/%s/gitea:prod", cfg.ARHost(), cfg.Project, registry.RepoID)
	imageCaddy := fmt.Sprintf("%s/%s/%s/caddy:prod", cfg.ARHost(), cfg.Project, registry.RepoID)

	metadata := pulumi.StringMap{
		// OS Login means SSH access follows IAM instead of metadata keys.
		"enable-oslogin":         pulumi.String("TRUE"),
		"google-logging-enabled": pulumi.String("true"),

		"config-bucket": buckets.Name,
		"backup-bucket": backupBucket.Name,
		"gcp-project":   pulumi.String(cfg.Project),
		"gcp-region":    pulumi.String(cfg.Region),
		"ar-host":       pulumi.String(cfg.ARHost()),

		// Floating :prod tags -- AutoUpdate=registry has nothing to poll if
		// these are digests.
		"image-gitea": pulumi.String(imageGitea),
		"image-caddy": pulumi.String(imageCaddy),

		"domain":              pulumi.String(cfg.Domain),
		"app-name":            pulumi.String(cfg.AppName),
		"require-signin-view": pulumi.String(cfg.RequireSigninView),

		// Drives Coraza's SecRuleEngine and, from the same value, whether
		// the fail2ban jail acting on WAF verdicts is enabled.
		"waf-mode": pulumi.String(cfg.WAFMode),

		"podman-subnet":  pulumi.String(cfg.PodmanCIDR),
		"podman-gateway": pulumi.String(gatewayFor(cfg.PodmanCIDR)),

		"data-disk-device": pulumi.String("/dev/disk/by-id/google-" + DataDiskDeviceName),

		// Not read by the guest -- it exists so `gcloud compute instances
		// describe` shows which revision of vm/ the box was built against.
		"config-hash": pulumi.String(configHash),
	}
	if net.Runners != nil {
		// The Cloud NAT address every GKE runner and CI job egresses from.
		// vm/bootstrap.sh puts it in fail2ban's ignoreip: one misconfigured job
		// retrying a bad token would otherwise ban all of CI at once. Omitted,
		// not empty, when runners are off -- meta() already returns "" on 404.
		metadata["runner-egress-ip"] = net.Runners.EgressIP.Address
	}

	vm, err := gcpcompute.NewInstance(ctx, "gitea-vm", &gcpcompute.InstanceArgs{
		Name:        pulumi.String("gitea-vm"),
		Zone:        pulumi.String(cfg.Zone),
		MachineType: pulumi.String(cfg.MachineType),
		Tags:        pulumi.ToStringArray([]string{network.Tag}),
		Description: pulumi.String("Gitea, Caddy, and podman quadlets on AlmaLinux 10"),

		BootDisk: &gcpcompute.InstanceBootDiskArgs{
			InitializeParams: &gcpcompute.InstanceBootDiskInitializeParamsArgs{
				// Image family, not a pinned image: new VMs pick up the monthly
				// refreshed AlmaLinux build automatically.
				Image: pulumi.String("almalinux-cloud/almalinux-10"),
				Size:  pulumi.Int(cfg.BootDiskGB),
				Type:  pulumi.String("pd-balanced"),
			},
		},

		AttachedDisks: gcpcompute.InstanceAttachedDiskArray{
			&gcpcompute.InstanceAttachedDiskArgs{
				Source:     disk.ID(),
				DeviceName: pulumi.String(DataDiskDeviceName),
				Mode:       pulumi.String("READ_WRITE"),
			},
		},

		NetworkInterfaces: gcpcompute.InstanceNetworkInterfaceArray{
			&gcpcompute.InstanceNetworkInterfaceArgs{
				Network:    net.Network.ID(),
				Subnetwork: net.Subnetwork.ID(),
				AccessConfigs: gcpcompute.InstanceNetworkInterfaceAccessConfigArray{
					&gcpcompute.InstanceNetworkInterfaceAccessConfigArgs{
						NatIp: net.Address.Address,
					},
				},
			},
		},

		ServiceAccount: &gcpcompute.InstanceServiceAccountArgs{
			Email: sa.Email,
			// cloud-platform plus per-resource IAM, rather than a hand-rolled
			// scope list: scopes are a coarse legacy filter and IAM is where the
			// real restrictions live.
			Scopes: pulumi.ToStringArray([]string{"cloud-platform"}),
		},

		ShieldedInstanceConfig: &gcpcompute.InstanceShieldedInstanceConfigArgs{
			EnableSecureBoot:          pulumi.Bool(true),
			EnableVtpm:                pulumi.Bool(true),
			EnableIntegrityMonitoring: pulumi.Bool(true),
		},

		Metadata: metadata,

		MetadataStartupScript: pulumi.String(string(startupScript)),

		// Lets Pulumi change machine type or metadata in place instead of
		// refusing the update.
		AllowStoppingForUpdate: pulumi.Bool(true),

		Labels: pulumi.StringMap{"app": pulumi.String("gitea")},
	}, opts)
	if err != nil {
		return nil, err
	}

	return &Instance{VM: vm, Disk: disk}, nil
}

// gatewayFor returns the .1 address of a /24. The podman network's gateway has
// to be stated explicitly alongside the pinned subnet.
func gatewayFor(cidr string) string {
	var a, b, c, d, bits int
	if _, err := fmt.Sscanf(cidr, "%d.%d.%d.%d/%d", &a, &b, &c, &d, &bits); err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.1", a, b, c)
}
