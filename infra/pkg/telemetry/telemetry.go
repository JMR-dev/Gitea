// Package telemetry hands the runner cluster's logs and metrics to Watchtower,
// our observability stack, which runs outside GCP. Nothing here pushes: a Cloud
// Logging sink feeds a Pub/Sub topic, and Watchtower pulls that subscription,
// and Cloud Monitoring, as one read-only service account in its own project. A
// host agent has no place here: the base node has no headroom, and a VPN key in
// this cluster would be in reach of DinD jobs (docs/runners.md).
//
// All of it is behind gitea:watchtowerReader, and creates nothing when unset.
// The same package in cicd/ does this for the CI project's two functions.
package telemetry

import (
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/logging"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/gke"
	"gitea-infra/pkg/network"
	"gitea-infra/pkg/runner"
)

const (
	TopicName        = "watchtower-logs"
	SubscriptionName = "watchtower-logs-pull"
	SinkName         = "watchtower-ci"
	metricsRoleID    = "watchtowerMetricsReader"
)

// metricsPermissions are all the reader gets from Cloud Monitoring. Not
// roles/monitoring.viewer: that also reads notification channels, and so the
// owner's email.
var metricsPermissions = []string{
	"monitoring.timeSeries.list",
	"monitoring.metricDescriptors.list",
	"monitoring.monitoredResourceDescriptors.list",
}

// LogFilter is what the sink carries: an allow-list, so a log nobody thought
// about stays out. Left out, and why:
//   - cloudaudit: it carries the owner's email. It shares resource.type
//     k8s_cluster with the autoscaler's log, which is why that clause names its
//     log rather than its type.
//   - Job output: the gitea-jobs and gitea-jobs-arm64 containers. Only their pod
//     events come through, which say a pod started or failed and nothing it
//     printed.
//   - kube-system and gmp-system: volume, GKE's own.
//   - gitea-ops: the release engine's Jobs.
//   - The serial console, and the node logs GKE files under k8s_container with
//     no namespace (GCEGuestAgent and the like).
//
// In this query language OR binds tighter than AND, so every mix of the two is
// parenthesised; the test refuses one that is not.
func LogFilter() string {
	cluster := `resource.labels.cluster_name="` + gke.Name + `"`
	return strings.Join([]string{
		`(resource.type="k8s_container" AND ` + cluster +
			` AND resource.labels.namespace_name="` + runner.Namespace + `"` +
			` AND resource.labels.container_name="` + runner.ContainerName + `"` +
			` AND (log_id("stdout") OR log_id("stderr")))`,
		`(resource.type="k8s_pod" AND ` + cluster +
			` AND resource.labels.namespace_name=("` + runner.JobsNamespace + `" OR "` + runner.ArmJobsNamespace + `")` +
			` AND log_id("events"))`,
		`(resource.type="k8s_cluster" AND ` + cluster +
			` AND log_id("container.googleapis.com/cluster-autoscaler-visibility"))`,
		`(resource.type="nat_gateway" AND resource.labels.gateway_name="` + network.RunnerNATName + `"` +
			` AND log_id("compute.googleapis.com/nat_flows"))`,
	}, " OR ")
}

// New does nothing unless gitea:watchtowerReader is set. The account applying
// it needs roles/logging.configWriter and roles/pubsub.admin here: the CI
// project's cb-infra gets them from cicd/pkg/identity, so `make ci-up` has to
// come before the deploy that first runs this.
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
	// After the topic and the sink. A deployer without the telemetry roles fails
	// on the topic, but iam.roleAdmin would still let it create this, and the
	// rollback would then delete it: that only soft-deletes a custom role, and
	// from 7 to 37 days after, its ID can't be used again.
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
