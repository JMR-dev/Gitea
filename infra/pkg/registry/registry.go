// Package registry holds the Artifact Registry repository the VM pulls from.
package registry

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
)

const RepoID = "gitea"

func New(ctx *pulumi.Context, cfg *config.Config, deps []pulumi.Resource) (*artifactregistry.Repository, error) {
	return artifactregistry.NewRepository(ctx, "gitea-repo", &artifactregistry.RepositoryArgs{
		RepositoryId: pulumi.String(RepoID),
		Location:     pulumi.String(cfg.Region),
		Format:       pulumi.String("DOCKER"),
		Description:  pulumi.String("Gitea and Caddy images, Debian 13 base"),
		CleanupPolicies: artifactregistry.RepositoryCleanupPolicyArray{
			// Untagged layers accumulate fast with a weekly rebuild.
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id:     pulumi.String("delete-untagged"),
				Action: pulumi.String("DELETE"),
				Condition: &artifactregistry.RepositoryCleanupPolicyConditionArgs{
					TagState:  pulumi.String("UNTAGGED"),
					OlderThan: pulumi.String("604800s"), // 7 days
				},
			},
			// Keep enough tagged versions to roll back a few deploys.
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id:     pulumi.String("keep-recent"),
				Action: pulumi.String("KEEP"),
				MostRecentVersions: &artifactregistry.RepositoryCleanupPolicyMostRecentVersionsArgs{
					KeepCount: pulumi.Int(20),
				},
			},
		},
	}, pulumi.DependsOn(deps))
}
