// Package telemetry hands the CI project's two functions' logs and metrics to
// Watchtower, our observability stack, which runs outside GCP. Cloud Run has no
// host to put an agent on, so nothing here pushes: a Cloud Logging sink feeds a
// Pub/Sub topic, and Watchtower pulls that subscription, and Cloud Monitoring,
// as one read-only service account in its own project.
//
// All of it is behind gitea-cicd:watchtowerReader, and creates nothing when
// unset. The same package in infra/ does this for the runner cluster.
package telemetry

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/logging"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

const (
	TopicName        = "watchtower-logs"
	SubscriptionName = "watchtower-logs-pull"
	SinkName         = "watchtower-ci"
	metricsRoleID    = "watchtowerMetricsReader"
)

// metricsPermissions are all the reader gets from Cloud Monitoring. Not
// roles/monitoring.viewer: that also reads notification channels, and so the
// owner's email. This project's operator-email channel holds exactly that
// (package alerting). These two are the only Cloud Monitoring calls Alloy's
// prometheus.exporter.gcp makes (stackdriver_exporter v0.18.0 in Alloy
// v1.21.3); check its collector before a new Alloy needs more.
var metricsPermissions = []string{
	"monitoring.timeSeries.list",
	"monitoring.metricDescriptors.list",
}

// LogFilter is what the sink carries: an allow-list, so a log nobody thought
// about stays out. The functions' names are the ones package relay deploys.
// Left out, and why:
//   - cloudaudit: it carries the owner's email.
//   - The builds' own logs (resource.type build): they hold whatever a deploy
//     printed.
//   - run.googleapis.com/varlog/system: Cloud Run's own, not the functions'.
//     Their slog writes to stderr (cicd/relay/function.go), and nothing else of
//     theirs is on stdout.
//
// In this query language OR binds tighter than AND, so every mix of the two is
// parenthesised; the test refuses one that is not.
func LogFilter() string {
	return `resource.type="cloud_run_revision"` +
		` AND resource.labels.service_name=("gitea-webhook" OR "build-status")` +
		` AND (log_id("run.googleapis.com/stderr") OR log_id("run.googleapis.com/requests"))`
}

// New does nothing unless gitea-cicd:watchtowerReader is set.
func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) error {
	if cfg.WatchtowerReader == "" {
		return nil
	}
	opts := pulumi.DependsOn(deps)
	project := pulumi.String(cfg.Project)
	reader := pulumi.String("serviceAccount:" + cfg.WatchtowerReader)

	topic, err := pubsub.NewTopic(ctx, TopicName, &pubsub.TopicArgs{
		Project: project, Name: pulumi.String(TopicName),
	}, opts)
	if err != nil {
		return err
	}
	sub, err := pubsub.NewSubscription(ctx, SubscriptionName, &pubsub.SubscriptionArgs{
		Project: project, Name: pulumi.String(SubscriptionName), Topic: topic.ID(),
		AckDeadlineSeconds: pulumi.Int(60),
		// Buffers for 7 days while Watchtower isn't pulling. Not lossless once it
		// is: Alloy acknowledges a message when it hands it to its queue, so a
		// restart during an outage loses what's queued, about 10 MiB per node, and
		// Loki can refuse entries more than about an hour behind their stream.
		MessageRetentionDuration: pulumi.String("604800s"),
		// Never expire: a quiet month must not delete the subscription.
		ExpirationPolicy: &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
	}, opts)
	if err != nil {
		return err
	}
	// Unique rather than the shared cloud-logs@ account, so the publish grant
	// below answers for this sink alone.
	sink, err := logging.NewProjectSink(ctx, SinkName, &logging.ProjectSinkArgs{
		Project: project, Name: pulumi.String(SinkName),
		Destination:          pulumi.Sprintf("pubsub.googleapis.com/%s", topic.ID()),
		Filter:               pulumi.String(LogFilter()),
		UniqueWriterIdentity: pulumi.Bool(true),
	}, opts)
	if err != nil {
		return err
	}
	// WriterIdentity already starts with serviceAccount:.
	if _, err := pubsub.NewTopicIAMMember(ctx, SinkName+"-publisher", &pubsub.TopicIAMMemberArgs{
		Project: project, Topic: topic.Name,
		Role: pulumi.String("roles/pubsub.publisher"), Member: sink.WriterIdentity,
	}, opts); err != nil {
		return err
	}

	if _, err := pubsub.NewSubscriptionIAMMember(ctx, SubscriptionName+"-reader", &pubsub.SubscriptionIAMMemberArgs{
		Project: project, Subscription: sub.Name,
		Role: pulumi.String("roles/pubsub.subscriber"), Member: reader,
	}, opts); err != nil {
		return err
	}
	// After the topic and the sink, as in infra/pkg/telemetry: if either fails, the
	// role isn't created only to be deleted. That only soft-deletes a custom role,
	// and from 7 to 37 days after, its ID can't be used again.
	role, err := projects.NewIAMCustomRole(ctx, "watchtower-metrics-reader", &projects.IAMCustomRoleArgs{
		Project:     project,
		RoleId:      pulumi.String(metricsRoleID),
		Title:       pulumi.String("Watchtower metrics reader"),
		Description: pulumi.String("List Cloud Monitoring series and descriptors; no channels, no alert policies"),
		Permissions: pulumi.ToStringArray(metricsPermissions),
	}, opts, pulumi.DependsOn([]pulumi.Resource{topic, sink}))
	if err != nil {
		return err
	}
	_, err = projects.NewIAMMember(ctx, "watchtower-metrics-reader", &projects.IAMMemberArgs{
		Project: project, Role: role.Name, Member: reader,
	}, opts)
	return err
}
