package telemetry

import (
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-cicd/pkg/config"
)

const testReader = "reader@example-project.iam.gserviceaccount.com"

func cloudRun(service, logID string) map[string]string {
	return map[string]string{
		"resource.type":                "cloud_run_revision",
		"resource.labels.service_name": service,
		"log_id":                       logID,
	}
}

func other(resourceType, logID string) map[string]string {
	return map[string]string{"resource.type": resourceType, "log_id": logID}
}

const (
	stderr   = "run.googleapis.com/stderr"
	requests = "run.googleapis.com/requests"
	system   = "run.googleapis.com/varlog/system"
	audit    = "cloudaudit.googleapis.com/activity"
)

func TestLogFilterIncludes(t *testing.T) {
	for name, e := range map[string]map[string]string{
		"webhook stderr":        cloudRun("gitea-webhook", stderr),
		"webhook requests":      cloudRun("gitea-webhook", requests),
		"build-status stderr":   cloudRun("build-status", stderr),
		"build-status requests": cloudRun("build-status", requests),
	} {
		if !matches(t, LogFilter(), e) {
			t.Errorf("%s: left out, should be sent", name)
		}
	}
}

func TestLogFilterExcludes(t *testing.T) {
	for name, e := range map[string]map[string]string{
		// cloudaudit carries the owner's email, whatever resource it sits on.
		"audit on the webhook": cloudRun("gitea-webhook", audit),
		"audit on the status":  cloudRun("build-status", "cloudaudit.googleapis.com/data_access"),
		"audit on the project": other("project", audit),
		"audit on a build":     other("build", audit),

		// The two named streams only.
		"webhook stdout":     cloudRun("gitea-webhook", "run.googleapis.com/stdout"),
		"webhook system log": cloudRun("gitea-webhook", system),
		"status system log":  cloudRun("build-status", system),

		// The two named functions only.
		"another service, stderr":   cloudRun("not-ours", stderr),
		"another service, requests": cloudRun("not-ours", requests),

		// What a deploy printed.
		"a build's log": other("build", "cloudbuild"),

		"same log, other type": other("cloud_function", stderr),
	} {
		if matches(t, LogFilter(), e) {
			t.Errorf("%s: sent, should be left out", name)
		}
	}
}

// The file is the filter as a reviewer reads it, and what the read-only gcloud
// check in the pull request ran.
func TestLogFilterGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/log-filter.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := LogFilter(); got != strings.TrimSpace(string(want)) {
		t.Errorf("filter changed; if on purpose, update testdata/log-filter.txt:\n%s", got)
	}
}

// recorder is a Pulumi mock that keeps what the program registers.
type recorder struct {
	mu   sync.Mutex
	got  map[string]resource.PropertyMap
	deps map[string][]string
}

func (r *recorder) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[a.TypeToken] = a.Inputs
	r.deps[a.TypeToken] = a.RegisterRPC.GetDependencies()
	state := a.Inputs.Copy()
	id := a.Name + "-id"
	switch a.TypeToken {
	case "gcp:logging/projectSink:ProjectSink":
		state["writerIdentity"] = resource.NewStringProperty("serviceAccount:sink-writer@example.com")
	case "gcp:projects/iAMCustomRole:IAMCustomRole":
		state["name"] = resource.NewStringProperty("projects/p/roles/" + metricsRoleID)
	case "gcp:pubsub/topic:Topic":
		id = "projects/p/topics/" + TopicName
	}
	return id, state, nil
}

func (r *recorder) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

func record(t *testing.T, cfg *config.Config) *recorder {
	t.Helper()
	r := &recorder{got: map[string]resource.PropertyMap{}, deps: map[string][]string{}}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return New(ctx, cfg, nil)
	}, pulumi.WithMocks("p", "s", r))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, cfg *config.Config) map[string]resource.PropertyMap {
	t.Helper()
	return record(t, cfg).got
}

// If the topic or the sink fails, the custom role must not have been created: the
// rollback would soft-delete it, and its ID is unusable from 7 to 37 days later.
func TestCustomRoleWaitsForTopicAndSink(t *testing.T) {
	deps := strings.Join(record(t, &config.Config{Project: "p", WatchtowerReader: testReader}).
		deps["gcp:projects/iAMCustomRole:IAMCustomRole"], " ")
	for _, want := range []string{"gcp:pubsub/topic:Topic", "gcp:logging/projectSink:ProjectSink"} {
		if !strings.Contains(deps, want) {
			t.Errorf("custom role doesn't wait for %s; it waits for: %q", want, deps)
		}
	}
}

func TestUnsetCreatesNothing(t *testing.T) {
	if got := run(t, &config.Config{Project: "p"}); len(got) != 0 {
		t.Fatalf("watchtowerReader unset, yet created %d resources", len(got))
	}
}

func str(m resource.PropertyMap, key string) string {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsString() {
		return ""
	}
	return v.StringValue()
}

func TestResourcesAndAccess(t *testing.T) {
	got := run(t, &config.Config{Project: "p", WatchtowerReader: testReader})

	const (
		topicT  = "gcp:pubsub/topic:Topic"
		subT    = "gcp:pubsub/subscription:Subscription"
		sinkT   = "gcp:logging/projectSink:ProjectSink"
		pubT    = "gcp:pubsub/topicIAMMember:TopicIAMMember"
		subIamT = "gcp:pubsub/subscriptionIAMMember:SubscriptionIAMMember"
		roleT   = "gcp:projects/iAMCustomRole:IAMCustomRole"
		memberT = "gcp:projects/iAMMember:IAMMember"
	)
	types := make([]string, 0, len(got))
	for k := range got {
		types = append(types, k)
	}
	slices.Sort(types)
	want := []string{topicT, subT, sinkT, pubT, subIamT, roleT, memberT}
	slices.Sort(want)
	if !slices.Equal(types, want) {
		t.Fatalf("resources:\n got %v\nwant %v", types, want)
	}

	if s := got[topicT]; str(s, "name") != "watchtower-logs" {
		t.Errorf("topic: %v", s)
	}

	s := got[subT]
	if str(s, "name") != "watchtower-logs-pull" || str(s, "messageRetentionDuration") != "604800s" ||
		s["ackDeadlineSeconds"].NumberValue() != 60 {
		t.Errorf("subscription: %v", s)
	}
	if ttl, ok := s["expirationPolicy"].ObjectValue()["ttl"]; !ok || ttl.StringValue() != "" {
		t.Errorf("subscription must never expire: %v", s["expirationPolicy"])
	}
	if s.HasValue("pushConfig") {
		t.Errorf("subscription must be pull: %v", s["pushConfig"])
	}

	sink := got[sinkT]
	if str(sink, "name") != "watchtower-ci" || str(sink, "filter") != LogFilter() ||
		!sink["uniqueWriterIdentity"].BoolValue() ||
		str(sink, "destination") != "pubsub.googleapis.com/projects/p/topics/watchtower-logs" {
		t.Errorf("sink: %v", sink)
	}

	if p := got[pubT]; str(p, "role") != "roles/pubsub.publisher" ||
		str(p, "member") != "serviceAccount:sink-writer@example.com" {
		t.Errorf("sink's publisher grant: %v", p)
	}

	// The reader's whole access: subscriber on the one subscription, the
	// custom role on the project, and nothing else.
	reader := "serviceAccount:" + testReader
	if m := got[subIamT]; str(m, "role") != "roles/pubsub.subscriber" || str(m, "member") != reader ||
		str(m, "subscription") != "watchtower-logs-pull" {
		t.Errorf("reader's subscriber grant: %v", m)
	}
	if m := got[memberT]; str(m, "role") != "projects/p/roles/"+metricsRoleID || str(m, "member") != reader {
		t.Errorf("reader's custom role grant: %v", m)
	}
	var perms []string
	for _, v := range got[roleT]["permissions"].ArrayValue() {
		perms = append(perms, v.StringValue())
	}
	slices.Sort(perms)
	if !slices.Equal(perms, []string{
		"monitoring.metricDescriptors.list",
		"monitoring.timeSeries.list",
	}) {
		t.Errorf("custom role permissions: %v", perms)
	}
}
