// Package identity holds the CI project's service accounts and every grant
// they get, here and in the app project. Each grant into the app project is a
// non-authoritative IAMMember, so this stack never fights infra/ over a
// policy, and `pulumi destroy` here removes exactly what it added.
//
// Who runs what:
//
//	cb-launcher     every trigger's own build: clones the commit, then
//	                submits cloudbuild/<pipeline>.yaml as one of the two below
//	cb-infra        cloudbuild/infra.yaml: pulumi up on the app stack, the
//	                config sync, and the backup and verify Jobs around them
//	cb-image        cloudbuild/image.yaml: builds and pushes images, then
//	                launches the release Job (scripts/ops/launch_job.py)
//	ci-relay        the public gitea-webhook function
//	ci-status       the private build-status function
//	ci-status-push  the identity the cloud-builds push subscription signs as
//	ci-fn-build     builds the two functions' images
//
// The release itself never runs as any of these. cb-image only creates the
// Job; the engine runs as the app project's gitea-release@ on the ops pool,
// the identity a release has always run as (infra/pkg/ops). cb-image's
// Kubernetes rights in the gitea-ops namespace come from there too
// (gitea:opsLaunchers), because that stack owns the cluster's provider.
//
// A pull request's preview runs as cb-infra and cb-image as well: a preview
// executes the PR's code, so its identity can do what that code does. That is
// why the relay previews only same-repo branches by gitea-cicd:allowedAuthors
// -- people who can already push to main, which deploys. An author who cannot
// push to main needs a read-only preview identity first.
package identity

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
	cistorage "gitea-cicd/pkg/storage"
)

// Secrets bootstrap.py creates; this stack only grants access to them.
const (
	WebhookSecret = "gitea-webhook-secret"
	StatusToken   = "gitea-ci-status-token"
)

type Accounts struct {
	Launcher, Infra, Image, Relay, Status, StatusPush, FnBuild *serviceaccount.Account
}

// infraRoles is what cb-infra needs on the app project: Pulumi manages IAM,
// compute, DNS, storage, the runner cluster and the Kubernetes objects in it.
// The same set scripts/bootstrap.sh gives the app project's own cb-infra@,
// plus the two that set lacked and every `pulumi up` since has needed from
// the operator instead: buckets (storage.admin) and custom roles
// (iam.roleAdmin).
var infraRoles = []string{
	"roles/compute.admin",
	"roles/dns.admin",
	"roles/artifactregistry.admin",
	"roles/secretmanager.admin",
	"roles/iam.serviceAccountAdmin",
	"roles/iam.serviceAccountUser",
	"roles/iam.roleAdmin",
	"roles/resourcemanager.projectIamAdmin",
	"roles/serviceusage.serviceUsageAdmin",
	"roles/storage.admin",
	"roles/cloudscheduler.admin",
	// Includes container.clusters.connect, which the cluster's IAM-only DNS
	// endpoint requires, and every Kubernetes object, gitea-ops included --
	// the backup and verify Jobs around a deploy.
	"roles/container.admin",
	"roles/cloudbuild.builds.editor",
	// The config sync: SSH to the VM through IAP, sudo via OS Login.
	"roles/iap.tunnelResourceAccessor",
	"roles/compute.osAdminLogin",
	"roles/logging.logWriter",
}

func New(ctx *pulumi.Context, cfg *config.Config, b *cistorage.Buckets, deps []pulumi.Resource) (*Accounts, error) {
	opts := pulumi.DependsOn(deps)
	sa := func(id, display, desc string) (*serviceaccount.Account, error) {
		return serviceaccount.NewAccount(ctx, id, &serviceaccount.AccountArgs{
			Project: pulumi.String(cfg.Project), AccountId: pulumi.String(id),
			DisplayName: pulumi.String(display), Description: pulumi.String(desc),
		}, opts)
	}
	var a Accounts
	var err error
	for _, s := range []struct {
		dst               **serviceaccount.Account
		id, display, desc string
	}{
		{&a.Launcher, "cb-launcher", "Cloud Build launcher", "Runs each trigger's build: clone, then submit the repo's pipeline"},
		{&a.Infra, "cb-infra", "Cloud Build infra pipeline", "pulumi up on the Gitea app stack (cloudbuild/infra.yaml)"},
		{&a.Image, "cb-image", "Cloud Build image pipeline", "Builds images and launches the release Job (cloudbuild/image.yaml)"},
		{&a.Relay, "ci-relay", "Gitea webhook relay", "Runtime of the gitea-webhook function"},
		{&a.Status, "ci-status", "Build status relay", "Runtime of the build-status function"},
		{&a.StatusPush, "ci-status-push", "Build status push", "Signs the cloud-builds push subscription's requests"},
		{&a.FnBuild, "ci-fn-build", "Relay function builds", "Builds the relay functions' images"},
	} {
		if *s.dst, err = sa(s.id, s.display, s.desc); err != nil {
			return nil, err
		}
	}

	member := func(acct *serviceaccount.Account) pulumi.StringOutput {
		return pulumi.Sprintf("serviceAccount:%s", acct.Email)
	}
	projectRole := func(name, project, role string, m pulumi.StringInput) error {
		_, err := projects.NewIAMMember(ctx, name, &projects.IAMMemberArgs{
			Project: pulumi.String(project), Role: pulumi.String(role), Member: m,
		}, opts)
		return err
	}
	bucketRole := func(name string, bucket *storage.Bucket, role string, m pulumi.StringInput) error {
		_, err := storage.NewBucketIAMMember(ctx, name, &storage.BucketIAMMemberArgs{
			Bucket: bucket.Name, Role: pulumi.String(role), Member: m,
		}, opts)
		return err
	}

	// --- In this project ---

	for _, g := range []struct{ name, role string }{
		{"launcher-builds", "roles/cloudbuild.builds.editor"},
		{"launcher-logs", "roles/logging.logWriter"},
		// gcloud builds submit follows the submitted build's log from Cloud
		// Logging, because these builds log nowhere else.
		{"launcher-log-reader", "roles/logging.viewer"},
	} {
		if err := projectRole(g.name, cfg.Project, g.role, member(a.Launcher)); err != nil {
			return nil, err
		}
	}
	// The launcher uploads the source tarball; gcloud also reads the bucket.
	if err := bucketRole("launcher-source-writer", b.BuildSource, "roles/storage.objectAdmin", member(a.Launcher)); err != nil {
		return nil, err
	}
	if err := bucketRole("launcher-source-bucket", b.BuildSource, "roles/storage.legacyBucketReader", member(a.Launcher)); err != nil {
		return nil, err
	}
	// Submitting a build as cb-infra or cb-image is acting as them.
	for name, target := range map[string]*serviceaccount.Account{"launcher-as-infra": a.Infra, "launcher-as-image": a.Image} {
		if _, err := serviceaccount.NewIAMMember(ctx, name, &serviceaccount.IAMMemberArgs{
			ServiceAccountId: target.Name, Role: pulumi.String("roles/iam.serviceAccountUser"), Member: member(a.Launcher),
		}, opts); err != nil {
			return nil, err
		}
	}

	for _, p := range []struct {
		prefix string
		acct   *serviceaccount.Account
	}{{"infra", a.Infra}, {"image", a.Image}} {
		if err := projectRole(p.prefix+"-logs", cfg.Project, "roles/logging.logWriter", member(p.acct)); err != nil {
			return nil, err
		}
		// Cloud Build fetches a build's source as the build's own identity.
		if err := bucketRole(p.prefix+"-source-reader", b.BuildSource, "roles/storage.objectViewer", member(p.acct)); err != nil {
			return nil, err
		}
	}
	// The infra deploy lock and the last-good marker and tarball.
	if err := bucketRole("infra-deploy-state", b.DeployState, "roles/storage.objectAdmin", member(a.Infra)); err != nil {
		return nil, err
	}

	if err := projectRole("fn-build-builder", cfg.Project, "roles/cloudbuild.builds.builder", member(a.FnBuild)); err != nil {
		return nil, err
	}

	// Approving a gated build is a person's job, and only this person's.
	if err := projectRole("operator-approver", cfg.Project, "roles/cloudbuild.builds.approver",
		pulumi.ToSecret(pulumi.String("user:"+cfg.OperatorEmail)).(pulumi.StringOutput)); err != nil {
		return nil, err
	}

	for _, g := range []struct {
		name, secret string
		acct         *serviceaccount.Account
	}{
		{"relay-webhook-secret", WebhookSecret, a.Relay},
		// The relay posts "nothing to deploy" itself.
		{"relay-status-token", StatusToken, a.Relay},
		{"status-status-token", StatusToken, a.Status},
	} {
		if _, err := secretmanager.NewSecretIamMember(ctx, g.name, &secretmanager.SecretIamMemberArgs{
			Project: pulumi.String(cfg.Project), SecretId: pulumi.String(g.secret),
			Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: member(g.acct),
		}, opts); err != nil {
			return nil, err
		}
	}

	// --- In the app project ---

	for _, role := range infraRoles {
		if err := projectRole("app-infra-"+role, cfg.AppProject, role, member(a.Infra)); err != nil {
			return nil, err
		}
	}

	// Push images: the same repository-scoped grant the app project's own
	// cb-image@ has. Tags it pushes are :<sha> and :<version>, never :prod --
	// moving :prod is the release Job's.
	if _, err := artifactregistry.NewRepositoryIamMember(ctx, "app-image-ar-writer", &artifactregistry.RepositoryIamMemberArgs{
		Project: pulumi.String(cfg.AppProject), Location: pulumi.String(cfg.Region), Repository: pulumi.String("gitea"),
		Role: pulumi.String("roles/artifactregistry.writer"), Member: member(a.Image),
	}, opts); err != nil {
		return nil, err
	}
	// Find the runner cluster's DNS endpoint and connect to it. What it may
	// do inside is a namespaced Role in gitea-ops (infra/pkg/ops).
	if err := projectRole("app-image-cluster-viewer", cfg.AppProject, "roles/container.clusterViewer", member(a.Image)); err != nil {
		return nil, err
	}

	return &a, nil
}
