// Command gitea-infra provisions the Gitea deployment: a single AlmaLinux 10
// VM running podman quadlets, fronted by Caddy with ACME DNS-01 against an
// existing Cloud DNS zone, with images built and rolled out by Cloud Build.
//
// The program is deliberately thin. Each package owns one slice of the
// infrastructure and the wiring order below is the dependency order.
package main

import (
	"path/filepath"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/build"
	"gitea-infra/pkg/compute"
	"gitea-infra/pkg/config"
	"gitea-infra/pkg/dns"
	"gitea-infra/pkg/iam"
	"gitea-infra/pkg/network"
	"gitea-infra/pkg/project"
	"gitea-infra/pkg/registry"
	"gitea-infra/pkg/secrets"
	"gitea-infra/pkg/storage"
)

// The vm/ tree and its bootstrap script live one level up from infra/.
const vmDir = "../vm"

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg, err := config.Load(ctx)
		if err != nil {
			return err
		}

		// Everything depends on these: a resource created against an API that is
		// still enabling fails in confusing ways.
		apis, err := project.EnableAPIs(ctx)
		if err != nil {
			return err
		}

		net, err := network.New(ctx, cfg, apis)
		if err != nil {
			return err
		}

		accounts, err := iam.New(ctx, cfg, apis)
		if err != nil {
			return err
		}

		repo, err := registry.New(ctx, cfg, apis)
		if err != nil {
			return err
		}
		if err := iam.GrantRegistry(ctx, cfg, accounts, repo); err != nil {
			return err
		}

		// The secrets themselves are created by scripts/bootstrap.sh; Pulumi
		// only grants access to them.
		if err := iam.GrantSecrets(ctx, cfg, accounts, secrets.Names); err != nil {
			return err
		}
		if err := iam.GrantSecretRead(ctx, cfg, accounts, secrets.ACMEEmail); err != nil {
			return err
		}

		buckets, err := storage.New(ctx, cfg, vmDir, apis)
		if err != nil {
			return err
		}
		if err := iam.GrantBuckets(ctx, accounts, buckets.Config, buckets.Backup); err != nil {
			return err
		}
		if err := iam.GrantBuildSource(ctx, accounts, buckets.BuildSource); err != nil {
			return err
		}

		// The zone already exists and is delegated; this only adds the A record
		// and the zone-scoped permission Caddy needs for DNS-01.
		if _, err := dns.New(ctx, cfg, net.Address, accounts.VM.Email, apis); err != nil {
			return err
		}

		inst, err := compute.New(ctx, cfg, net, accounts.VM,
			buckets.Config, buckets.Backup, buckets.ConfigHash,
			filepath.Join(vmDir, "bootstrap.sh"), apis)
		if err != nil {
			return err
		}

		if _, err := build.New(ctx, cfg, accounts.Image, apis); err != nil {
			return err
		}

		ctx.Export("address", net.Address.Address)
		ctx.Export("url", pulumi.Sprintf("https://%s/", cfg.Domain))
		ctx.Export("cloneSSH", pulumi.Sprintf("ssh://git@%s:2222/", cfg.Domain))
		ctx.Export("instance", inst.VM.Name)
		ctx.Export("zone", pulumi.String(cfg.Zone))
		ctx.Export("registry", pulumi.Sprintf("%s/%s/%s", cfg.ARHost(), cfg.Project, registry.RepoID))
		ctx.Export("configBucket", buckets.Config.Name)
		ctx.Export("backupBucket", buckets.Backup.Name)
		ctx.Export("buildSourceBucket", buckets.BuildSource.Name)
		ctx.Export("configHash", pulumi.String(buckets.ConfigHash))
		ctx.Export("vmServiceAccount", accounts.VM.Email)
		ctx.Export("imageServiceAccount", accounts.Image.Email)
		// Printed so the runbook never has to guess the flags.
		ctx.Export("sshCommand", pulumi.Sprintf(
			"gcloud compute ssh %s --zone=%s --tunnel-through-iap --project=%s",
			inst.VM.Name, cfg.Zone, cfg.Project))

		return nil
	})
}
