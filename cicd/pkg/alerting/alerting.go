// Package alerting e-mails the operator when the pipeline rolls prod back,
// when a rollback itself fails, and when a pipeline build fails. Each is a
// log-match alert: one e-mail per matching line (rate-limited), with no
// metric or threshold in between to get wrong.
//
// The lines come from three places, all in this project's logs:
//   - GITEA_RELEASE_ROLLBACK[_FAILED], from the release engine. The engine runs
//     in the app project, but its launcher follows the Job's log into the
//     image build's own output, which is where these match it.
//   - GITEA_INFRA_ROLLBACK[_FAILED], from scripts/cicd/infra_deploy.py.
//   - GITEA_PIPELINE_FAILED, from the build-status function, for every
//     pipeline build that ends in failure.
package alerting

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/monitoring"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

type alert struct {
	name, title, severity, filter, doc string
}

var alerts = []alert{
	{
		name: "rollback-failed", title: "Gitea: an automatic rollback FAILED", severity: "CRITICAL",
		filter: `resource.type="build" AND (textPayload:"GITEA_RELEASE_ROLLBACK_FAILED" OR textPayload:"GITEA_INFRA_ROLLBACK_FAILED")`,
		doc: "A release or infra deploy failed and so did the automatic rollback. Prod may be down or half-changed. " +
			"Follow docs/runbook.md (manual rollback from the release manifest).",
	},
	{
		name: "rolled-back", title: "Gitea: prod was rolled back automatically", severity: "WARNING",
		filter: `resource.type="build" AND (textPayload:"GITEA_RELEASE_ROLLBACK path=" OR textPayload:"GITEA_INFRA_ROLLBACK sha=")`,
		doc: "A release or infra deploy failed its checks and was rolled back; prod is on the previous version. " +
			"The build log says why. A drill (make ci-drill) also sends this, on purpose.",
	},
	{
		name: "pipeline-failed", title: "Gitea: a pipeline build failed", severity: "WARNING",
		filter: `resource.type="cloud_run_revision" AND resource.labels.service_name="build-status" AND jsonPayload.msg="GITEA_PIPELINE_FAILED"`,
		doc:    "A Gitea pipeline build failed. The commit's status on Gitea links to its log.",
	},
}

func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) error {
	opts := pulumi.DependsOn(deps)
	ch, err := monitoring.NewNotificationChannel(ctx, "operator-email", &monitoring.NotificationChannelArgs{
		Project: pulumi.String(cfg.Project), DisplayName: pulumi.String("Gitea operator"), Type: pulumi.String("email"),
		Labels: pulumi.StringMap{"email_address": pulumi.ToSecret(pulumi.String(cfg.OperatorEmail)).(pulumi.StringOutput)},
	}, opts)
	if err != nil {
		return err
	}
	for _, a := range alerts {
		if _, err := monitoring.NewAlertPolicy(ctx, a.name, &monitoring.AlertPolicyArgs{
			Project: pulumi.String(cfg.Project), DisplayName: pulumi.String(a.title),
			Severity: pulumi.String(a.severity), Combiner: pulumi.String("OR"),
			Conditions: monitoring.AlertPolicyConditionArray{&monitoring.AlertPolicyConditionArgs{
				DisplayName: pulumi.String(a.title),
				ConditionMatchedLog: &monitoring.AlertPolicyConditionConditionMatchedLogArgs{
					Filter: pulumi.String(a.filter),
				},
			}},
			AlertStrategy: &monitoring.AlertPolicyAlertStrategyArgs{
				NotificationRateLimit: &monitoring.AlertPolicyAlertStrategyNotificationRateLimitArgs{
					Period: pulumi.String("300s"),
				},
				AutoClose: pulumi.String("1800s"),
			},
			Documentation: &monitoring.AlertPolicyDocumentationArgs{
				Content: pulumi.String(a.doc), MimeType: pulumi.String("text/markdown"),
			},
			NotificationChannels: pulumi.StringArray{ch.Name},
		}, opts); err != nil {
			return err
		}
	}
	return nil
}
