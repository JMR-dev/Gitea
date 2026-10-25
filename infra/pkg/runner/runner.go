// Package runner deploys the Gitea Actions runners into the GKE cluster from
// package gke, the NetworkPolicies that keep jobs away from the metadata
// server, the plumbing that gets the registration token into the runner pods
// without Pulumi ever handling it, and the fixed tunnel-pod count running jobs
// depend on (tunnel.go). There are two runners, with disjoint labels:
//
//   - The pod runner takes `runs-on: ubuntu-*`. It uses the runner's kubernetes
//     backend: every job is its own unprivileged pod in JobsNamespace, so jobs
//     fan out over the node pool and the autoscaler adds nodes while they
//     queue. It cannot run Docker actions, `docker://` steps or `docker build`.
//   - The DinD runner takes `runs-on: docker`, for exactly those. A StatefulSet
//     of the official rootless Docker-in-Docker image, one pod per node.
//
// Threat model, in one paragraph: DinD jobs run inside that runner pod's own
// dockerd, and they get its socket (that is what makes `docker build` work). A
// job there can therefore read anything that daemon can see -- the runner's
// /data/.runner credentials and the registration token included. That is
// accepted: this is a single-tenant instance with registration disabled, so
// every workflow author is already trusted. Pod jobs share nothing with their
// runner but the cache port, and run under the baseline Pod Security level.
// What is NOT accepted is a job reaching Google Cloud or the cluster. The node
// SA holds nothing useful (package gke), the runners' Kubernetes service
// accounts hold only read access to that one token, the pod runner's API token
// reaches only JobsNamespace, and the NetworkPolicies below stop jobs from
// asking the metadata server or the API server for anything.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	rbacv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/rbac/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/gke"
	"gitea-infra/pkg/network"
	"gitea-infra/pkg/secrets"
)

const (
	// Namespace holds both runners.
	Namespace = "gitea-runner"
	// JobsNamespace holds the pod runner's job pods and nothing else. It is
	// the only namespace the pod runner's API token can touch, and it does not
	// admit privileged pods.
	JobsNamespace = "gitea-jobs"
	// KSAName and PodsKSAName are the Kubernetes service accounts the DinD and
	// pod runners run as. They are spelled out in the Workload Identity
	// principals, so renaming one means its IAM grant moves with it.
	KSAName     = "gitea-runner"
	PodsKSAName = "gitea-runner-pods"

	// The DinD runner's label, and the selector of its StatefulSet: immutable,
	// so it keeps the name it had when it was the only runner.
	appLabel     = "gitea-runner"
	podsAppLabel = "gitea-runner-pods"

	// heavyJobImage is the image `runs-on: heavy` jobs run in (the same one
	// ubuntu-latest jobs use). The pre-pull DaemonSet keeps it resident on
	// heavy nodes so a reused node starts a job without pulling it again.
	heavyJobImage = "docker.io/gitea/runner-images:ubuntu-latest"

	spcName   = "gitea-runner-registration-token"
	tokenDir  = "/var/run/secrets/gitea-runner"
	configDir = "/etc/gitea-runner"
	// The rootless image's dockerd reads $XDG_CONFIG_HOME/docker/daemon.json,
	// i.e. this directory -- NOT /etc/docker, which it ignores.
	dockerConfigDir = "/home/rootless/.config/docker"
	// The docker-data PVC mounts at the rootless default data root, but dockerd
	// is pointed one level down. It chmods its data root whenever that already
	// exists, and the mount point is owned by host root -- which fsGroup does
	// not change, and which is unmapped inside rootlesskit's user namespace --
	// so `chmod ...: operation not permitted` kills the daemon on every start.
	// A subdirectory dockerd creates itself is owned by its own (namespaced)
	// root, and fsGroup makes the mount point group-writable for it to do so.
	dockerDataMount = "/home/rootless/.local/share/docker"
	dockerDataRoot  = dockerDataMount + "/data"

	// Fixed rather than random, so the NetworkPolicies can name it. Jobs
	// reach their runner's cache server on the runner pod's IP at this port.
	cachePort = 8088
	// The pod runner's /healthz, for its readiness probe. Only the kubelet
	// reaches it: kubelet probes come from the node, which no policy blocks.
	metricsPort = 9101

	// GCE's default VPC MTU. Docker defaults to 1500 inside, and the mismatch
	// shows up as TLS handshakes that hang on large certificates.
	mtu = 1460
)

type Runner struct {
	Provider *kubernetes.Provider
	// StatefulSet is the DinD runner, PodsStatefulSet the pod runner.
	StatefulSet     *appsv1.StatefulSet
	PodsStatefulSet *appsv1.StatefulSet
}

// New returns nil (without error) when the cluster was not created.
func New(ctx *pulumi.Context, cfg *config.Config, gk *gke.Cluster, deps []pulumi.Resource) (*Runner, error) {
	if gk == nil {
		return nil, nil
	}
	opts := pulumi.DependsOn(deps)

	k8s, err := provider(ctx, gk)
	if err != nil {
		return nil, err
	}
	// Every Kubernetes object goes through the explicit provider (Pulumi.yaml
	// disables the default one, so forgetting this is an error rather than a
	// deploy into whatever kube context is current). DeletedWith: when the
	// cluster is deleted, the engine skips its objects' individual deletes,
	// which would otherwise run against a token in state that expired long
	// ago. How this plays out when the cluster is REPLACED rather than deleted
	// is not verified; if a replacement leaves objects stuck in state, run once
	// with PULUMI_K8S_DELETE_UNREACHABLE=true (docs/runners.md).
	k8sOpts := []pulumi.ResourceOption{opts, pulumi.Provider(k8s), pulumi.DeletedWith(gk.Cluster)}

	if err := pinTunnelAgents(ctx, k8sOpts); err != nil {
		return nil, err
	}

	ns, err := corev1.NewNamespace(ctx, "gitea-runner", &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String(Namespace),
			Labels: pulumi.StringMap{
				// GKE does not enforce Pod Security admission by default; the
				// label states the intent. Docker-in-Docker is privileged by
				// definition.
				"pod-security.kubernetes.io/enforce": pulumi.String("privileged"),
			},
		},
	}, k8sOpts...)
	if err != nil {
		return nil, err
	}
	inNS := with(k8sOpts, pulumi.DependsOn([]pulumi.Resource{ns}))

	ksa, err := corev1.NewServiceAccount(ctx, "gitea-runner", &corev1.ServiceAccountArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(KSAName),
			Namespace: pulumi.String(Namespace),
		},
		// In DinD mode the runner never talks to the Kubernetes API, so there is
		// no reason to hand every job a token for it.
		AutomountServiceAccountToken: pulumi.Bool(false),
	}, inNS...)
	if err != nil {
		return nil, err
	}

	proj, err := organizations.LookupProject(ctx, &organizations.LookupProjectArgs{
		ProjectId: pulumi.StringRef(cfg.Project),
	})
	if err != nil {
		return nil, err
	}
	grant, err := grantToken(ctx, cfg, gk, proj.Number, "runner-read-registration-token", KSAName, opts)
	if err != nil {
		return nil, err
	}

	// The registration token reaches the pod as a file mounted by the GKE
	// Secret Manager add-on, authenticated as the pod's KSA. Neither Pulumi nor
	// a Kubernetes Secret ever holds the value.
	spc, err := apiextensions.NewCustomResource(ctx, "gitea-runner-registration-token", &apiextensions.CustomResourceArgs{
		ApiVersion: pulumi.String("secrets-store.csi.x-k8s.io/v1"),
		Kind:       pulumi.String("SecretProviderClass"),
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(spcName),
			Namespace: pulumi.String(Namespace),
		},
		OtherFields: kubernetes.UntypedArgs{
			"spec": map[string]interface{}{
				"provider": "gke",
				"parameters": map[string]interface{}{
					"secrets": fmt.Sprintf("- resourceName: \"projects/%s/secrets/%s/versions/latest\"\n  path: \"token\"\n",
						cfg.Project, secrets.RunnerTokenName),
				},
			},
		},
		// The SecretProviderClass CRD is installed by the add-on after the
		// cluster reports ready; waiting for the node pool gives it time.
	}, with(inNS, pulumi.DependsOn([]pulumi.Resource{gk.NodePool}))...)
	if err != nil {
		return nil, err
	}

	daemon, err := daemonJSON()
	if err != nil {
		return nil, err
	}
	cm, err := corev1.NewConfigMap(ctx, "gitea-runner-config", &corev1.ConfigMapArgs{
		// No explicit name: Pulumi auto-names it, so a content change produces
		// a new name, which changes the pod template and rolls the runners.
		Metadata: &metav1.ObjectMetaArgs{Namespace: pulumi.String(Namespace)},
		Data: pulumi.StringMap{
			"config.yaml": pulumi.String(dindConfig(cfg)),
			"daemon.json": pulumi.String(daemon),
		},
	}, inNS...)
	if err != nil {
		return nil, err
	}

	netpol, err := networkPolicy(ctx, inNS)
	if err != nil {
		return nil, err
	}

	sts, err := statefulSet(ctx, cfg, ksa, cm, with(inNS,
		// No runner pod may ever exist without the egress policy in place, or
		// without the token it needs to register.
		pulumi.DependsOn([]pulumi.Resource{netpol, spc, grant, gk.NodePool}),
	))
	if err != nil {
		return nil, err
	}

	podsSts, err := podRunner(ctx, cfg, gk, proj.Number, opts, k8sOpts, inNS, []pulumi.Resource{netpol, spc})
	if err != nil {
		return nil, err
	}

	return &Runner{Provider: k8s, StatefulSet: sts, PodsStatefulSet: podsSts}, nil
}

// podRunner deploys the runner whose jobs are pods: their namespace, the
// runner's access to it, both NetworkPolicies it adds, and the runner itself.
// inNSDeps are the gitea-runner objects every runner pod waits for (the
// namespace fence and the token's SecretProviderClass).
func podRunner(
	ctx *pulumi.Context,
	cfg *config.Config,
	gk *gke.Cluster,
	projectNumber string,
	opts pulumi.ResourceOption,
	k8sOpts, inNS []pulumi.ResourceOption,
	inNSDeps []pulumi.Resource,
) (*appsv1.StatefulSet, error) {
	jobsNS, err := corev1.NewNamespace(ctx, "gitea-jobs", &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String(JobsNamespace),
			Labels: pulumi.StringMap{
				// Enforced: Pod Security admission is built into the API
				// server. Baseline rather than restricted, because job images
				// run as root -- but never privileged, never host namespaces
				// or hostPath. Nothing the runner creates needs more.
				"pod-security.kubernetes.io/enforce": pulumi.String("baseline"),
			},
		},
	}, k8sOpts...)
	if err != nil {
		return nil, err
	}
	inJobsNS := with(k8sOpts, pulumi.DependsOn([]pulumi.Resource{jobsNS}))

	ksa, err := corev1.NewServiceAccount(ctx, "gitea-runner-pods", &corev1.ServiceAccountArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(PodsKSAName),
			Namespace: pulumi.String(Namespace),
		},
	}, inNS...)
	if err != nil {
		return nil, err
	}

	grant, err := grantToken(ctx, cfg, gk, projectNumber, "runner-pods-read-registration-token", PodsKSAName, opts)
	if err != nil {
		return nil, err
	}

	// Exactly the verbs upstream's examples/kubernetes/kubernetes-pods.yaml
	// grants, and only in the jobs namespace -- so the runner cannot exec into
	// the DinD runner, or read the Secrets it creates for its own jobs back.
	role, err := rbacv1.NewRole(ctx, "gitea-runner-pods", &rbacv1.RoleArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(PodsKSAName),
			Namespace: pulumi.String(JobsNamespace),
		},
		Rules: rbacv1.PolicyRuleArray{
			rule("pods", "create", "get", "list", "delete"),
			rule("pods/exec", "create", "get"),
			rule("pods/log", "get"),
			rule("secrets", "create", "delete"),
		},
	}, inJobsNS...)
	if err != nil {
		return nil, err
	}
	binding, err := rbacv1.NewRoleBinding(ctx, "gitea-runner-pods", &rbacv1.RoleBindingArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(PodsKSAName),
			Namespace: pulumi.String(JobsNamespace),
		},
		RoleRef: &rbacv1.RoleRefArgs{
			ApiGroup: pulumi.String("rbac.authorization.k8s.io"),
			Kind:     pulumi.String("Role"),
			Name:     role.Metadata.Name().Elem(),
		},
		Subjects: rbacv1.SubjectArray{
			&rbacv1.SubjectArgs{
				Kind:      pulumi.String("ServiceAccount"),
				Name:      ksa.Metadata.Name().Elem(),
				Namespace: pulumi.String(Namespace),
			},
		},
	}, inJobsNS...)
	if err != nil {
		return nil, err
	}

	jobsPol, err := jobsNetworkPolicy(ctx, inJobsNS)
	if err != nil {
		return nil, err
	}
	podsPol, err := podsNetworkPolicy(ctx, inNS)
	if err != nil {
		return nil, err
	}

	cm, err := corev1.NewConfigMap(ctx, "gitea-runner-pods-config", &corev1.ConfigMapArgs{
		// Auto-named, like the DinD runner's, so a change rolls the pod.
		Metadata: &metav1.ObjectMetaArgs{Namespace: pulumi.String(Namespace)},
		Data:     pulumi.StringMap{"config.yaml": pulumi.String(podsConfig(cfg))},
	}, inNS...)
	if err != nil {
		return nil, err
	}

	// Keep the heavy job image resident on heavy nodes, so a job that reuses a
	// node still up from an earlier one does not re-pull it. Only with the pool.
	if cfg.RunnerHeavyMaxNodes > 0 {
		if _, err := heavyPrepull(ctx, inJobsNS); err != nil {
			return nil, err
		}
	}

	// No job pod may exist before its fence, and the runner cannot do its job
	// before it can reach the API server and create pods.
	deps := append([]pulumi.Resource{jobsPol, podsPol, role, binding, grant, gk.NodePool}, inNSDeps...)
	return podsStatefulSet(ctx, cfg, ksa, cm, with(inNS, pulumi.DependsOn(deps)))
}

// heavyPrepull runs one tiny pod per heavy node that holds the heavy job image
// in the node's image cache. On a brand-new node (the pool scales from zero)
// the image is still pulled once, shared with the first job's pod; the win is a
// node reused within a burst, which then starts a job without pulling. It is
// safe-to-evict and requests almost nothing, so it never holds a heavy node up
// against scale-down nor crowds out the one heavy job the node is sized for.
func heavyPrepull(ctx *pulumi.Context, inJobsNS []pulumi.ResourceOption) (*appsv1.DaemonSet, error) {
	labels := pulumi.StringMap{"app": pulumi.String("gitea-runner-heavy-prepull")}
	return appsv1.NewDaemonSet(ctx, "gitea-runner-heavy-prepull", &appsv1.DaemonSetArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("gitea-runner-heavy-prepull"),
			Namespace: pulumi.String(JobsNamespace),
			Labels:    labels,
		},
		Spec: &appsv1.DaemonSetSpecArgs{
			Selector: &metav1.LabelSelectorArgs{MatchLabels: labels},
			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Labels: labels,
					Annotations: pulumi.StringMap{
						// Must never keep a heavy node alive: scale-to-zero is the
						// point of the pool.
						"cluster-autoscaler.kubernetes.io/safe-to-evict": pulumi.String("true"),
					},
				},
				Spec: &corev1.PodSpecArgs{
					// Only heavy nodes, tolerating their taint like a heavy job.
					NodeSelector: pulumi.StringMap{gke.PoolKey: pulumi.String(gke.HeavyPoolValue)},
					Tolerations: corev1.TolerationArray{
						&corev1.TolerationArgs{
							Key:      pulumi.String(gke.PoolKey),
							Operator: pulumi.String("Equal"),
							Value:    pulumi.String(gke.HeavyPoolValue),
							Effect:   pulumi.String("NoSchedule"),
						},
					},
					AutomountServiceAccountToken:  pulumi.Bool(false),
					EnableServiceLinks:            pulumi.Bool(false),
					TerminationGracePeriodSeconds: pulumi.Int(0),
					Containers: corev1.ContainerArray{
						&corev1.ContainerArgs{
							Name:  pulumi.String("prepull"),
							Image: pulumi.String(heavyJobImage),
							// IfNotPresent: hold the cached image, do not re-pull.
							ImagePullPolicy: pulumi.String("IfNotPresent"),
							Command:         pulumi.ToStringArray([]string{"sleep", "infinity"}),
							Resources: &corev1.ResourceRequirementsArgs{
								Requests: pulumi.StringMap{
									"cpu":    pulumi.String("10m"),
									"memory": pulumi.String("32Mi"),
								},
								Limits: pulumi.StringMap{"memory": pulumi.String("64Mi")},
							},
							SecurityContext: &corev1.SecurityContextArgs{
								AllowPrivilegeEscalation: pulumi.Bool(false),
								Capabilities:             &corev1.CapabilitiesArgs{Drop: pulumi.ToStringArray([]string{"ALL"})},
								SeccompProfile:           &corev1.SeccompProfileArgs{Type: pulumi.String("RuntimeDefault")},
							},
						},
					},
				},
			},
		},
	}, inJobsNS...)
}

func rule(resource string, verbs ...string) *rbacv1.PolicyRuleArgs {
	return &rbacv1.PolicyRuleArgs{
		ApiGroups: pulumi.ToStringArray([]string{""}),
		Resources: pulumi.ToStringArray([]string{resource}),
		Verbs:     pulumi.ToStringArray(verbs),
	}
}

// provider builds a Kubernetes provider for the cluster. The kubeconfig carries
// no credential: it names the gke-gcloud-auth-plugin exec plugin, which mints a
// short-lived token from the caller's own application-default credentials each
// time Pulumi connects. So the acting identity is whoever runs Pulumi -- cb-infra
// on a deploy, cb-infra-preview on a pull-request preview -- and no token sits in
// state for a reader of the state to replay as a deployer. The deploy step's
// gcloud image (cloudbuild/infra.yaml) has the plugin on PATH; a workstation
// needs google-cloud-cli-gke-gcloud-auth-plugin.
func provider(ctx *pulumi.Context, gk *gke.Cluster) (*kubernetes.Provider, error) {
	// Only the cluster's endpoint is needed, read from its own output (from
	// state at preview). No token is fetched, so the provider no longer depends
	// on a live GetClientConfig call; the plugin resolves the identity at
	// connect time instead.
	endpoint := gk.Cluster.ControlPlaneEndpointsConfig.DnsEndpointConfig().Endpoint()

	kubeconfig := endpoint.ApplyT(func(ep *string) (string, error) {
		if ep == nil || *ep == "" {
			return "", errors.New("cluster has no DNS endpoint -- check controlPlaneEndpointsConfig")
		}
		return kubeconfigJSON(*ep)
	}).(pulumi.StringOutput)

	return kubernetes.NewProvider(ctx, "gitea-runners", &kubernetes.ProviderArgs{
		// No ToSecret: the kubeconfig holds only the public endpoint and an exec
		// plugin name, no credential. Relative to the token-based kubeconfig
		// stored before, only the users[0].user block differs (an exec stanza in
		// place of the bearer token), so Pulumi treats the switch as an update,
		// not a replacement of the provider and every object under it.
		Kubeconfig: kubeconfig,
		// DeleteUnreachable is deliberately left unset. With it on, a refresh
		// that cannot reach the cluster reads as "cluster unreachable" and
		// silently drops every object from state; the next `up` then fails
		// with "already exists". DeletedWith covers the legitimate case.
	})
}

// kubeconfigJSON renders a kubeconfig whose user authenticates by running the
// gke-gcloud-auth-plugin exec plugin at connect time. JSON is valid YAML, so no
// YAML library is needed. There is no certificate-authority-data: the DNS
// endpoint presents a certificate from a public CA.
func kubeconfigJSON(endpoint string) (string, error) {
	server := "https://" + strings.TrimPrefix(endpoint, "https://")
	kc := map[string]interface{}{
		"apiVersion":      "v1",
		"kind":            "Config",
		"current-context": gke.Name,
		"clusters": []map[string]interface{}{{
			"name":    gke.Name,
			"cluster": map[string]interface{}{"server": server},
		}},
		"contexts": []map[string]interface{}{{
			"name":    gke.Name,
			"context": map[string]interface{}{"cluster": gke.Name, "user": "pulumi"},
		}},
		"users": []map[string]interface{}{{
			"name": "pulumi",
			// The plugin returns a bearer token derived from the caller's ADC
			// and makes no GKE API call, so minting one needs no cluster
			// permission. A bare command name, not an absolute path: the plugin
			// lives in a different directory in the image and on a workstation.
			"user": map[string]interface{}{
				"exec": map[string]interface{}{
					"apiVersion":         "client.authentication.k8s.io/v1beta1",
					"command":            "gke-gcloud-auth-plugin",
					"provideClusterInfo": false,
					"interactiveMode":    "Never",
				},
			},
		}},
	}
	b, err := json.Marshal(kc)
	return string(b), err
}

// grantToken lets one runner KSA -- and only it -- read the registration
// token, through a Workload Identity Federation principal. No Google service
// account is involved, so there is nothing to impersonate. One principal per
// KSA rather than a principalSet for the namespace, so a pod that runs as any
// other account in it gets nothing.
func grantToken(
	ctx *pulumi.Context,
	cfg *config.Config,
	gk *gke.Cluster,
	projectNumber, name, ksa string,
	opts pulumi.ResourceOption,
) (*secretmanager.SecretIamMember, error) {
	return secretmanager.NewSecretIamMember(ctx, name, &secretmanager.SecretIamMemberArgs{
		Project:  pulumi.String(cfg.Project),
		SecretId: pulumi.String(secrets.RunnerTokenName),
		Role:     pulumi.String("roles/secretmanager.secretAccessor"),
		Member: pulumi.String(fmt.Sprintf(
			"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
			projectNumber, cfg.Project, Namespace, ksa)),
		// The <project>.svc.id.goog pool does not exist until the first
		// Workload Identity cluster does, and IAM rejects principals in a pool
		// it cannot find.
	}, opts, pulumi.DependsOn([]pulumi.Resource{gk.Cluster}))
}

// runnerYAML renders the parts of config.yaml both runners share. Keys are from
// the v5.1.0 config.example.yaml; anything not set here keeps its default.
func runnerYAML(capacity int, shutdown string, labels []string) string {
	var lb strings.Builder
	for _, l := range labels {
		fmt.Fprintf(&lb, "    - %q\n", l)
	}

	return fmt.Sprintf(`# Rendered by infra/pkg/runner. Edit there, not in the cluster.
log:
  level: info

runner:
  # Absolute, on the runner-data PVC, so registration survives pod restarts.
  # Without it a recreated pod registers as a NEW runner and leaves a stale one.
  file: /data/.runner
  capacity: %d
  timeout: 3h
  shutdown_timeout: %s
  fetch_interval: 2s
  labels:
%s
cache:
  enabled: true
  dir: /data/cache
  port: %d
`, capacity, shutdown, strings.TrimRight(lb.String(), "\n"), cachePort)
}

// dindConfig is the DinD runner's config.yaml.
func dindConfig(cfg *config.Config) string {
	// The runners live on the on-demand base pool, where a drain for a node
	// upgrade waits up to an hour, so let running jobs use it. Must stay below
	// the pod's terminationGracePeriodSeconds.
	shutdown := "55m"

	// One label, disjoint from the pod runner's: a job that needs a Docker
	// daemon must say so with `runs-on: docker`, or it lands on a job pod that
	// has none.
	//
	// docker.io paths rather than docker.gitea.com: identical digests, but only
	// Docker Hub can be served through the registry mirror in daemon.json.
	// Every job behind one NAT IP would otherwise share Docker Hub's anonymous
	// per-IP pull limit.
	labels := []string{"docker:docker://docker.io/gitea/runner-images:ubuntu-latest"}

	return runnerYAML(cfg.RunnerCapacity, shutdown, labels) + `
container:
  # Empty: a fresh network per job.
  network: ""
  privileged: false
  # Empty allows none. Jobs that need host paths are a design smell here.
  valid_volumes: []
  # Empty: auto-detect DOCKER_HOST (the rootless unix socket) AND mount it into
  # job containers, which is what lets a job run ` + "`docker build`" + `. "-" would
  # auto-detect without mounting.
  docker_host: ""
`
}

// podsConfig is the pod runner's config.yaml.
func podsConfig(cfg *config.Config) string {
	// On a rollout or a node upgrade this lets every running job finish,
	// rather than failing all of them at once -- this one pod is the runner for
	// every ubuntu-* job in the cluster. Must stay below the pod's
	// terminationGracePeriodSeconds.
	shutdown := "55m"

	// Pulled by the nodes' containerd, not by a dockerd, so the daemon.json
	// mirror does not apply. Each node pulls an image once and keeps it.
	labels := []string{
		"ubuntu-latest:kubernetes://docker.io/gitea/runner-images:ubuntu-latest",
		"ubuntu-24.04:kubernetes://docker.io/gitea/runner-images:ubuntu-24.04",
		"ubuntu-22.04:kubernetes://docker.io/gitea/runner-images:ubuntu-22.04",
	}

	// At 0 the StatefulSet has no replicas and nothing reads this; the runner
	// would refuse to load a capacity of 0.
	capacity := max(1, cfg.RunnerJobCapacity)

	// The job container's requests are what make jobs fan out: the scheduler
	// places pods by them, and the autoscaler adds a burst node when a pending
	// pod fits on none. No CPU limit, so a job can burst into a node's idle
	// CPU. The memory limit keeps one runaway job from OOMing its neighbours.
	//
	// Every job pod tolerates the burst pool's taint, and nothing else does,
	// so that pool runs jobs and nothing a job depends on (package gke). No
	// nodeSelector: a job still takes room on a base node when there is any.
	//
	// `runs-on: heavy` gets the same image on the heavy pool, through a
	// pod_templates entry keyed by the label: it selects and tolerates the
	// pool, and requests enough CPU and memory that the autoscaler can only
	// satisfy it with a heavy node and fits one job per node. Unlike the burst
	// jobs, the job container's memory limit equals its request, so the request
	// sizes the whole node (gitea:runnerHeavyMemory ~= the node's usable
	// memory); there is no CPU limit, so the one job still bursts into all the
	// node's cores. One-per-node is now held by both requests (two no longer
	// fit by either CPU or memory). The $services sidecar from pod_template
	// still merges in, so a job that uses `services:` is Burstable, not
	// Guaranteed; the node budget leaves room for it. ephemeral-storage is set
	// explicitly: the strategic merge over pod_template is not relied on to
	// carry the base request through.
	var heavy string
	if cfg.RunnerHeavyMaxNodes > 0 {
		labels = append(labels, "heavy:kubernetes://"+heavyJobImage)
		heavy = fmt.Sprintf(`  pod_templates:
    heavy:
      spec:
        nodeSelector:
          %s: %s
        tolerations:
          - key: %s
            operator: Equal
            value: %s
            effect: NoSchedule
        containers:
          - name: job
            resources:
              requests:
                cpu: %q
                memory: %q
                ephemeral-storage: 2Gi
              limits:
                memory: %q
`, gke.PoolKey, gke.HeavyPoolValue, gke.PoolKey, gke.HeavyPoolValue,
			cfg.RunnerHeavyCPU, cfg.RunnerHeavyMemory, cfg.RunnerHeavyMemory)
	}

	// "$services" is literal, as in upstream's docs/kubernetes.md: config
	// values expand ${NAME}, and a container of that name merges into every
	// service container.
	return runnerYAML(capacity, shutdown, labels) + fmt.Sprintf(`
metrics:
  enabled: true
  addr: ":%d"

kubernetes:
  namespace: %s
  pod_template:
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      tolerations:
        - key: %s
          operator: Equal
          value: %s
          effect: NoSchedule
      containers:
        - name: job
          resources:
            requests:
              cpu: %q
              memory: %q
              ephemeral-storage: 2Gi
            limits:
              memory: %q
        - name: $services
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              memory: 1Gi
`, metricsPort, JobsNamespace, gke.PoolKey, gke.BurstPoolValue,
		cfg.RunnerJobCPU, cfg.RunnerJobMemory, double(cfg.RunnerJobMemory)) + heavy
}

// double doubles a resource quantity that config.validate has already matched,
// keeping its suffix: "1536Mi" -> "3072Mi", "1.5Gi" -> "3Gi".
func double(q string) string {
	i := strings.IndexFunc(q, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i < 0 {
		i = len(q)
	}
	v, err := strconv.ParseFloat(q[:i], 64)
	if err != nil {
		return q
	}
	return strconv.FormatFloat(2*v, 'f', -1, 64) + q[i:]
}

// daemonJSON configures the runner's inner dockerd.
func daemonJSON() (string, error) {
	b, err := json.Marshal(map[string]interface{}{
		// Google's Docker Hub pull-through cache. Free, anonymous, and not
		// subject to Docker Hub's per-IP limit. Hub-only: other registries are
		// pulled directly.
		"registry-mirrors": []string{"https://mirror.gcr.io"},
		"data-root":        dockerDataRoot,
		// "mtu" covers the default bridge; act creates a network per job, which
		// only default-network-opts reaches.
		"mtu": mtu,
		"default-network-opts": map[string]interface{}{
			"bridge": map[string]string{"com.docker.network.driver.mtu": fmt.Sprint(mtu)},
		},
	})
	return string(b), err
}

// networkPolicy fences everything in Namespace, both runners included. It is
// load-bearing, not defence in depth.
//
// DinD job containers run inside the runner pod's dockerd, so their traffic
// leaves from the runner pod's network namespace. The GKE metadata server
// identifies callers by pod IP -- it would hand any job the runner KSA's
// federated token, which can read the registration secret. Excluding
// 169.254.0.0/16 covers both addresses Dataplane V2 uses for it (.254 on port
// 80, .252 on port 988).
//
// The RFC1918 exclusions cover the node, pod and service ranges, the control
// plane's private address, and the VM's internal IP. Jobs reach Gitea the same
// way the rest of the internet does: its public address, through Caddy. The pod
// runner alone gets the control plane back, in podsNetworkPolicy.
//
// Ingress is denied outright, apart from the cache ports. That also covers the rootless image's Docker API,
// which listens on the pod IP at :2376 (mTLS) and cannot be switched off.
//
// The Secret Manager CSI mount is unaffected: the add-on fetches the token from
// its own node DaemonSet, not from inside this pod.
func networkPolicy(ctx *pulumi.Context, opts []pulumi.ResourceOption) (*networkingv1.NetworkPolicy, error) {
	runnerPods := &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(appLabel)}}
	cache := tcp(cachePort)

	return networkingv1.NewNetworkPolicy(ctx, "gitea-runner", &networkingv1.NetworkPolicyArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("gitea-runner"),
			Namespace: pulumi.String(Namespace),
		},
		Spec: &networkingv1.NetworkPolicySpecArgs{
			// Every pod in the namespace, not just the runners: nothing else
			// should be here, and anything that is gets the same fence.
			PodSelector: &metav1.LabelSelectorArgs{},
			PolicyTypes: pulumi.ToStringArray([]string{"Ingress", "Egress"}),

			Ingress: networkingv1.NetworkPolicyIngressRuleArray{
				// The DinD runner's cache server, for its jobs. Their traffic
				// most likely never leaves the pod's own namespace (and so is
				// never evaluated), but if the dataplane does see it, this is
				// the one thing that should be let through.
				&networkingv1.NetworkPolicyIngressRuleArgs{
					From:  networkingv1.NetworkPolicyPeerArray{&networkingv1.NetworkPolicyPeerArgs{PodSelector: runnerPods}},
					Ports: cache,
				},
			},

			Egress: networkingv1.NetworkPolicyEgressRuleArray{
				dnsEgress(),
				&networkingv1.NetworkPolicyEgressRuleArgs{
					To:    networkingv1.NetworkPolicyPeerArray{&networkingv1.NetworkPolicyPeerArgs{PodSelector: runnerPods}},
					Ports: cache,
				},
				internetEgress(),
			},
		},
	}, opts...)
}

// podsNetworkPolicy adds to networkPolicy for the pod runner alone. Policies
// are additive, so it only opens what that runner needs beyond the fence:
// its jobs' cache traffic, and the API server.
func podsNetworkPolicy(ctx *pulumi.Context, opts []pulumi.ResourceOption) (*networkingv1.NetworkPolicy, error) {
	return networkingv1.NewNetworkPolicy(ctx, "gitea-runner-pods", &networkingv1.NetworkPolicyArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(PodsKSAName),
			Namespace: pulumi.String(Namespace),
		},
		Spec: &networkingv1.NetworkPolicySpecArgs{
			PodSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(podsAppLabel)}},
			PolicyTypes: pulumi.ToStringArray([]string{"Ingress", "Egress"}),
			Ingress: networkingv1.NetworkPolicyIngressRuleArray{
				// Job pods, to the runner's cache server. Unlike DinD jobs,
				// theirs is real pod-to-pod traffic, so this rule is needed.
				&networkingv1.NetworkPolicyIngressRuleArgs{
					From:  networkingv1.NetworkPolicyPeerArray{&networkingv1.NetworkPolicyPeerArgs{NamespaceSelector: namespace(JobsNamespace)}},
					Ports: tcp(cachePort),
				},
			},
			Egress: networkingv1.NetworkPolicyEgressRuleArray{
				// The API server, to create and exec into job pods. Its private
				// endpoint is a Private Service Connect address in the node
				// subnet (the `kubernetes` EndpointSlice), and Dataplane V2
				// evaluates policy after a Service address is translated to
				// it -- so the rule names the node subnet, not the Service IP
				// in the services range.
				&networkingv1.NetworkPolicyEgressRuleArgs{
					To: networkingv1.NetworkPolicyPeerArray{
						&networkingv1.NetworkPolicyPeerArgs{IpBlock: &networkingv1.IPBlockArgs{Cidr: pulumi.String(network.RunnerNodeCIDR)}},
					},
					Ports: tcp(443),
				},
			},
		},
	}, opts...)
}

// jobsNetworkPolicy fences JobsNamespace the way networkPolicy fences the
// runners: no ingress, and egress to DNS, the pod runner's cache server and the
// internet minus everything private and link-local. That last exclusion is
// what keeps job pods away from the metadata server and the API server, so it
// is load-bearing here too.
func jobsNetworkPolicy(ctx *pulumi.Context, opts []pulumi.ResourceOption) (*networkingv1.NetworkPolicy, error) {
	return networkingv1.NewNetworkPolicy(ctx, "gitea-jobs", &networkingv1.NetworkPolicyArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(JobsNamespace),
			Namespace: pulumi.String(JobsNamespace),
		},
		Spec: &networkingv1.NetworkPolicySpecArgs{
			PodSelector: &metav1.LabelSelectorArgs{},
			PolicyTypes: pulumi.ToStringArray([]string{"Ingress", "Egress"}),
			Egress: networkingv1.NetworkPolicyEgressRuleArray{
				dnsEgress(),
				&networkingv1.NetworkPolicyEgressRuleArgs{
					To: networkingv1.NetworkPolicyPeerArray{
						&networkingv1.NetworkPolicyPeerArgs{
							NamespaceSelector: namespace(Namespace),
							PodSelector:       &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(podsAppLabel)}},
						},
					},
					Ports: tcp(cachePort),
				},
				internetEgress(),
			},
		},
	}, opts...)
}

// dnsEgress allows cluster DNS. Without it, the RFC1918 exclusion in
// internetEgress would break name resolution for runners and jobs alike.
//
// Both labels: GKE enables NodeLocal DNSCache by default on new clusters, and
// then queries to the kube-dns Service IP are answered by the node-local-dns
// pod on the same node, not by kube-dns. Allowing kube-dns alone resolves
// nothing.
func dnsEgress() *networkingv1.NetworkPolicyEgressRuleArgs {
	return &networkingv1.NetworkPolicyEgressRuleArgs{
		To: networkingv1.NetworkPolicyPeerArray{
			&networkingv1.NetworkPolicyPeerArgs{
				NamespaceSelector: namespace("kube-system"),
				PodSelector: &metav1.LabelSelectorArgs{
					MatchExpressions: metav1.LabelSelectorRequirementArray{
						&metav1.LabelSelectorRequirementArgs{
							Key:      pulumi.String("k8s-app"),
							Operator: pulumi.String("In"),
							Values:   pulumi.ToStringArray([]string{"kube-dns", "node-local-dns"}),
						},
					},
				},
			},
		},
		Ports: networkingv1.NetworkPolicyPortArray{
			&networkingv1.NetworkPolicyPortArgs{Protocol: pulumi.String("UDP"), Port: pulumi.Int(53)},
			&networkingv1.NetworkPolicyPortArgs{Protocol: pulumi.String("TCP"), Port: pulumi.Int(53)},
		},
	}
}

// internetEgress allows the internet, minus everything private and link-local.
func internetEgress() *networkingv1.NetworkPolicyEgressRuleArgs {
	return &networkingv1.NetworkPolicyEgressRuleArgs{
		To: networkingv1.NetworkPolicyPeerArray{
			&networkingv1.NetworkPolicyPeerArgs{
				IpBlock: &networkingv1.IPBlockArgs{
					Cidr: pulumi.String("0.0.0.0/0"),
					Except: pulumi.ToStringArray([]string{
						"10.0.0.0/8",
						"172.16.0.0/12",
						"192.168.0.0/16",
						"169.254.0.0/16",
					}),
				},
			},
		},
	}
}

func namespace(name string) *metav1.LabelSelectorArgs {
	return &metav1.LabelSelectorArgs{
		MatchLabels: pulumi.StringMap{"kubernetes.io/metadata.name": pulumi.String(name)},
	}
}

func tcp(port int) networkingv1.NetworkPolicyPortArray {
	return networkingv1.NetworkPolicyPortArray{
		&networkingv1.NetworkPolicyPortArgs{Protocol: pulumi.String("TCP"), Port: pulumi.Int(port)},
	}
}

func statefulSet(
	ctx *pulumi.Context,
	cfg *config.Config,
	ksa *corev1.ServiceAccount,
	cm *corev1.ConfigMap,
	opts []pulumi.ResourceOption,
) (*appsv1.StatefulSet, error) {
	labels := pulumi.StringMap{"app": pulumi.String(appLabel)}

	// On-demand drains honour up to an hour; it must stay above
	// runner.shutdown_timeout (dindConfig).
	grace := 3600

	return appsv1.NewStatefulSet(ctx, "gitea-runner", &appsv1.StatefulSetArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("gitea-runner"),
			Namespace: pulumi.String(Namespace),
			Labels:    labels,
		},
		Spec: &appsv1.StatefulSetSpecArgs{
			// A StatefulSet rather than a Deployment for the stable identity:
			// gitea-runner-0 keeps its PVC, so it keeps its .runner registration
			// and its image cache across restarts and rescheduling.
			ServiceName:         pulumi.String("gitea-runner"),
			Replicas:            pulumi.Int(cfg.RunnerReplicas),
			PodManagementPolicy: pulumi.String("Parallel"),
			Selector:            &metav1.LabelSelectorArgs{MatchLabels: labels},

			PersistentVolumeClaimRetentionPolicy: deletePVCs(),
			VolumeClaimTemplates: corev1.PersistentVolumeClaimTypeArray{
				// .runner and the actions cache. 10Gi is pd-balanced's floor.
				pvc("runner-data", "10Gi"),
				// The inner dockerd's data root: pulled job images and build
				// layers. Without it every new pod re-pulls everything.
				pvc("docker-data", "20Gi"),
			},

			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: labels},
				Spec: &corev1.PodSpecArgs{
					ServiceAccountName:            ksa.Metadata.Name(),
					AutomountServiceAccountToken:  pulumi.Bool(false),
					EnableServiceLinks:            pulumi.Bool(false),
					TerminationGracePeriodSeconds: pulumi.Int(grace),

					// Required for the Secret Manager add-on on Standard
					// clusters. Every node in a GKE_METADATA pool carries it.
					NodeSelector: pulumi.StringMap{"iam.gke.io/gke-metadata-server-enabled": pulumi.String("true")},
					// One DinD runner per node, by construction rather than by
					// arithmetic on resource requests. gitea:runnerReplicas then
					// maps directly to nodes, and changing the machine type
					// cannot silently stack two Docker daemons on one box. Job
					// pods and the pod runner may still share the node.
					Affinity: &corev1.AffinityArgs{
						PodAntiAffinity: &corev1.PodAntiAffinityArgs{
							RequiredDuringSchedulingIgnoredDuringExecution: corev1.PodAffinityTermArray{
								&corev1.PodAffinityTermArgs{
									LabelSelector: &metav1.LabelSelectorArgs{MatchLabels: labels},
									TopologyKey:   pulumi.String("kubernetes.io/hostname"),
								},
							},
						},
					},

					// The rootless image runs as uid 1000; both PVCs must be
					// writable by it. OnRootMismatch, or every start re-chowns
					// the entire Docker data root.
					SecurityContext: &corev1.PodSecurityContextArgs{
						FsGroup:             pulumi.Int(1000),
						FsGroupChangePolicy: pulumi.String("OnRootMismatch"),
					},

					Volumes: corev1.VolumeArray{
						&corev1.VolumeArgs{
							Name: pulumi.String("config"),
							ConfigMap: &corev1.ConfigMapVolumeSourceArgs{
								Name:  cm.Metadata.Name(),
								Items: corev1.KeyToPathArray{&corev1.KeyToPathArgs{Key: pulumi.String("config.yaml"), Path: pulumi.String("config.yaml")}},
							},
						},
						&corev1.VolumeArgs{
							Name: pulumi.String("docker-config"),
							ConfigMap: &corev1.ConfigMapVolumeSourceArgs{
								Name:  cm.Metadata.Name(),
								Items: corev1.KeyToPathArray{&corev1.KeyToPathArgs{Key: pulumi.String("daemon.json"), Path: pulumi.String("daemon.json")}},
							},
						},
						&corev1.VolumeArgs{
							Name: pulumi.String("registration-token"),
							Csi: &corev1.CSIVolumeSourceArgs{
								Driver:           pulumi.String("secrets-store-gke.csi.k8s.io"),
								ReadOnly:         pulumi.Bool(true),
								VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(spcName)},
							},
						},
					},

					Containers: corev1.ContainerArray{
						&corev1.ContainerArgs{
							Name:  pulumi.String("runner"),
							Image: pulumi.String(cfg.RunnerImage()),
							// Rootless dockerd still needs a privileged container
							// for its user namespaces and mounts -- but the daemon
							// runs as uid 1000, so a job that escapes its own
							// container lands as an unprivileged user, not as
							// root on the node.
							SecurityContext: &corev1.SecurityContextArgs{Privileged: pulumi.Bool(true)},
							Env: corev1.EnvVarArray{
								// The public URL, through Caddy, exactly as any
								// other client. ROOT_URL is what Gitea builds
								// artifact and checkout URLs from anyway.
								env("GITEA_INSTANCE_URL", fmt.Sprintf("https://%s", cfg.Domain)),
								// run.sh reads this only when .runner does not
								// exist yet, and unsets it before starting the
								// daemon.
								env("GITEA_RUNNER_REGISTRATION_TOKEN_FILE", tokenDir+"/token"),
								env("CONFIG_FILE", configDir+"/config.yaml"),
								// Explicit, so run.sh does not have to derive it
								// by loading the config -- which fails, leaving it
								// empty, if the config ever references an unset
								// ${NAME}.
								env("RUNNER_STATE_FILE", "/data/.runner"),
								env("DOCKERD_ROOTLESS_ROOTLESSKIT_MTU", fmt.Sprint(mtu)),
								// DOCKER_HOST stays at the image default (the
								// rootless unix socket) so it can be mounted into
								// jobs. Do NOT set DOCKER_TLS_CERTDIR="": that
								// switches the always-on TCP listener from mTLS
								// on :2376 to plaintext on :2375.
							},
							VolumeMounts: corev1.VolumeMountArray{
								&corev1.VolumeMountArgs{Name: pulumi.String("runner-data"), MountPath: pulumi.String("/data")},
								&corev1.VolumeMountArgs{Name: pulumi.String("docker-data"), MountPath: pulumi.String(dockerDataMount)},
								&corev1.VolumeMountArgs{Name: pulumi.String("config"), MountPath: pulumi.String(configDir), ReadOnly: pulumi.Bool(true)},
								&corev1.VolumeMountArgs{Name: pulumi.String("docker-config"), MountPath: pulumi.String(dockerConfigDir), ReadOnly: pulumi.Bool(true)},
								&corev1.VolumeMountArgs{Name: pulumi.String("registration-token"), MountPath: pulumi.String(tokenDir), ReadOnly: pulumi.Bool(true)},
							},
							// Without a probe the container counts as ready the
							// moment it starts, so `pulumi up` reports success
							// for a runner whose dockerd is crash-looping. This
							// is what makes the StatefulSet await mean "the
							// daemon jobs need is up".
							ReadinessProbe: &corev1.ProbeArgs{
								Exec: &corev1.ExecActionArgs{
									Command: pulumi.ToStringArray([]string{"docker", "info", "--format", "{{.ServerVersion}}"}),
								},
								InitialDelaySeconds: pulumi.Int(5),
								PeriodSeconds:       pulumi.Int(20),
								TimeoutSeconds:      pulumi.Int(10),
								FailureThreshold:    pulumi.Int(3),
							},
							// Requests are what the scheduler places by, not
							// what a build gets: with no CPU limit, dockerd
							// uses whatever the base node has idle. Half a CPU
							// is what lets this pod, the pod runner and GKE's
							// own pods fit one e2-standard-2 (docs/runners.md).
							// There is deliberately no memory limit: no other
							// Docker daemon shares the node (anti-affinity
							// above), and a limit would only OOM-kill large
							// builds that the node could have carried.
							Resources: &corev1.ResourceRequirementsArgs{
								Requests: pulumi.StringMap{
									"cpu":               pulumi.String("500m"),
									"memory":            pulumi.String("3Gi"),
									"ephemeral-storage": pulumi.String("2Gi"),
								},
							},
						},
					},
				},
			},
		},
	}, opts...)
}

// podsStatefulSet is the pod runner: one small, unprivileged pod that polls
// Gitea and turns each job into a pod in JobsNamespace. A StatefulSet, like the
// DinD runner, so its PVC keeps .runner and the actions cache across restarts.
func podsStatefulSet(
	ctx *pulumi.Context,
	cfg *config.Config,
	ksa *corev1.ServiceAccount,
	cm *corev1.ConfigMap,
	opts []pulumi.ResourceOption,
) (*appsv1.StatefulSet, error) {
	labels := pulumi.StringMap{"app": pulumi.String(podsAppLabel)}

	// gitea:runnerJobCapacity 0 removes the pod runner, and with it its disk.
	replicas := 0
	if cfg.RunnerJobCapacity > 0 {
		replicas = 1
	}

	return appsv1.NewStatefulSet(ctx, "gitea-runner-pods", &appsv1.StatefulSetArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(PodsKSAName),
			Namespace: pulumi.String(Namespace),
			Labels:    labels,
		},
		Spec: &appsv1.StatefulSetSpecArgs{
			ServiceName:                          pulumi.String(PodsKSAName),
			Replicas:                             pulumi.Int(replicas),
			Selector:                             &metav1.LabelSelectorArgs{MatchLabels: labels},
			PersistentVolumeClaimRetentionPolicy: deletePVCs(),
			VolumeClaimTemplates: corev1.PersistentVolumeClaimTypeArray{
				// .runner, the actions cache, and cloned actions ($HOME).
				pvc("runner-data", "10Gi"),
			},

			Template: &corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Labels: labels,
					Annotations: pulumi.StringMap{
						// Every running ubuntu-* job dies with this pod. The base
						// pool is not autoscaled, so nothing asks today; this
						// keeps it that way if the pool ever is.
						"cluster-autoscaler.kubernetes.io/safe-to-evict": pulumi.String("false"),
					},
				},
				Spec: &corev1.PodSpecArgs{
					ServiceAccountName: ksa.Metadata.Name(),
					// The one pod here that needs an API token: it creates,
					// execs into and deletes job pods. The Role confines it to
					// JobsNamespace.
					AutomountServiceAccountToken: pulumi.Bool(true),
					EnableServiceLinks:           pulumi.Bool(false),
					// Above runner.shutdown_timeout (podsConfig).
					TerminationGracePeriodSeconds: pulumi.Int(3600),

					// Required for the Secret Manager add-on on Standard
					// clusters. Every node in a GKE_METADATA pool carries it.
					// No toleration: like the DinD runner, this pod can only
					// land on the base pool.
					NodeSelector: pulumi.StringMap{"iam.gke.io/gke-metadata-server-enabled": pulumi.String("true")},

					// The image is built to run as root and needs none of it.
					// fsGroup makes the PVC and the token files readable.
					SecurityContext: &corev1.PodSecurityContextArgs{
						RunAsUser:           pulumi.Int(1000),
						RunAsGroup:          pulumi.Int(1000),
						RunAsNonRoot:        pulumi.Bool(true),
						FsGroup:             pulumi.Int(1000),
						FsGroupChangePolicy: pulumi.String("OnRootMismatch"),
						SeccompProfile:      &corev1.SeccompProfileArgs{Type: pulumi.String("RuntimeDefault")},
					},

					Volumes: corev1.VolumeArray{
						&corev1.VolumeArgs{
							Name:      pulumi.String("config"),
							ConfigMap: &corev1.ConfigMapVolumeSourceArgs{Name: cm.Metadata.Name()},
						},
						&corev1.VolumeArgs{
							Name: pulumi.String("registration-token"),
							Csi: &corev1.CSIVolumeSourceArgs{
								Driver:           pulumi.String("secrets-store-gke.csi.k8s.io"),
								ReadOnly:         pulumi.Bool(true),
								VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(spcName)},
							},
						},
					},

					Containers: corev1.ContainerArray{
						&corev1.ContainerArgs{
							Name:  pulumi.String("runner"),
							Image: pulumi.String(cfg.RunnerPodsImage()),
							SecurityContext: &corev1.SecurityContextArgs{
								AllowPrivilegeEscalation: pulumi.Bool(false),
								Capabilities:             &corev1.CapabilitiesArgs{Drop: pulumi.ToStringArray([]string{"ALL"})},
							},
							Env: corev1.EnvVarArray{
								env("GITEA_INSTANCE_URL", fmt.Sprintf("https://%s", cfg.Domain)),
								env("GITEA_RUNNER_REGISTRATION_TOKEN_FILE", tokenDir+"/token"),
								env("CONFIG_FILE", configDir+"/config.yaml"),
								env("RUNNER_STATE_FILE", "/data/.runner"),
								// uid 1000 has no home directory in the image. git
								// and the action cache want one; the PVC is it.
								env("HOME", "/data"),
							},
							Ports: corev1.ContainerPortArray{
								&corev1.ContainerPortArgs{Name: pulumi.String("cache"), ContainerPort: pulumi.Int(cachePort)},
								&corev1.ContainerPortArgs{Name: pulumi.String("metrics"), ContainerPort: pulumi.Int(metricsPort)},
							},
							VolumeMounts: corev1.VolumeMountArray{
								&corev1.VolumeMountArgs{Name: pulumi.String("runner-data"), MountPath: pulumi.String("/data")},
								&corev1.VolumeMountArgs{Name: pulumi.String("config"), MountPath: pulumi.String(configDir), ReadOnly: pulumi.Bool(true)},
								&corev1.VolumeMountArgs{Name: pulumi.String("registration-token"), MountPath: pulumi.String(tokenDir), ReadOnly: pulumi.Bool(true)},
							},
							// Ready once the daemon serves /healthz, so the
							// StatefulSet await means the runner is up.
							ReadinessProbe: &corev1.ProbeArgs{
								HttpGet: &corev1.HTTPGetActionArgs{
									Path: pulumi.String("/healthz"),
									Port: pulumi.Int(metricsPort),
								},
								InitialDelaySeconds: pulumi.Int(5),
								PeriodSeconds:       pulumi.Int(20),
								TimeoutSeconds:      pulumi.Int(5),
								FailureThreshold:    pulumi.Int(3),
							},
							// It polls, streams logs and serves the cache; the
							// jobs' own work happens in their pods. Idle it
							// uses a few millicores. Kept small so the base
							// node fits (docs/runners.md).
							Resources: &corev1.ResourceRequirementsArgs{
								Requests: pulumi.StringMap{
									"cpu":    pulumi.String("200m"),
									"memory": pulumi.String("512Mi"),
								},
							},
						},
					},
				},
			},
		},
	}, opts...)
}

func pvc(name, size string) *corev1.PersistentVolumeClaimTypeArgs {
	return &corev1.PersistentVolumeClaimTypeArgs{
		Metadata: &metav1.ObjectMetaArgs{Name: pulumi.String(name)},
		Spec: &corev1.PersistentVolumeClaimSpecArgs{
			AccessModes:      pulumi.ToStringArray([]string{"ReadWriteOnce"}),
			StorageClassName: pulumi.String("standard-rwo"),
			Resources: &corev1.VolumeResourceRequirementsArgs{
				Requests: pulumi.StringMap{"storage": pulumi.String(size)},
			},
		},
	}
}

// deletePVCs: scaling down deletes the disks too. Leaving them would orphan PDs
// that keep billing after the pods are gone -- and a re-created pod would come
// back with a .runner Gitea may already have pruned.
func deletePVCs() *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicyArgs {
	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicyArgs{
		WhenDeleted: pulumi.String("Delete"),
		WhenScaled:  pulumi.String("Delete"),
	}
}

// with returns base plus extra in a fresh slice. A bare append would share
// base's backing array between callers.
func with(base []pulumi.ResourceOption, extra ...pulumi.ResourceOption) []pulumi.ResourceOption {
	out := make([]pulumi.ResourceOption, 0, len(base)+len(extra))
	return append(append(out, base...), extra...)
}

func env(name, value string) *corev1.EnvVarArgs {
	return &corev1.EnvVarArgs{Name: pulumi.String(name), Value: pulumi.String(value)}
}
