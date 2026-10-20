// Command gitea-cicd is CI/CD for Gitea itself -- this repo's images, VM and
// infra/ stack -- in a GCP project of its own: a webhook relay from Gitea,
// Pub/Sub, and Cloud Build triggers that deploy into the app project with
// backups, status checks and automatic rollback. CI for the repos Gitea hosts
// stays on Gitea Actions.
//
// Applied locally only (make ci-preview / ci-up): it decides who may deploy,
// so nothing it deploys may change it. Like infra/, the program is thin, each
// package owns one slice, and the wiring order is the dependency order.
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/alerting"
	"gitea-cicd/pkg/config"
	"gitea-cicd/pkg/identity"
	"gitea-cicd/pkg/project"
	"gitea-cicd/pkg/relay"
	"gitea-cicd/pkg/schedule"
	"gitea-cicd/pkg/storage"
	"gitea-cicd/pkg/triggers"
)

// The relay functions' source, its own Go module.
const relayDir = "relay"

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg, err := config.Load(ctx)
		if err != nil {
			return err
		}
		apis, err := project.EnableAPIs(ctx, cfg)
		if err != nil {
			return err
		}
		buckets, err := storage.New(ctx, cfg, apis)
		if err != nil {
			return err
		}
		acc, err := identity.New(ctx, cfg, buckets, apis)
		if err != nil {
			return err
		}
		rl, err := relay.New(ctx, cfg, acc, buckets, relayDir, apis)
		if err != nil {
			return err
		}
		if err := triggers.New(ctx, cfg, acc, buckets, rl.Topics, apis); err != nil {
			return err
		}
		if err := schedule.New(ctx, cfg, rl.Topics[relay.TopicRebuild], apis); err != nil {
			return err
		}
		if err := alerting.New(ctx, cfg, apis); err != nil {
			return err
		}

		ctx.Export("project", pulumi.String(cfg.Project))
		ctx.Export("autoDeploy", pulumi.Bool(cfg.AutoDeploy))
		// What scripts/cicd/bootstrap.py hook points the Gitea webhook at.
		ctx.Export("webhookUrl", rl.WebhookURL)
		ctx.Export("deployStateBucket", buckets.DeployState.Name)
		ctx.Export("buildSourceBucket", buckets.BuildSource.Name)
		// infra/ needs this in gitea:opsLaunchers before CI can launch a release.
		ctx.Export("imageServiceAccount", acc.Image.Email)
		ctx.Export("infraServiceAccount", acc.Infra.Email)
		return nil
	})
}
