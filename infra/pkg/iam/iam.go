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
	// CaddyDNS is Caddy's ACME DNS-01 identity and nothing else: package dns
	// grants it the challenge records in the one zone. It has no key; gitea-vm@
	// mints its short-lived tokens. Caddy does not use it yet.
	CaddyDNS *serviceaccount.Account
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

	// Kept apart from gitea-vm@, which also reads Gitea's secrets, so a stolen
	// Caddy token can change records in the one zone and nothing more: no
	// secret, no IAM, not the zone itself. It gets no key: a timer on the VM
	// mints its tokens as gitea-vm@ (below), and they expire within the hour.
	caddyDNS, err := serviceaccount.NewAccount(ctx, "gitea-caddy-dns-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("gitea-caddy-dns"),
		DisplayName: pulumi.String("Caddy ACME DNS-01"),
		Description: pulumi.String("Caddy's ACME DNS-01 identity: challenge records in the Gitea zone, nothing else"),
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

	// The Caddy account's only member: gitea-vm@ may mint its access tokens,
	// which a host timer hands to Caddy. Anyone added here could write DNS
	// records.
	if _, err := serviceaccount.NewIAMMember(ctx, "vm-mints-caddy-dns", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: caddyDNS.Name,
		Role:             pulumi.String("roles/iam.serviceAccountTokenCreator"),
		Member:           pulumi.Sprintf("serviceAccount:%s", vm.Email),
	}, opts); err != nil {
		return nil, err
	}

	return &Accounts{VM: vm, Image: img, CaddyDNS: caddyDNS}, nil
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
// Read only: the VM cannot add a version. scripts/bootstrap.sh makes these,
// and the next deploy installs whatever version is newest, so a version the
// VM added would replace them -- and a new SECRET_KEY leaves everything Gitea
// encrypted with the old one unreadable. A missing one fails the app.ini
// render instead (vm/ansible/roles/app/files/gitea-render-secrets).
//
// Takes ids rather than resources because the secrets are created by
// scripts/bootstrap.sh, not by Pulumi; see package secrets for why.
func GrantSecrets(ctx *pulumi.Context, cfg *config.Config, a *Accounts, names []string) error {
	for _, name := range names {
		if err := GrantSecretRead(ctx, cfg, a, name); err != nil {
			return err
		}
	}
	return nil
}

// GrantSecretRead gives the VM read-only access to one secret. The VM writes
// to none of its secrets.
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

// GrantBuckets: read-only on config; create, get and list on backups. The VM
// can add a backup but cannot overwrite or delete existing ones (overwriting
// needs storage.objects.delete), which limits what ransomware on the box could
// do to the backup history. The bucket is versioned on top of that.
//
// Why not plain roles/storage.objectCreator: before it uploads, `gcloud
// storage cp` reads the destination object (storage.objects.get) and lists the
// bucket (storage.objects.list), so with create alone every upload is a 403.
// That is how the nightly gitea dump failed from day one without anyone
// noticing. The price is that the VM can read old backups -- of data it holds
// live anyway. What matters against ransomware, no delete and no overwrite,
// is unchanged.
func GrantBuckets(ctx *pulumi.Context, cfg *config.Config, a *Accounts, configBucket, backupBucket *storage.Bucket) error {
	if _, err := storage.NewBucketIAMMember(ctx, "vm-config-reader", &storage.BucketIAMMemberArgs{
		Bucket: configBucket.Name,
		Role:   pulumi.String("roles/storage.objectViewer"),
		Member: pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	}); err != nil {
		return err
	}
	role, err := projects.NewIAMCustomRole(ctx, "gitea-backup-writer", &projects.IAMCustomRoleArgs{
		Project:     pulumi.String(cfg.Project),
		RoleId:      pulumi.String("giteaBackupWriter"),
		Title:       pulumi.String("Gitea backup writer"),
		Description: pulumi.String("Add objects to the backup bucket; no overwrite or delete"),
		Permissions: pulumi.ToStringArray([]string{
			"storage.objects.create",
			"storage.objects.get",
			"storage.objects.list",
		}),
	})
	if err != nil {
		return err
	}
	_, err = storage.NewBucketIAMMember(ctx, "vm-backup-writer", &storage.BucketIAMMemberArgs{
		Bucket: backupBucket.Name,
		Role:   role.Name,
		Member: pulumi.Sprintf("serviceAccount:%s", a.VM.Email),
	})
	return err
}
