// Package triggers holds the four Cloud Build triggers, one per Pub/Sub topic,
// and the launcher build each runs.
//
// Cloud Build has no Gitea connector, so a trigger cannot fetch the repo's
// own cloudbuild/*.yaml. Each trigger instead carries an inline launcher: it
// clones the commit from Gitea (anonymously; the repo is public) and submits
// the repo's pipeline files as builds of their own, as cb-infra or cb-image.
// The pipelines stay in the repo, so a pull request previews its own pipeline
// changes, and the launcher -- the part that picks the identity, the mode and
// the approval -- stays here, applied by hand.
//
// The trigger decides the mode, never the message: what arrives on ci-deploy
// runs as a deploy whatever its attributes say, and only ci-gated's trigger
// waits for approval. Only the relay may publish to the first three topics
// and only Cloud Scheduler to the fourth.
package triggers

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudbuild"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
	"gitea-cicd/pkg/identity"
	"gitea-cicd/pkg/relay"
	cistorage "gitea-cicd/pkg/storage"
)

// Images the launcher runs, pinned. The engine's Job pins the same gcloud.
const (
	gitImage    = "gcr.io/cloud-builders/git"
	gcloudImage = "gcr.io/google.com/cloudsdktool/google-cloud-cli:589.0.0-slim"
)

// Mode is one trigger's behaviour.
type Mode struct {
	Name     string // cicd/relay's Mode* (and "rebuild"): the _MODE substitution and the status context
	Topic    string
	Approval bool // the build waits for `make approve`
	// Gated by autoDeploy: off, the trigger is disabled. A preview changes
	// nothing in prod, so it is never gated.
	Automatic bool
}

var Modes = []Mode{
	{Name: "preview", Topic: relay.TopicPreview},
	{Name: "deploy", Topic: relay.TopicDeploy, Automatic: true},
	{Name: "gated", Topic: relay.TopicGated, Approval: true, Automatic: true},
	{Name: "rebuild", Topic: relay.TopicRebuild, Automatic: true},
}

func New(ctx *pulumi.Context, cfg *config.Config, acc *identity.Accounts, b *cistorage.Buckets,
	topics map[string]*pubsub.Topic, deps []pulumi.Resource) error {
	for _, m := range Modes {
		args := &cloudbuild.TriggerArgs{
			Project:     pulumi.String(cfg.Project),
			Location:    pulumi.String(cfg.Region),
			Name:        pulumi.String("ci-" + m.Name),
			Description: pulumi.String(fmt.Sprintf("Gitea %s: clone the commit, submit cloudbuild/<pipeline>.yaml", m.Name)),
			Disabled:    pulumi.Bool(m.Automatic && !cfg.AutoDeploy),
			// The launcher's identity; the pipelines it submits run as
			// cb-infra or cb-image (see Script).
			ServiceAccount: acc.Launcher.Name,
			PubsubConfig: &cloudbuild.TriggerPubsubConfigArgs{
				Topic: topics[m.Topic].ID(),
			},
			// Bound from the message's attributes (cicd/relay); _MODE is the
			// trigger's own. "main" means: whatever main is now.
			Substitutions: pulumi.StringMap{
				"_SHA":       pulumi.String("$(body.message.attributes.sha)"),
				"_PIPELINES": pulumi.String("$(body.message.attributes.pipelines)"),
				"_MODE":      pulumi.String(m.Name),
			},
			Build: &cloudbuild.TriggerBuildArgs{
				Tags:    pulumi.ToStringArray([]string{"gitea-launch", "mode-" + m.Name}),
				Timeout: pulumi.String("28800s"), // above the infra and image builds' own bounds, back to back
				Options: &cloudbuild.TriggerBuildOptionsArgs{
					Logging: pulumi.String("CLOUD_LOGGING_ONLY"),
				},
				Steps: cloudbuild.TriggerBuildStepArray{
					&cloudbuild.TriggerBuildStepArgs{
						Id: pulumi.String("clone"), Name: pulumi.String(gitImage),
						Entrypoint: pulumi.String("bash"),
						Args:       pulumi.ToStringArray([]string{"-c", CloneScript(cfg)}),
					},
					&cloudbuild.TriggerBuildStepArgs{
						Id: pulumi.String("submit"), Name: pulumi.String(gcloudImage),
						Entrypoint: pulumi.String("bash"),
						Args:       pulumi.ToStringArray([]string{"-c", SubmitScript(cfg)}),
					},
				},
			},
		}
		if m.Approval {
			args.ApprovalConfig = &cloudbuild.TriggerApprovalConfigArgs{ApprovalRequired: pulumi.Bool(true)}
		}
		if _, err := cloudbuild.NewTrigger(ctx, "ci-"+m.Name, args, pulumi.DependsOn(deps)); err != nil {
			return err
		}
	}
	return nil
}

// CloneScript checks out the commit into /workspace/src. For anything that
// deploys, the commit must be on main: the relay only publishes main's, but
// the trigger checks rather than trusts. The script is a Cloud Build step
// argument, so $$ is a literal $ and ${_X} a substitution.
func CloneScript(cfg *config.Config) string {
	return strings.ReplaceAll(`set -euo pipefail
sha="${_SHA}"
[ -n "$$sha" ] || { echo "no _SHA in the message" >&2; exit 1; }
git clone --quiet --filter=blob:none --no-checkout "@REPO@" /workspace/src
cd /workspace/src
if [ "$$sha" = main ]; then sha=$$(git rev-parse origin/main); fi
git -c advice.detachedHead=false checkout --quiet "$$sha"
sha=$$(git rev-parse HEAD)
case "${_MODE}" in
  deploy|gated|rebuild)
    git merge-base --is-ancestor "$$sha" origin/main \
      || { echo "$$sha is not on main; refusing to deploy it" >&2; exit 1; } ;;
esac
echo -n "$$sha" > /workspace/sha
echo "mode=${_MODE} pipelines=${_PIPELINES} commit=$$sha"
`, "@REPO@", cfg.GiteaURL+"/"+cfg.GiteaRepo+".git")
}

// SubmitScript runs each pipeline the message names, infra before image, as a
// build of its own, and stops at the first that fails: an image is never
// released onto infrastructure that just failed to deploy.
func SubmitScript(cfg *config.Config) string {
	r := strings.NewReplacer(
		"@CI@", cfg.Project, "@REGION@", cfg.Region, "@APP@", cfg.AppProject, "@ZONE@", cfg.AppZone,
		"@STATE@", cfg.Project+"-deploy-state", "@SOURCE@", cfg.Project+"-build-source",
		"@GITEA@", cfg.GiteaURL, "@REPO@", cfg.GiteaRepo,
	)
	return r.Replace(`set -euo pipefail
sha=$$(cat /workspace/sha)
short=$${sha:0:7}
ran=0
for p in infra image; do
  case ",${_PIPELINES}," in *",$$p,"*) ;; *) continue ;; esac
  # Only substitutions each file declares: Cloud Build refuses the others.
  subs="_SHA=$$sha,_APP_PROJECT=@APP@,_REGION=@REGION@,_ZONE=@ZONE@,_DEPLOY_STATE_BUCKET=@STATE@"
  case "$$p:${_MODE}" in
    infra:preview) subs="$$subs,_MODE=preview" ;;
    infra:gated)   subs="$$subs,_MODE=up,_APPROVED=true,_GITEA_URL=@GITEA@,_GITEA_REPO=@REPO@" ;;
    infra:*)       subs="$$subs,_MODE=up,_GITEA_URL=@GITEA@,_GITEA_REPO=@REPO@" ;;
    image:preview) subs="$$subs,SHORT_SHA=$$short,_PUSH=false,_RELEASE=false" ;;
    image:gated)   subs="$$subs,SHORT_SHA=$$short,_PUSH=true,_RELEASE=true,_APPROVED=true" ;;
    image:*)       subs="$$subs,SHORT_SHA=$$short,_PUSH=true,_RELEASE=true" ;;
  esac
  echo "submitting cloudbuild/$$p.yaml as cb-$$p@ ($$subs)"
  gcloud builds submit /workspace/src --project=@CI@ --region=@REGION@ \
    --config=/workspace/src/cloudbuild/$$p.yaml \
    --service-account=projects/@CI@/serviceAccounts/cb-$$p@@CI@.iam.gserviceaccount.com \
    --gcs-source-staging-dir=gs://@SOURCE@/source \
    --substitutions="$$subs" \
    --tags="gitea-pipeline,pipeline-$$p,sha-$$short"
  ran=$$((ran + 1))
done
[ "$$ran" -gt 0 ] || { echo "no known pipeline in _PIPELINES=${_PIPELINES}" >&2; exit 1; }
`)
}
