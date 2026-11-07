// Package alerting e-mails the operator when the pipeline rolls prod back,
// when a rollback itself fails, when a pipeline build fails, and when Gitea's
// TLS certificate stops renewing or Gitea stops answering from outside.
//
// The first three are log-match alerts: one e-mail per matching line
// (rate-limited), with no metric or threshold in between to get wrong. The
// lines come from three places, all in this project's logs:
//   - GITEA_RELEASE_ROLLBACK[_FAILED], from the release engine. The engine runs
//     in the app project, but its launcher follows the Job's log into the
//     image build's own output, which is where these match it.
//   - GITEA_INFRA_ROLLBACK[_FAILED], from scripts/cicd/infra_deploy.py.
//   - GITEA_PIPELINE_FAILED, from the build-status function, for every
//     pipeline build that ends in failure.
//
// The certificate alert is a threshold on an uptime check's metric. Caddy
// renews the certificate through ACME DNS-01, and a deploy's checks see only
// deploy day: a renewal that broke later would retry quietly until the
// certificate expired and the site went down. So the check (gitea-https)
// fetches /api/healthz over HTTPS from Google's checkers every 15 min,
// validating the certificate, and records the days it has left; the policy
// (cert-not-renewing) fires when they drop below gitea-cicd:certAlertDays,
// or when there has been no reading for an hour.
package alerting

import (
	"fmt"
	"net/url"
	"strings"

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

// New creates the operator's e-mail channel and every alert, and returns the
// certificate check's ID (main.go exports it as certCheckId).
func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) (pulumi.StringOutput, error) {
	opts := pulumi.DependsOn(deps)
	ch, err := monitoring.NewNotificationChannel(ctx, "operator-email", &monitoring.NotificationChannelArgs{
		Project: pulumi.String(cfg.Project), DisplayName: pulumi.String("Gitea operator"), Type: pulumi.String("email"),
		Labels: pulumi.StringMap{"email_address": pulumi.ToSecret(pulumi.String(cfg.OperatorEmail)).(pulumi.StringOutput)},
	}, opts)
	if err != nil {
		return pulumi.StringOutput{}, err
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
			return pulumi.StringOutput{}, err
		}
	}
	return certAlert(ctx, cfg, ch, opts)
}

const (
	certTitle = "Gitea: TLS certificate not renewing, or unreachable from outside"
	// Days until the certificate a check was served expires, per check_id.
	certMetric = "monitoring.googleapis.com/uptime_check/time_until_ssl_cert_expires"
)

// certAlert creates the uptime check on Gitea's public URL and the policy on
// its certificate metric, and returns the check's ID: the check_id label on
// its metrics.
func certAlert(ctx *pulumi.Context, cfg *config.Config, ch *monitoring.NotificationChannel,
	opts pulumi.ResourceOption) (pulumi.StringOutput, error) {
	u, err := url.Parse(cfg.GiteaURL)
	if err != nil || u.Hostname() == "" {
		return pulumi.StringOutput{}, fmt.Errorf("gitea-cicd:giteaUrl %q has no host for the uptime check", cfg.GiteaURL)
	}
	host := u.Hostname()

	check, err := monitoring.NewUptimeCheckConfig(ctx, "gitea-https", &monitoring.UptimeCheckConfigArgs{
		Project:     pulumi.String(cfg.Project),
		DisplayName: pulumi.String("Gitea over HTTPS, from outside"),
		MonitoredResource: &monitoring.UptimeCheckConfigMonitoredResourceArgs{
			Type:   pulumi.String("uptime_url"),
			Labels: pulumi.StringMap{"host": pulumi.String(host), "project_id": pulumi.String(cfg.Project)},
		},
		// validateSsl: a certificate that doesn't validate fails the check.
		HttpCheck: &monitoring.UptimeCheckConfigHttpCheckArgs{
			UseSsl: pulumi.Bool(true), ValidateSsl: pulumi.Bool(true), Port: pulumi.Int(443),
			Path: pulumi.String("/api/healthz"), RequestMethod: pulumi.String("GET"),
		},
		Period:  pulumi.String("900s"),
		Timeout: pulumi.String("10s"),
		// From Google's published checker addresses, so a firewall or a ban
		// list can tell them apart; SelectedRegions unset is every region.
		CheckerType: pulumi.String("STATIC_IP_CHECKERS"),
	}, opts)
	if err != nil {
		return pulumi.StringOutput{}, err
	}

	_, err = monitoring.NewAlertPolicy(ctx, "cert-not-renewing", &monitoring.AlertPolicyArgs{
		Project: pulumi.String(cfg.Project), DisplayName: pulumi.String(certTitle),
		Severity: pulumi.String("CRITICAL"), Combiner: pulumi.String("OR"),
		Conditions: monitoring.AlertPolicyConditionArray{&monitoring.AlertPolicyConditionArgs{
			DisplayName: pulumi.String(fmt.Sprintf("Certificate under %d days left, or no reading for an hour", cfg.CertAlertDays)),
			ConditionThreshold: &monitoring.AlertPolicyConditionConditionThresholdArgs{
				Filter: pulumi.Sprintf(`metric.type="%s" AND resource.type="uptime_url" AND metric.labels.check_id="%s"`,
					certMetric, check.UptimeCheckId),
				// The lowest reading of any checker in each half hour.
				Aggregations: monitoring.AlertPolicyConditionConditionThresholdAggregationArray{
					&monitoring.AlertPolicyConditionConditionThresholdAggregationArgs{
						AlignmentPeriod:    pulumi.String("1800s"),
						PerSeriesAligner:   pulumi.String("ALIGN_NEXT_OLDER"),
						CrossSeriesReducer: pulumi.String("REDUCE_MIN"),
						GroupByFields:      pulumi.StringArray{pulumi.String("metric.label.check_id")},
					},
				},
				Comparison:     pulumi.String("COMPARISON_LT"),
				ThresholdValue: pulumi.Float64(float64(cfg.CertAlertDays)),
				Duration:       pulumi.String("3600s"),
				Trigger:        &monitoring.AlertPolicyConditionConditionThresholdTriggerArgs{Count: pulumi.Int(1)},
				// A dead VM, a refused TLS handshake or a deleted check stop
				// the series; an hour of silence then fires too.
				EvaluationMissingData: pulumi.String("EVALUATION_MISSING_DATA_ACTIVE"),
			},
		}},
		// Again every day while it is open; a week with nothing below the
		// threshold closes it. No notificationRateLimit: that is for
		// log-match alerts only.
		AlertStrategy: &monitoring.AlertPolicyAlertStrategyArgs{
			AutoClose: pulumi.String("604800s"),
			NotificationChannelStrategies: monitoring.AlertPolicyAlertStrategyNotificationChannelStrategyArray{
				&monitoring.AlertPolicyAlertStrategyNotificationChannelStrategyArgs{
					NotificationChannelNames: pulumi.StringArray{ch.Name},
					RenotifyInterval:         pulumi.String("86400s"),
				},
			},
		},
		Documentation: &monitoring.AlertPolicyDocumentationArgs{
			Content: pulumi.String(certDoc(host, cfg.Project, cfg.CertAlertDays)), MimeType: pulumi.String("text/markdown"),
		},
		NotificationChannels: pulumi.StringArray{ch.Name},
	}, opts)
	if err != nil {
		return pulumi.StringOutput{}, err
	}
	return check.UptimeCheckId, nil
}

// certDoc is the policy's documentation, which the e-mail carries, in
// markdown. Unreachable first: that one needs action within minutes.
func certDoc(host, project string, days int) string {
	return fmt.Sprintf(strings.Join([]string{
		"The uptime check gitea-https fetches https://%[1]s/api/healthz every 15 min from Google's checkers, " +
			"outside our network, validating the certificate. This fires on either of two things; " +
			"`curl -sv https://%[1]s/api/healthz` from outside Google tells them apart: if it fails, it is the first.",
		"",
		"**1. Unreachable from outside: act within minutes.** No checker has had a reading for an hour: " +
			"the VM, Caddy or Gitea is down, the way in from the internet is, or the check was deleted.",
		"- `make status` (Caddy, Gitea, nftables, fail2ban) and `make logs`, from a checkout.",
		"- The check: `gcloud monitoring uptime list-configs --project %[2]s`.",
		"",
		"**2. Not renewing: under %[3]d days left.** Caddy renews a certificate when a third of its lifetime is " +
			"left (about 21 days of a 64-day one) and retries from then on, so it has been failing for days. " +
			"The site goes down when the certificate expires. First checks, on the VM (`make ssh`):",
		"- The DNS-01 token minter: `systemctl status gitea-caddy-dns-token.timer gitea-caddy-dns-token.service` " +
			"and its journal, `sudo journalctl -u gitea-caddy-dns-token --since -2d`.",
		"- Caddy's own errors: `sudo journalctl CONTAINER_NAME=caddy | grep -E '\"logger\":\"tls\\.(obtain|renew)\"'`.",
		"- `sudo gitea-caddy-dns-token --check`: 0 it works, 1 a definite failure (it says what), 2 a transient one.",
		"- From a checkout: `make acme-staging-check`, a certificate end to end against Let's Encrypt's staging CA.",
		"",
		"A drill (gitea-cicd:certAlertDays raised above the days left) sends this on purpose. " +
			"More in docs/runbook.md, \"When the certificate alert fires\".",
	}, "\n"), host, project, days)
}
