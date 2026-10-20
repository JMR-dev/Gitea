// Package relay deploys cicd/relay as two Cloud Run functions, and the
// Pub/Sub topics on either side of them.
//
//	Gitea webhook --HMAC--> gitea-webhook (public) --> ci-preview | ci-deploy | ci-gated
//	Cloud Build --> cloud-builds --push, OIDC--> build-status (private) --> Gitea commit status
//
// gitea-webhook is open to allUsers because Gitea cannot sign a Google
// identity token; the HMAC check in code is its authentication. build-status
// is not: Cloud Run admits only requests signed as ci-status-push, so a forged
// "build succeeded" cannot reach it. That split is why they are two
// functions -- passing an Authorization header through a public service is
// undocumented.
package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudfunctionsv2"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/pubsub"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
	"gitea-cicd/pkg/identity"
	cistorage "gitea-cicd/pkg/storage"
)

// Runtime is the functions' Go runtime. go125 is deprecated as of
// 2026-10-01; go127 is GA (`gcloud functions runtimes list`).
const Runtime = "go127"

// Topics, by the mode each carries (cicd/relay's Mode*), plus the weekly
// rebuild's and the one Cloud Build publishes every build's state to. That
// one must be named cloud-builds: Cloud Build publishes to it by name.
const (
	TopicPreview = "ci-preview"
	TopicDeploy  = "ci-deploy"
	TopicGated   = "ci-gated"
	TopicRebuild = "ci-rebuild"
	TopicBuilds  = "cloud-builds"
)

type Relay struct {
	Topics     map[string]*pubsub.Topic
	WebhookURL pulumi.StringOutput
}

func New(ctx *pulumi.Context, cfg *config.Config, acc *identity.Accounts, b *cistorage.Buckets,
	srcDir string, deps []pulumi.Resource) (*Relay, error) {
	opts := pulumi.DependsOn(deps)
	project := pulumi.String(cfg.Project)

	topics := map[string]*pubsub.Topic{}
	for _, name := range []string{TopicPreview, TopicDeploy, TopicGated, TopicRebuild, TopicBuilds} {
		t, err := pubsub.NewTopic(ctx, name, &pubsub.TopicArgs{
			Project: project, Name: pulumi.String(name),
			// A trigger that is disabled (autoDeploy off) never sees what
			// was published meanwhile; nothing should pile up for it.
			MessageRetentionDuration: pulumi.String("86400s"),
		}, opts)
		if err != nil {
			return nil, err
		}
		topics[name] = t
	}
	for _, name := range []string{TopicPreview, TopicDeploy, TopicGated} {
		if _, err := pubsub.NewTopicIAMMember(ctx, "relay-publish-"+name, &pubsub.TopicIAMMemberArgs{
			Project: project, Topic: topics[name].Name,
			Role: pulumi.String("roles/pubsub.publisher"), Member: pulumi.Sprintf("serviceAccount:%s", acc.Relay.Email),
		}, opts); err != nil {
			return nil, err
		}
	}

	// Named by content, so a code change is a new object and a redeploy.
	sum, err := treeHash(srcDir)
	if err != nil {
		return nil, err
	}
	src, err := storage.NewBucketObject(ctx, "relay-source", &storage.BucketObjectArgs{
		Bucket: b.RelaySource.Name,
		Name:   pulumi.String("relay-" + sum + ".zip"),
		Source: pulumi.NewFileArchive(srcDir),
	}, opts)
	if err != nil {
		return nil, err
	}

	secretEnv := func(key, secret string) *cloudfunctionsv2.FunctionServiceConfigSecretEnvironmentVariableArgs {
		return &cloudfunctionsv2.FunctionServiceConfigSecretEnvironmentVariableArgs{
			Key: pulumi.String(key), ProjectId: project, Secret: pulumi.String(secret), Version: pulumi.String("latest"),
		}
	}
	env := pulumi.StringMap{
		"GITEA_URL":  pulumi.String(cfg.GiteaURL),
		"GITEA_REPO": pulumi.String(cfg.GiteaRepo),
	}
	fn := func(name, entry string, runAs pulumi.StringInput, extra pulumi.StringMap,
		secrets cloudfunctionsv2.FunctionServiceConfigSecretEnvironmentVariableArray) (*cloudfunctionsv2.Function, error) {
		vars := pulumi.StringMap{}
		for k, v := range env {
			vars[k] = v
		}
		for k, v := range extra {
			vars[k] = v
		}
		return cloudfunctionsv2.NewFunction(ctx, name, &cloudfunctionsv2.FunctionArgs{
			Project: project, Location: pulumi.String(cfg.Region), Name: pulumi.String(name),
			BuildConfig: &cloudfunctionsv2.FunctionBuildConfigArgs{
				Runtime: pulumi.String(Runtime), EntryPoint: pulumi.String(entry),
				Source: &cloudfunctionsv2.FunctionBuildConfigSourceArgs{
					StorageSource: &cloudfunctionsv2.FunctionBuildConfigSourceStorageSourceArgs{
						Bucket: b.RelaySource.Name, Object: src.Name,
					},
				},
				ServiceAccount: acc.FnBuild.Name,
			},
			ServiceConfig: &cloudfunctionsv2.FunctionServiceConfigArgs{
				ServiceAccountEmail: runAs,
				AvailableMemory:     pulumi.String("256M"),
				TimeoutSeconds:      pulumi.Int(60),
				// A burst of deliveries is a few at once; this bounds what a
				// flood of forged ones can cost before the HMAC check refuses it.
				MaxInstanceCount:           pulumi.Int(3),
				MinInstanceCount:           pulumi.Int(0),
				IngressSettings:            pulumi.String("ALLOW_ALL"),
				AllTrafficOnLatestRevision: pulumi.Bool(true),
				EnvironmentVariables:       vars,
				SecretEnvironmentVariables: secrets,
			},
		}, opts)
	}

	hook, err := fn("gitea-webhook", "GiteaWebhook", acc.Relay.Email, pulumi.StringMap{
		"ALLOWED_AUTHORS": pulumi.String(strings.Join(cfg.Authors, ",")),
		"TOPIC_PREVIEW":   topics[TopicPreview].ID(),
		"TOPIC_DEPLOY":    topics[TopicDeploy].ID(),
		"TOPIC_GATED":     topics[TopicGated].ID(),
	}, cloudfunctionsv2.FunctionServiceConfigSecretEnvironmentVariableArray{
		secretEnv("GITEA_WEBHOOK_SECRET", identity.WebhookSecret),
		secretEnv("GITEA_STATUS_TOKEN", identity.StatusToken),
	})
	if err != nil {
		return nil, err
	}
	status, err := fn("build-status", "BuildStatus", acc.Status.Email, nil,
		cloudfunctionsv2.FunctionServiceConfigSecretEnvironmentVariableArray{
			secretEnv("GITEA_STATUS_TOKEN", identity.StatusToken),
		})
	if err != nil {
		return nil, err
	}

	invoker := func(name string, f *cloudfunctionsv2.Function, m pulumi.StringInput) error {
		_, err := cloudrunv2.NewServiceIamMember(ctx, name, &cloudrunv2.ServiceIamMemberArgs{
			Project: project, Location: pulumi.String(cfg.Region), Name: f.Name,
			Role: pulumi.String("roles/run.invoker"), Member: m,
		}, opts)
		return err
	}
	if err := invoker("gitea-webhook-public", hook, pulumi.String("allUsers")); err != nil {
		return nil, err
	}
	if err := invoker("build-status-push", status, pulumi.Sprintf("serviceAccount:%s", acc.StatusPush.Email)); err != nil {
		return nil, err
	}

	statusURL := status.ServiceConfig.Uri().Elem()
	if _, err := pubsub.NewSubscription(ctx, "build-status", &pubsub.SubscriptionArgs{
		Project: project, Name: pulumi.String("build-status-push"), Topic: topics[TopicBuilds].ID(),
		AckDeadlineSeconds:       pulumi.Int(60),
		MessageRetentionDuration: pulumi.String("86400s"),
		// Never expire: a quiet month must not delete the subscription.
		ExpirationPolicy: &pubsub.SubscriptionExpirationPolicyArgs{Ttl: pulumi.String("")},
		RetryPolicy: &pubsub.SubscriptionRetryPolicyArgs{
			MinimumBackoff: pulumi.String("10s"), MaximumBackoff: pulumi.String("600s"),
		},
		PushConfig: &pubsub.SubscriptionPushConfigArgs{
			PushEndpoint: statusURL,
			OidcToken: &pubsub.SubscriptionPushConfigOidcTokenArgs{
				ServiceAccountEmail: acc.StatusPush.Email, Audience: statusURL,
			},
		},
	}, opts); err != nil {
		return nil, err
	}

	return &Relay{Topics: topics, WebhookURL: hook.ServiceConfig.Uri().Elem()}, nil
}

// treeHash is a short content hash of every file under dir.
func treeHash(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		rel, _ := filepath.Rel(dir, p)
		h.Write([]byte(rel + "\x00"))
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}
