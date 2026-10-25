// Package storage holds the buckets, and hashes the vm/ tree.
//
// The VM no longer pulls its config from the config bucket: the playbook in
// vm/ansible is pushed to it over SSH (scripts/cicd/infra_deploy.py), and its
// golden renders show a template change in the PR's own diff. The bucket stays
// for now -- a rollback to a tree from before that change uploads into it again
// -- and goes in a later, approved change.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
)

type Buckets struct {
	Config *storage.Bucket
	Backup *storage.Bucket
	// BuildSource stages the source tarball for `make build`. See New.
	BuildSource *storage.Bucket
	// ConfigHash changes whenever any file under vm/ changes. It is written into
	// instance metadata so a config change is visible from `describe`, and so
	// there is something to compare against when debugging drift.
	ConfigHash string
}

func New(ctx *pulumi.Context, cfg *config.Config, vmDir string, deps []pulumi.Resource) (*Buckets, error) {
	opts := pulumi.DependsOn(deps)

	configBucket, err := storage.NewBucket(ctx, "gitea-config", &storage.BucketArgs{
		Name:     pulumi.Sprintf("%s-gitea-config", cfg.Project),
		Location: pulumi.String(strings.ToUpper(cfg.Region)),
		// Uniform access: per-object ACLs are a footgun and IAM already covers it.
		UniformBucketLevelAccess: pulumi.Bool(true),
		PublicAccessPrevention:   pulumi.String("enforced"),
		Versioning:               &storage.BucketVersioningArgs{Enabled: pulumi.Bool(true)},
		ForceDestroy:             pulumi.Bool(true),
	}, opts)
	if err != nil {
		return nil, err
	}

	backupBucket, err := storage.NewBucket(ctx, "gitea-backups", &storage.BucketArgs{
		Name:                     pulumi.Sprintf("%s-gitea-backups", cfg.Project),
		Location:                 pulumi.String(strings.ToUpper(cfg.Region)),
		UniformBucketLevelAccess: pulumi.Bool(true),
		PublicAccessPrevention:   pulumi.String("enforced"),
		Versioning:               &storage.BucketVersioningArgs{Enabled: pulumi.Bool(true)},
		LifecycleRules: storage.BucketLifecycleRuleArray{
			&storage.BucketLifecycleRuleArgs{
				Action: &storage.BucketLifecycleRuleActionArgs{
					Type:         pulumi.String("SetStorageClass"),
					StorageClass: pulumi.String("NEARLINE"),
				},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(30)},
			},
			&storage.BucketLifecycleRuleArgs{
				Action:    &storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(365)},
			},
		},
		// No ForceDestroy: dumps are the disaster-recovery path and should not
		// disappear because someone ran `pulumi destroy`.
	}, opts)
	if err != nil {
		return nil, err
	}

	// Where `gcloud builds submit` stages its source tarball. Builds run as
	// cb-image@, which needs storage.objects.get on that tarball. The default
	// staging bucket is <project>_cloudbuild, created by gcloud on the first
	// submit -- after `pulumi up`, so there is nothing to bind to in advance --
	// and project-wide objectViewer would also open the backup and state
	// buckets. A dedicated bucket keeps the grant exact. Triggered builds fetch
	// source through the GitHub connection and never touch it.
	buildSourceBucket, err := storage.NewBucket(ctx, "gitea-build-source", &storage.BucketArgs{
		Name:                     pulumi.Sprintf("%s-gitea-build-source", cfg.Project),
		Location:                 pulumi.String(strings.ToUpper(cfg.Region)),
		UniformBucketLevelAccess: pulumi.Bool(true),
		PublicAccessPrevention:   pulumi.String("enforced"),
		// Tarballs are only read once, by the build they were uploaded for.
		LifecycleRules: storage.BucketLifecycleRuleArray{
			&storage.BucketLifecycleRuleArgs{
				Action:    &storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(7)},
			},
		},
		ForceDestroy: pulumi.Bool(true),
	}, opts)
	if err != nil {
		return nil, err
	}

	hash, err := hashTree(vmDir)
	if err != nil {
		return nil, err
	}

	return &Buckets{Config: configBucket, Backup: backupBucket, BuildSource: buildSourceBucket, ConfigHash: hash}, nil
}

// hashTree returns a content hash of the vm/ tree, skipping dot-directories
// (a local Ansible or Python cache, say).
func hashTree(vmDir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(vmDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != vmDir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walking %s: %w", vmDir, err)
	}
	// Deterministic order, so the hash is stable across machines.
	sort.Strings(paths)

	sum := sha256.New()
	for _, p := range paths {
		rel, err := filepath.Rel(vmDir, p)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		sum.Write([]byte(filepath.ToSlash(rel)))
		sum.Write(content)
	}
	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}
