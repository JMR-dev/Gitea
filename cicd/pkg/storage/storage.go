// Package storage holds the CI project's buckets. The Pulumi state bucket is
// not among them: bootstrap.py makes it, because this program cannot run
// without it.
package storage

import (
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

type Buckets struct {
	// BuildSource stages the source tarballs the launcher submits.
	BuildSource *storage.Bucket
	// RelaySource holds the relay functions' source archive.
	RelaySource *storage.Bucket
	// DeployState holds the infra deploy lock and the last-good markers the
	// infra pipeline rolls back to. Versioned, so a bad marker is recoverable.
	DeployState *storage.Bucket
}

func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) (*Buckets, error) {
	opts := pulumi.DependsOn(deps)
	bucket := func(name string, versioned bool, deleteAfterDays int) (*storage.Bucket, error) {
		args := &storage.BucketArgs{
			Name:                     pulumi.Sprintf("%s-%s", cfg.Project, name),
			Location:                 pulumi.String(strings.ToUpper(cfg.Region)),
			UniformBucketLevelAccess: pulumi.Bool(true),
			PublicAccessPrevention:   pulumi.String("enforced"),
			Versioning:               &storage.BucketVersioningArgs{Enabled: pulumi.Bool(versioned)},
		}
		if deleteAfterDays > 0 {
			args.LifecycleRules = storage.BucketLifecycleRuleArray{&storage.BucketLifecycleRuleArgs{
				Action:    &storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(deleteAfterDays)},
			}}
		}
		return storage.NewBucket(ctx, name, args, opts)
	}

	src, err := bucket("build-source", false, 7)
	if err != nil {
		return nil, err
	}
	relay, err := bucket("relay-source", false, 0)
	if err != nil {
		return nil, err
	}
	state, err := bucket("deploy-state", true, 0)
	if err != nil {
		return nil, err
	}
	return &Buckets{BuildSource: src, RelaySource: relay, DeployState: state}, nil
}
