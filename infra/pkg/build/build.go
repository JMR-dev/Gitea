// Package build wires Cloud Build to the source repository and schedules the
// weekly image rebuild.
//
// The weekly rebuild is not optional garnish: podman's AutoUpdate=registry only
// pulls when the :prod tag's digest changes, so without something pushing a new
// image the container-update layer has nothing to do. The rebuild is what turns
// Debian and Go security fixes into a running container.
package build

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudbuild"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudbuildv2"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudscheduler"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
)

type Triggers struct {
	Image *cloudbuild.Trigger
	Infra *cloudbuild.Trigger
}

// New creates the GitHub connection, the two push triggers, and the scheduled
// rebuild. Returns nil (without error) when GitHub is not configured yet, so a
// fresh stack can be brought up before the GitHub App install is finished.
func New(
	ctx *pulumi.Context,
	cfg *config.Config,
	imageSA *serviceaccount.Account,
	deps []pulumi.Resource,
) (*Triggers, error) {
	if !cfg.GitHubConfigured() {
		ctx.Log.Warn("gitea:githubOwner/githubRepo/githubAppInstallationId not set -- skipping Cloud Build triggers", nil)
		return nil, nil
	}
	opts := pulumi.DependsOn(deps)

	// The PAT is created out of band (it is an OAuth artifact, not
	// infrastructure) and referenced by version.
	patVersion := fmt.Sprintf("projects/%s/secrets/%s/versions/latest", cfg.Project, cfg.GitHubPATSecret)

	// A 2nd-gen connection is read by the Cloud Build SERVICE AGENT, not by the
	// build service account and not by whoever runs Pulumi. Without this grant,
	// creating the connection fails with a permission error on the PAT secret.
	//
	// ServiceIdentity both materialises the agent (it may not exist yet on a
	// fresh project) and hands back its email, which avoids having to look up
	// the project number just to spell out
	// service-{number}@gcp-sa-cloudbuild.iam.gserviceaccount.com.
	agent, err := projects.NewServiceIdentity(ctx, "cloudbuild-agent", &projects.ServiceIdentityArgs{
		Project: pulumi.String(cfg.Project),
		Service: pulumi.String("cloudbuild.googleapis.com"),
	}, opts)
	if err != nil {
		return nil, err
	}

	patAccess, err := secretmanager.NewSecretIamMember(ctx, "cloudbuild-agent-pat", &secretmanager.SecretIamMemberArgs{
		Project:  pulumi.String(cfg.Project),
		SecretId: pulumi.String(cfg.GitHubPATSecret),
		Role:     pulumi.String("roles/secretmanager.secretAccessor"),
		Member:   pulumi.Sprintf("serviceAccount:%s", agent.Email),
	}, opts)
	if err != nil {
		return nil, err
	}

	conn, err := cloudbuildv2.NewConnection(ctx, "github", &cloudbuildv2.ConnectionArgs{
		Name:     pulumi.String("github"),
		Location: pulumi.String(cfg.Region),
		GithubConfig: &cloudbuildv2.ConnectionGithubConfigArgs{
			AppInstallationId: pulumi.Int(cfg.GitHubInstallationID),
			AuthorizerCredential: &cloudbuildv2.ConnectionGithubConfigAuthorizerCredentialArgs{
				OauthTokenSecretVersion: pulumi.String(patVersion),
			},
		},
	}, opts, pulumi.DependsOn([]pulumi.Resource{patAccess}))
	if err != nil {
		return nil, err
	}

	repo, err := cloudbuildv2.NewRepository(ctx, "gitea-repo-link", &cloudbuildv2.RepositoryArgs{
		Name:             pulumi.String(cfg.GitHubRepo),
		Location:         pulumi.String(cfg.Region),
		ParentConnection: conn.ID(),
		RemoteUri:        pulumi.Sprintf("https://github.com/%s/%s.git", cfg.GitHubOwner, cfg.GitHubRepo),
	}, opts)
	if err != nil {
		return nil, err
	}

	imageTrigger, err := cloudbuild.NewTrigger(ctx, "gitea-image", &cloudbuild.TriggerArgs{
		Name:        pulumi.String("gitea-image"),
		Location:    pulumi.String(cfg.Region),
		Description: pulumi.String("Build and roll out the Gitea and Caddy images"),
		RepositoryEventConfig: &cloudbuild.TriggerRepositoryEventConfigArgs{
			Repository: repo.ID(),
			Push:       &cloudbuild.TriggerRepositoryEventConfigPushArgs{Branch: pulumi.String("^main$")},
		},
		Filename: pulumi.String("cloudbuild/image.yaml"),
		// Only rebuild when something that affects the image changed.
		IncludedFiles:  pulumi.ToStringArray([]string{"image/**", "cloudbuild/image.yaml"}),
		ServiceAccount: imageSA.ID(),
		Substitutions: pulumi.StringMap{
			"_REGION": pulumi.String(cfg.Region),
			"_ZONE":   pulumi.String(cfg.Zone),
			"_DOMAIN": pulumi.String(cfg.Domain),
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	// The infra trigger runs Pulumi, so it uses the bootstrap-created account
	// with the broad permissions -- deliberately a different identity from the
	// one that merely pushes images.
	infraSA := cfg.InfraBuildServiceAccount
	if infraSA == "" || strings.Contains(infraSA, "CHANGEME") {
		// Fail here rather than letting Cloud Build reject a malformed service
		// account path at apply time with an opaque error.
		return nil, fmt.Errorf("gitea:infraBuildServiceAccount is not set -- " +
			"scripts/bootstrap.sh prints the value to use")
	}
	infraTrigger, err := cloudbuild.NewTrigger(ctx, "gitea-infra", &cloudbuild.TriggerArgs{
		Name:        pulumi.String("gitea-infra"),
		Location:    pulumi.String(cfg.Region),
		Description: pulumi.String("pulumi up, then push VM config to the instance"),
		RepositoryEventConfig: &cloudbuild.TriggerRepositoryEventConfigArgs{
			Repository: repo.ID(),
			Push:       &cloudbuild.TriggerRepositoryEventConfigPushArgs{Branch: pulumi.String("^main$")},
		},
		Filename:      pulumi.String("cloudbuild/infra.yaml"),
		IncludedFiles: pulumi.ToStringArray([]string{"infra/**", "vm/**", "cloudbuild/infra.yaml"}),
		ServiceAccount: pulumi.Sprintf("projects/%s/serviceAccounts/%s",
			cfg.Project, infraSA),
		Substitutions: pulumi.StringMap{
			"_REGION": pulumi.String(cfg.Region),
			"_ZONE":   pulumi.String(cfg.Zone),
		},
	}, opts)
	if err != nil {
		return nil, err
	}

	if err := scheduleRebuild(ctx, cfg, imageSA, imageTrigger, opts); err != nil {
		return nil, err
	}

	return &Triggers{Image: imageTrigger, Infra: infraTrigger}, nil
}

// scheduleRebuild re-runs the image trigger weekly so the :prod tag picks up
// Debian, Go, and Caddy security fixes without anyone opening a PR.
func scheduleRebuild(
	ctx *pulumi.Context,
	cfg *config.Config,
	imageSA *serviceaccount.Account,
	trigger *cloudbuild.Trigger,
	opts pulumi.ResourceOption,
) error {
	// Running a trigger is a write against the Cloud Build API.
	if _, err := projects.NewIAMMember(ctx, "img-builds-editor", &projects.IAMMemberArgs{
		Project: pulumi.String(cfg.Project),
		Role:    pulumi.String("roles/cloudbuild.builds.editor"),
		Member:  pulumi.Sprintf("serviceAccount:%s", imageSA.Email),
	}, opts); err != nil {
		return err
	}

	// v1 .../locations/{region}/triggers/{id}:run -- the regional (2nd-gen)
	// endpoint. The global .../projects/{p}/triggers/{id}:run path is the
	// 1st-gen one and would not find a regional trigger.
	_, err := cloudscheduler.NewJob(ctx, "gitea-weekly-rebuild", &cloudscheduler.JobArgs{
		Name:        pulumi.String("gitea-weekly-rebuild"),
		Region:      pulumi.String(cfg.Region),
		Description: pulumi.String("Weekly rebuild of the Gitea and Caddy images for base-OS security fixes"),
		// Sunday 04:00 UTC -- an hour before the maintenance reboot window, so a
		// fresh image is waiting when the box restarts.
		Schedule: pulumi.String("0 4 * * 0"),
		TimeZone: pulumi.String("Etc/UTC"),
		HttpTarget: &cloudscheduler.JobHttpTargetArgs{
			HttpMethod: pulumi.String("POST"),
			Uri: pulumi.Sprintf("https://cloudbuild.googleapis.com/v1/projects/%s/locations/%s/triggers/%s:run",
				cfg.Project, cfg.Region, trigger.TriggerId),
			// Empty body, deliberately.
			//
			// RunBuildTriggerRequest.source is a RepoSource -- a 1st-generation
			// shape that names a Cloud Source Repositories repo by project and
			// repo name. There is no way to express a 2nd-gen repository
			// (projects/*/locations/*/connections/*/repositories/*) in it, and
			// these triggers are 2nd-gen: they carry repositoryEventConfig.
			//
			// source is optional, and omitting it tells Cloud Build to use the
			// trigger's own configured repository and branch -- which is exactly
			// what a scheduled rebuild of main wants. This is the REST
			// equivalent of `gcloud builds triggers run TRIGGER --region=...`
			// with no --branch/--tag/--sha, all three of which are optional.
			Body: pulumi.String(base64JSON(`{}`)),
			Headers: pulumi.StringMap{
				"Content-Type": pulumi.String("application/json"),
			},
			OauthToken: &cloudscheduler.JobHttpTargetOauthTokenArgs{
				ServiceAccountEmail: imageSA.Email,
				Scope:               pulumi.String("https://www.googleapis.com/auth/cloud-platform"),
			},
		},
	}, opts)
	return err
}

// base64JSON encodes a Cloud Scheduler HTTP body. The API takes the body as a
// base64 string, and passing raw JSON fails at apply time with an unhelpful
// error rather than at plan time.
func base64JSON(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
