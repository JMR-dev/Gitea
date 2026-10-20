// Package project enables the Google APIs the rest of the stack depends on.
package project

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Services are enabled explicitly so a fresh project works without anyone
// clicking through the console first.
var services = []string{
	"compute.googleapis.com",
	"dns.googleapis.com",
	"artifactregistry.googleapis.com",
	"cloudbuild.googleapis.com",
	"secretmanager.googleapis.com",
	"iap.googleapis.com",
	"storage.googleapis.com",
	"logging.googleapis.com",
	"monitoring.googleapis.com",
	"cloudscheduler.googleapis.com",
	"iamcredentials.googleapis.com",
	"oslogin.googleapis.com",
	// gcloud projects get-iam-policy, which scripts/ops/gitea_ops.py runs as
	// gitea-release@ for every backup's IAM export. Calls made with a service
	// account's credentials bill quota to the account's own project, so this
	// API has to be on here even though the operator's own calls never needed it.
	"cloudresourcemanager.googleapis.com",
	// Ungated, unlike the runner cluster itself: enabling an API costs nothing,
	// and scripts/bootstrap.sh enables the same list.
	"container.googleapis.com",
}

// EnableAPIs returns resources every other package should depend on, so nothing
// races an API that is still turning on.
func EnableAPIs(ctx *pulumi.Context) ([]pulumi.Resource, error) {
	var out []pulumi.Resource
	for _, s := range services {
		svc, err := projects.NewService(ctx, "api-"+s, &projects.ServiceArgs{
			Service: pulumi.String(s),
			// Tearing down the stack should not disable APIs that other things
			// in the project may be using.
			DisableOnDestroy:         pulumi.Bool(false),
			DisableDependentServices: pulumi.Bool(false),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, nil
}
