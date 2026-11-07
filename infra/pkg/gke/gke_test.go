package gke

import (
	"slices"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/network"
)

const (
	clusterType = "gcp:container/cluster:Cluster"
	accountType = "gcp:serviceaccount/account:Account"
)

// key names a registered resource by type token and name: the type alone
// isn't enough, since there are several node pools.
func key(typeToken, name string) string { return typeToken + "/" + name }

// recorder is a Pulumi mock that keeps what the program registers.
type recorder struct {
	mu  sync.Mutex
	got map[string]resource.PropertyMap
}

func (r *recorder) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[key(a.TypeToken, a.Name)] = a.Inputs
	state := a.Inputs.Copy()
	// So the node SA's email, which the cluster and the pools name, is known.
	if a.TypeToken == accountType {
		state["email"] = resource.NewStringProperty("gitea-gke-nodes@p.iam.gserviceaccount.com")
	}
	return a.Name + "-id", state, nil
}

func (r *recorder) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

// run builds the network and the cluster with the runners on, no burst, heavy
// or arm64 pool, and no base commitment.
func run(t *testing.T) map[string]resource.PropertyMap {
	t.Helper()
	cfg := &config.Config{
		Project:           "p",
		Region:            "us-east1",
		Zone:              "us-east1-b",
		RunnersEnabled:    true,
		RunnerMachineType: "e2-standard-2",
		OpsMachineType:    "e2-standard-2",
	}
	r := &recorder{got: map[string]resource.PropertyMap{}}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		net, err := network.New(ctx, cfg, nil)
		if err != nil {
			return err
		}
		_, err = New(ctx, cfg, net, nil)
		return err
	}, pulumi.WithMocks("p", "s", r))
	if err != nil {
		t.Fatal(err)
	}
	return r.got
}

func cluster(t *testing.T) resource.PropertyMap {
	t.Helper()
	c, ok := run(t)[key(clusterType, Name)]
	if !ok {
		t.Fatalf("no %s named %q was registered", clusterType, Name)
	}
	return c
}

// get walks nested objects along path. It reports false when a step is
// missing, null or not an object, rather than panicking.
func get(m resource.PropertyMap, path ...string) (resource.PropertyValue, bool) {
	var v resource.PropertyValue
	for i, k := range path {
		if !m.HasValue(resource.PropertyKey(k)) {
			return resource.PropertyValue{}, false
		}
		v = m[resource.PropertyKey(k)]
		if i < len(path)-1 {
			if !v.IsObject() {
				return resource.PropertyValue{}, false
			}
			m = v.ObjectValue()
		}
	}
	return v, true
}

func stringList(t *testing.T, v resource.PropertyValue, what string) []string {
	t.Helper()
	if !v.IsArray() {
		t.Fatalf("%s is not a list: %v", what, v)
	}
	var out []string
	for _, e := range v.ArrayValue() {
		if !e.IsString() {
			t.Fatalf("%s holds a non-string: %v", what, e)
		}
		out = append(out, e.StringValue())
	}
	return out
}

// GKE's default monitoring packages (kube-state-metrics, cAdvisor, Kubelet,
// DCGM and the rest) bill as Managed Prometheus samples. Only the free
// kubernetes.io/* system metrics are wanted.
func TestSystemMetricsOnly(t *testing.T) {
	v, ok := get(cluster(t), "monitoringConfig", "enableComponents")
	if !ok {
		t.Fatal("monitoringConfig.enableComponents is unset, so GKE's default packages stay on")
	}
	if got := stringList(t, v, "monitoringConfig.enableComponents"); !slices.Equal(got, []string{"SYSTEM_COMPONENTS"}) {
		t.Errorf("monitoringConfig.enableComponents = %q, want only SYSTEM_COMPONENTS", got)
	}
}

// Absent is not off: monitoringConfig is Optional+Computed, so with no
// managedPrometheus block GKE's default, on, stays.
func TestManagedPrometheusOff(t *testing.T) {
	v, ok := get(cluster(t), "monitoringConfig", "managedPrometheus", "enabled")
	if !ok {
		t.Fatal("monitoringConfig.managedPrometheus.enabled is unset, so GKE's default (on) wins")
	}
	if !v.IsBool() {
		t.Fatalf("monitoringConfig.managedPrometheus.enabled is not a bool: %v", v)
	}
	if v.BoolValue() {
		t.Error("monitoringConfig.managedPrometheus.enabled is true, want false")
	}
}

// Unset, logging keeps GKE's default, SYSTEM_COMPONENTS and WORKLOADS, and
// package telemetry's sink needs both: the runners' own log is a workload's,
// and the job pods' events and the autoscaler's log are the system's. If
// loggingConfig is ever set, it must keep them.
func TestLoggingKeepsWhatTheSinkReads(t *testing.T) {
	c := cluster(t)
	if !c.HasValue("loggingConfig") {
		return
	}
	v, ok := get(c, "loggingConfig", "enableComponents")
	if !ok {
		t.Fatal("loggingConfig is set without enableComponents")
	}
	got := stringList(t, v, "loggingConfig.enableComponents")
	for _, want := range []string{"SYSTEM_COMPONENTS", "WORKLOADS"} {
		if !slices.Contains(got, want) {
			t.Errorf("loggingConfig.enableComponents = %q, missing %s", got, want)
		}
	}
}
