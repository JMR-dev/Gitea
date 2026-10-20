// Package project enables the CI project's APIs. Everything else waits on
// them: a resource created against an API that is still enabling fails in
// confusing ways.
package project

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

var apis = []string{
	// The pipeline itself.
	"cloudbuild.googleapis.com",
	"pubsub.googleapis.com",
	"run.googleapis.com",
	"cloudfunctions.googleapis.com",
	"artifactregistry.googleapis.com", // where Cloud Run functions keeps the relay's images
	"cloudscheduler.googleapis.com",
	"secretmanager.googleapis.com",
	"logging.googleapis.com",
	"monitoring.googleapis.com",
	"iam.googleapis.com",
	"iamcredentials.googleapis.com",
	"storage.googleapis.com",
	"cloudresourcemanager.googleapis.com",
	"serviceusage.googleapis.com",

	// The app project's APIs, here too. A call made with a service account's
	// credentials can be checked against -- and billed to -- the account's own
	// project rather than the resource's, and these accounts live here. The
	// release engine hit exactly that with Cloud Resource Manager. Enabling an
	// API is free.
	"compute.googleapis.com",
	"container.googleapis.com",
	"dns.googleapis.com",
	"iap.googleapis.com",
	"oslogin.googleapis.com",
}

// EnableAPIs returns the services, for DependsOn.
func EnableAPIs(ctx *pulumi.Context, cfg *config.Config) ([]pulumi.Resource, error) {
	var out []pulumi.Resource
	for _, api := range apis {
		s, err := projects.NewService(ctx, api, &projects.ServiceArgs{
			Project: pulumi.String(cfg.Project),
			Service: pulumi.String(api),
			// Never disable on destroy: other things in the project, and the
			// state bucket bootstrap.py made, may depend on them.
			DisableOnDestroy: pulumi.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
