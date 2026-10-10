// Package iam creates the workload identities and grants them the narrowest set
// of roles that still works.
//
// Note what is NOT here: the Cloud Build service account that runs Pulumi
// itself. That one is created by scripts/bootstrap.sh, because it is the
// identity performing the very `pulumi up` that would create it.
package iam

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
)

type Accounts struct {
	// VM runs the containers; it reads secrets, pulls images, writes backups,
	// and answers ACME DNS-01 challenges.
	VM *serviceaccount.Account
	// Image builds and pushes container images, then triggers the rollout over
	// an IAP tunnel.
	Image *serviceaccount.Account
}

func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) (*Accounts, error) {
	opts := pulumi.DependsOn(deps)

	vm, err := serviceaccount.NewAccount(ctx, "gitea-vm-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("gitea-vm"),
		DisplayName: pulumi.String("Gitea VM"),
		Description: pulumi.String("Workload identity for the Gitea instance"),
	}, opts)
	if err != nil {
		return nil, err
	}

	img, err := serviceaccount.NewAccount(ctx, "cb-image-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("cb-image"),
		DisplayName: pulumi.String("Cloud Build: images"),
		Description: pulumi.String("Builds and pushes container images, then rolls them out"),
	}, opts)
	if err != nil {
		return nil, err
	}

	// Observability only. Everything with real blast radius is granted per
	// resource further down.
	vmProjectRoles := []string{
		"roles/logging.logWriter",
		"roles/monitoring.metricWriter",
	}
	for _, role := range vmProjectRoles {
		if _, err := projects.NewIAMMember(ctx, "vm-"+role, &projects.IAMMemberArgs{
			Project: pulumi.String(cfg.Project),
			Role:    pulumi.String(role),
			Member:  pulumi.Sprintf("serviceAccount:%s", vm.Email),
		}, opts); err != nil {
			return nil, err
		}
	}

	imageProjectRoles := []string{
		"roles/logging.logWriter",
		// Reaching the VM without a public SSH port.
		"roles/iap.tunnelResourceAccessor",
		// Needed to push an ephemeral SSH key via OS Login; osLogin (non-admin)
		// is not enough because the rollout runs `sudo systemctl`.
		"roles/compute.osAdminLogin",
	}
	for _, role := range imageProjectRoles {
		if _, err := projects.NewIAMMember(ctx, "img-"+role, &projects.IAMMemberArgs{
			Project: pulumi.String(cfg.Project),
			Role:    pulumi.String(role),
			Member:  pulumi.Sprintf("serviceAccount:%s", img.Email),
		}, opts); err != nil {
			return nil, err
		}
	}

	// OS Login on an instance that runs as gitea-vm@ requires the caller to be
	// able to act as that account.
	if _, err := serviceaccount.NewIAMMember(ctx, "img-act-as-vm", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: vm.Name,
		Role:             pulumi.String("roles/iam.serviceAccountUser"),
		Member:           pulumi.Sprintf("serviceAccount:%s", img.Email),
	}, opts); err != nil {
		return nil, err
	}

	return &Accounts{VM: vm, Image: img}, nil
}

// GrantRegistry scopes image pull/push to the one repository rather than
// granting project-wide Artifact Registry roles.
func GrantRegistry(ctx *pulumi.Context, cfg *config.Config, a *Accounts, repo *artifactregistry.Repository) error {
	if _, err := artifactregistry.NewRepositoryIamMember(ctx, "vm-ar-reader", &artifactregistry.RepositoryIamMemberArgs{
		Project:    pulumi.String(cfg.Project),
		Location:   repo.Location,
		Repository: repo.Name,
		Role:       pulumi.String("roles/artifactregistry.reader"),
		Member:     pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	}); err != nil {
		return err
	}
	_, err := artifactregistry.NewRepositoryIamMember(ctx, "img-ar-writer", &artifactregistry.RepositoryIamMemberArgs{
		Project:    pulumi.String(cfg.Project),
		Location:   repo.Location,
		Repository: repo.Name,
		Role:       pulumi.String("roles/artifactregistry.writer"),
		Member:     pulumi.Sprintf("serviceAccount:%s", a.Image.Email),
	})
	return err
}

// GrantSecrets gives the VM read access to each Gitea secret individually --
// not roles/secretmanager.secretAccessor across the project.
//
// Takes ids rather than resources because the secrets are created by
// scripts/bootstrap.sh, not by Pulumi; see package secrets for why.
func GrantSecrets(ctx *pulumi.Context, cfg *config.Config, a *Accounts, names []string) error {
	for _, name := range names {
		if err := GrantSecretRead(ctx, cfg, a, name); err != nil {
			return err
		}
		// vm/bootstrap.sh's safety net adds a version if one is somehow missing.
		if _, err := secretmanager.NewSecretIamMember(ctx, "vm-add-"+name, &secretmanager.SecretIamMemberArgs{
			Project:  pulumi.String(cfg.Project),
			SecretId: pulumi.String(name),
			Role:     pulumi.String("roles/secretmanager.secretVersionAdder"),
			Member:   pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
		}); err != nil {
			return err
		}
	}
	return nil
}

// GrantSecretRead gives the VM read-only access to one secret. On its own it is
// for operator-supplied values the VM must never write; GrantSecrets adds
// version-adder on top for the ones it may generate.
func GrantSecretRead(ctx *pulumi.Context, cfg *config.Config, a *Accounts, name string) error {
	_, err := secretmanager.NewSecretIamMember(ctx, "vm-read-"+name, &secretmanager.SecretIamMemberArgs{
		Project:  pulumi.String(cfg.Project),
		SecretId: pulumi.String(name),
		Role:     pulumi.String("roles/secretmanager.secretAccessor"),
		Member:   pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	})
	return err
}

// GrantBuildSource lets cb-image@ read the tarballs `make build` stages, and
// nothing else in storage. See storage.New for why the bucket exists.
func GrantBuildSource(ctx *pulumi.Context, a *Accounts, sourceBucket *storage.Bucket) error {
	_, err := storage.NewBucketIAMMember(ctx, "img-build-source-reader", &storage.BucketIAMMemberArgs{
		Bucket: sourceBucket.Name,
		Role:   pulumi.String("roles/storage.objectViewer"),
		Member: pulumi.Sprintf("serviceAccount:%s", a.Image.Email),
	})
	return err
}

// GrantBuckets: read-only on config, write-only on backups. The VM can create a
// backup but cannot read or delete existing ones, which limits what ransomware
// on the box could do to the backup history.
func GrantBuckets(ctx *pulumi.Context, a *Accounts, configBucket, backupBucket *storage.Bucket) error {
	if _, err := storage.NewBucketIAMMember(ctx, "vm-config-reader", &storage.BucketIAMMemberArgs{
		Bucket: configBucket.Name,
		Role:   pulumi.String("roles/storage.objectViewer"),
		Member: pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	}); err != nil {
		return err
	}
	_, err := storage.NewBucketIAMMember(ctx, "vm-backup-creator", &storage.BucketIAMMemberArgs{
		Bucket: backupBucket.Name,
		Role:   pulumi.String("roles/storage.objectCreator"),
		Member: pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	})
	return err
}
