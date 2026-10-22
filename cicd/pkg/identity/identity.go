// Package identity holds the CI project's service accounts and every grant
// they get, here and in the app project. Each grant into the app project is a
// non-authoritative IAMMember, so this stack never fights infra/ over a
// policy, and `pulumi destroy` here removes exactly what it added.
//
// Who runs what:
//
//	cb-launcher          the deploy triggers' own build: clones the commit,
//	                     then submits cloudbuild/<pipeline>.yaml as cb-infra
//	                     or cb-image
//	cb-infra             cloudbuild/infra.yaml: pulumi up on the app stack,
//	                     the config sync, and the backup and verify Jobs
//	                     around them
//	cb-image             cloudbuild/image.yaml: builds and pushes images, then
//	                     launches the release Job (scripts/ops/launch_job.py)
//	cb-launcher-preview  the preview trigger's build, the same launcher, which
//	                     can submit only as the two below
//	cb-infra-preview     a pull request's pulumi preview: reads, never writes
//	cb-image-preview     a pull request's image build: no push, nothing in
//	                     the app project
//	ci-relay             the public gitea-webhook function
//	ci-status            the private build-status function
//	ci-status-push       the identity the cloud-builds push subscription signs as
//	ci-fn-build          builds the two functions' images
//
// The release itself never runs as any of these. cb-image only creates the
// Job; the engine runs as the app project's gitea-release@ on the ops pool,
// the identity a release has always run as (infra/pkg/ops). cb-image's
// Kubernetes rights in the gitea-ops namespace come from there too
// (gitea:opsLaunchers), because that stack owns the cluster's provider.
//
// A pull request's preview runs code nobody has reviewed yet: `pulumi
// preview` executes the branch's program, and the pipeline file comes from
// the branch too. So whatever its identity may do, the branch can do, whatever
// the file claims to be. Previews therefore run as the *-preview accounts,
// which can change nothing in prod, and the preview trigger's launcher cannot
// act as cb-infra or cb-image at all. Read-only is not secret-free:
// cb-infra-preview needs the stack's passphrase, so a branch can read the
// stack's secret config. That is why the relay still previews only
// gitea-cicd:allowedAuthors.
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
	Launcher, Infra, Image                      *serviceaccount.Account
	PreviewLauncher, InfraPreview, ImagePreview *serviceaccount.Account
	Relay, Status, StatusPush, FnBuild          *serviceaccount.Account
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

// InfraPreviewRoles is all cb-infra-preview gets project-wide on the app
// project. `pulumi preview` on the infra stack runs with no --refresh
// (scripts/cicd/infra_deploy.py), so it never live-reads the compute, cluster,
// registry or secret resources -- it compares the program against the stack
// state, which it reads from the state bucket (granted below). The only live
// calls the program makes are two data-source lookups:
//   - dns.LookupManagedZone  -> roles/dns.reader
//   - organizations.LookupProject -> resourcemanager.projects.get, in roles/browser
//
// The Kubernetes provider connects as cb-infra-preview (which has no container
// role), so its cluster calls are refused and it falls back to a client-side
// diff; minting the exec-plugin token needs no cluster permission.
//
// Deliberately NOT roles/viewer: viewer makes the holder a `projectViewer:`
// member, and every bucket carries an IAM binding granting projectViewer read
// of its objects -- which would let a preview read the backups bucket (the
// Gitea database and server secrets) and so, in effect, deploy. These two
// read roles grant no object, secret or Kubernetes-Secret access.
var InfraPreviewRoles = []string{"roles/dns.reader", "roles/browser"}

// The app stack's state bucket and passphrase (scripts/bootstrap.sh).
func appStateBucket(cfg *config.Config) string { return cfg.AppProject + "-pulumi-state" }

const appPassphrase = "pulumi-config-passphrase"

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
		{&a.Launcher, "cb-launcher", "Cloud Build launcher", "Runs each deploy trigger's build: clone, then submit the repo's pipeline"},
		{&a.Infra, "cb-infra", "Cloud Build infra pipeline", "pulumi up on the Gitea app stack (cloudbuild/infra.yaml)"},
		{&a.Image, "cb-image", "Cloud Build image pipeline", "Builds images and launches the release Job (cloudbuild/image.yaml)"},
		{&a.PreviewLauncher, "cb-launcher-preview", "Cloud Build preview launcher", "Runs the preview trigger's build; submits only as the preview accounts"},
		{&a.InfraPreview, "cb-infra-preview", "Cloud Build infra preview", "pulumi preview of a pull request: read-only on the app project"},
		{&a.ImagePreview, "cb-image-preview", "Cloud Build image preview", "Image build of a pull request: no push, no app project access"},
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

	// Two launchers, the same rights, each able to act only as its own pair:
	// the preview trigger's cannot start anything as a deploy identity,
	// whatever its script or the branch's pipeline file says.
	for _, l := range []struct {
		prefix       string
		acct         *serviceaccount.Account
		infra, image *serviceaccount.Account
	}{
		{"launcher", a.Launcher, a.Infra, a.Image},
		{"launcher-preview", a.PreviewLauncher, a.InfraPreview, a.ImagePreview},
	} {
		for _, g := range []struct{ name, role string }{
			{"builds", "roles/cloudbuild.builds.editor"},
			{"logs", "roles/logging.logWriter"},
			// gcloud builds submit follows the submitted build's log from
			// Cloud Logging, because these builds log nowhere else.
			{"log-reader", "roles/logging.viewer"},
		} {
			if err := projectRole(l.prefix+"-"+g.name, cfg.Project, g.role, member(l.acct)); err != nil {
				return nil, err
			}
		}
		// The launcher uploads the source tarball; gcloud also reads the bucket.
		if err := bucketRole(l.prefix+"-source-writer", b.BuildSource, "roles/storage.objectAdmin", member(l.acct)); err != nil {
			return nil, err
		}
		if err := bucketRole(l.prefix+"-source-bucket", b.BuildSource, "roles/storage.legacyBucketReader", member(l.acct)); err != nil {
			return nil, err
		}
		// Submitting a build as an account is acting as it.
		for name, target := range map[string]*serviceaccount.Account{l.prefix + "-as-infra": l.infra, l.prefix + "-as-image": l.image} {
			if _, err := serviceaccount.NewIAMMember(ctx, name, &serviceaccount.IAMMemberArgs{
				ServiceAccountId: target.Name, Role: pulumi.String("roles/iam.serviceAccountUser"), Member: member(l.acct),
			}, opts); err != nil {
				return nil, err
			}
		}
	}

	for _, p := range []struct {
		prefix string
		acct   *serviceaccount.Account
	}{{"infra", a.Infra}, {"image", a.Image}, {"infra-preview", a.InfraPreview}, {"image-preview", a.ImagePreview}} {
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
	// The release gate: read which drills have passed, record a new pass.
	// Create, not admin -- each pass is a new object (release_gate.py), so it
	// can never overwrite or delete one, nor touch the infra lock. Viewer as
	// well, because gcloud storage reads before it writes.
	for name, role := range map[string]string{
		"image-drills-reader": "roles/storage.objectViewer",
		"image-drills-writer": "roles/storage.objectCreator",
	} {
		if err := bucketRole(name, b.DeployState, role, member(a.Image)); err != nil {
			return nil, err
		}
	}
	// A preview says whether the release gate would let it deploy.
	if err := bucketRole("infra-preview-drills-reader", b.DeployState, "roles/storage.objectViewer", member(a.InfraPreview)); err != nil {
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

	// Project-wide read for the two data-source lookups a preview makes (see
	// InfraPreviewRoles). The state and passphrase it also needs are granted on
	// those resources alone, just below -- they are load-bearing now that the
	// project role is no longer roles/viewer. Pulumi's file backend takes no
	// lock for a preview, and a preview never writes.
	for _, role := range InfraPreviewRoles {
		if err := projectRole("app-infra-preview-"+role, cfg.AppProject, role, member(a.InfraPreview)); err != nil {
			return nil, err
		}
	}
	if _, err := storage.NewBucketIAMMember(ctx, "app-infra-preview-state-reader", &storage.BucketIAMMemberArgs{
		Bucket: pulumi.String(appStateBucket(cfg)), Role: pulumi.String("roles/storage.objectViewer"), Member: member(a.InfraPreview),
	}, opts); err != nil {
		return nil, err
	}
	if _, err := secretmanager.NewSecretIamMember(ctx, "app-infra-preview-passphrase", &secretmanager.SecretIamMemberArgs{
		Project: pulumi.String(cfg.AppProject), SecretId: pulumi.String(appPassphrase),
		Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: member(a.InfraPreview),
	}, opts); err != nil {
		return nil, err
	}

	return &a, nil
}
