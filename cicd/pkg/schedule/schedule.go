// Package schedule is the weekly rebuild: Sunday 04:00 UTC, an hour before
// the VM's reboot window, the image is rebuilt from main (picking up Alpine
// patches and the newest Gitea of the pinned major) and released.
//
// It publishes straight to ci-rebuild as the Cloud Scheduler service agent;
// the rebuild trigger resolves "main" to main's current commit when it runs.
// Paused while autoDeploy is off.
package schedule

import (
	"encoding/base64"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudscheduler"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

func New(ctx *pulumi.Context, cfg *config.Config, topic *pubsub.Topic, deps []pulumi.Resource) error {
	_, err := cloudscheduler.NewJob(ctx, "weekly-rebuild", &cloudscheduler.JobArgs{
		Project: pulumi.String(cfg.Project), Region: pulumi.String(cfg.Region), Name: pulumi.String("weekly-rebuild"),
		Description: pulumi.String("Rebuild and release the Gitea image from main"),
		Schedule:    pulumi.String("0 4 * * 0"),
		TimeZone:    pulumi.String("Etc/UTC"),
		Paused:      pulumi.Bool(!cfg.AutoDeploy),
		PubsubTarget: &cloudscheduler.JobPubsubTargetArgs{
			TopicName:  topic.ID(),
			Data:       pulumi.String(base64.StdEncoding.EncodeToString([]byte("rebuild"))),
			Attributes: pulumi.StringMap{"sha": pulumi.String("main"), "pipelines": pulumi.String("image")},
		},
	}, pulumi.DependsOn(deps))
	return err
}
