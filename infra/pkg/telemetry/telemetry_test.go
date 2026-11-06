package telemetry

import (
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/gke"
	"gitea-infra/pkg/network"
	"gitea-infra/pkg/ops"
	"gitea-infra/pkg/runner"
)

const testReader = "reader@example-project.iam.gserviceaccount.com"

// container is a log entry in the fields the filter reads. Its namespace is ""
// for the node logs GKE files under k8s_container with none. Its container is
// the runner's, so a case that leaves it out says something about the rest.
func container(cluster, namespace, logID string) map[string]string {
	return map[string]string{
		"resource.type":                  "k8s_container",
		"resource.labels.cluster_name":   cluster,
		"resource.labels.namespace_name": namespace,
		"resource.labels.container_name": runner.ContainerName,
		"log_id":                         logID,
	}
}

func namedContainer(name, logID string) map[string]string {
	e := container(gke.Name, runner.Namespace, logID)
	e["resource.labels.container_name"] = name
	return e
}

func nat(gateway, logID string) map[string]string {
	return map[string]string{
		"resource.type":                "nat_gateway",
		"resource.labels.gateway_name": gateway,
		"log_id":                       logID,
	}
}

func pod(cluster, namespace, logID string) map[string]string {
	return map[string]string{
		"resource.type":                  "k8s_pod",
		"resource.labels.cluster_name":   cluster,
		"resource.labels.namespace_name": namespace,
		"log_id":                         logID,
	}
}

func clusterLog(logID string) map[string]string {
	return map[string]string{
		"resource.type":                "k8s_cluster",
		"resource.labels.cluster_name": gke.Name,
		"log_id":                       logID,
	}
}

func other(resourceType, logID string) map[string]string {
	return map[string]string{"resource.type": resourceType, "log_id": logID}
}

const (
	autoscaler = "container.googleapis.com/cluster-autoscaler-visibility"
	natFlows   = "compute.googleapis.com/nat_flows"
	audit      = "cloudaudit.googleapis.com/activity"
	dataAccess = "cloudaudit.googleapis.com/data_access"
	serial     = "serialconsole.googleapis.com/serial_port_1_output"
)

func TestLogFilterIncludes(t *testing.T) {
	for name, e := range map[string]map[string]string{
		"runner stdout":        container(gke.Name, runner.Namespace, "stdout"),
		"runner stderr":        container(gke.Name, runner.Namespace, "stderr"),
		"job pod events":       pod(gke.Name, runner.JobsNamespace, "events"),
		"arm64 job pod events": pod(gke.Name, runner.ArmJobsNamespace, "events"),
		"the autoscaler":       clusterLog(autoscaler),
		"NAT":                  nat(network.RunnerNATName, natFlows),
	} {
		if !matches(t, LogFilter(), e) {
			t.Errorf("%s: left out, should be sent", name)
		}
	}
}

func TestLogFilterExcludes(t *testing.T) {
	for name, e := range map[string]map[string]string{
		// Job output, stdout and stderr, both job namespaces.
		"job stdout":       container(gke.Name, runner.JobsNamespace, "stdout"),
		"job stderr":       container(gke.Name, runner.JobsNamespace, "stderr"),
		"arm64 job stdout": container(gke.Name, runner.ArmJobsNamespace, "stdout"),
		"arm64 job stderr": container(gke.Name, runner.ArmJobsNamespace, "stderr"),

		// GKE's own namespaces, and the release engine's, as containers and as events.
		"kube-system container": container(gke.Name, "kube-system", "stderr"),
		"gmp-system container":  container(gke.Name, "gmp-system", "stderr"),
		"gitea-ops container":   container(gke.Name, ops.Namespace, "stdout"),
		"kube-system events":    pod(gke.Name, "kube-system", "events"),
		"gmp-system events":     pod(gke.Name, "gmp-system", "events"),
		"gitea-ops events":      pod(gke.Name, ops.Namespace, "events"),

		// Events are the job namespaces' only: the runners' own come as logs.
		"runner pod events": pod(gke.Name, runner.Namespace, "events"),

		// cloudaudit carries the owner's email, whatever resource it sits on.
		"audit on the cluster":       clusterLog(audit),
		"data access on the cluster": clusterLog(dataAccess),
		"audit on a runner":          container(gke.Name, runner.Namespace, audit),
		"audit on a job pod":         pod(gke.Name, runner.JobsNamespace, audit),
		"audit on the NAT":           nat(network.RunnerNATName, audit),
		"audit on the project":       other("project", audit),
		"audit on GKE":               other("gke_cluster", audit),

		"serial console":        other("gce_instance", serial),
		"cluster events":        clusterLog("events"),
		"the VM's stdout":       other("gce_instance", "stdout"),
		"NAT type, another log": nat(network.RunnerNATName, "stdout"),

		// The one gateway and the one container, however alike the rest.
		"another NAT gateway":         nat("not-ours", natFlows),
		"a NAT with no gateway label": other("nat_gateway", natFlows),
		"a sidecar beside a runner":   namedContainer("sidecar", "stdout"),
		"a runner pod's dockerd":      namedContainer("dockerd", "stderr"),
		"no container label":          namedContainer("", "stdout"),

		// Node logs, under k8s_container with no namespace.
		"guest agent":  container(gke.Name, "", "GCEGuestAgent"),
		"node stdout":  container(gke.Name, "", "stdout"),
		"startup logs": container(gke.Name, "", "google_metadata_script_runner"),

		// Another cluster is another team's, however alike its names.
		"another cluster's runner": container("not-ours", runner.Namespace, "stdout"),
		"another cluster's events": pod("not-ours", runner.JobsNamespace, "events"),
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

// A deployer without the telemetry roles fails on the topic. The custom role must
// wait for it, or the rollback soft-deletes a role whose ID is then unusable from
// 7 to 37 days later.
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
