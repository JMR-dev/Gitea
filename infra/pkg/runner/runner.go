// Package runner deploys the Gitea Actions runner into the GKE cluster from
// package gke: a StatefulSet of the official rootless Docker-in-Docker runner
// image, the NetworkPolicy that keeps jobs away from the metadata server, and
// the plumbing that gets the registration token into the pod without Pulumi
// ever handling it.
//
// Threat model, in one paragraph: jobs run inside the runner pod's own dockerd,
// and they get its socket (that is what makes `docker build` work). A job can
// therefore read anything that daemon can see -- the runner's /data/.runner
// credentials and the registration token included. That is accepted: this is a
// single-tenant instance with registration disabled, so every workflow author is
// already trusted. What is NOT accepted is a job reaching Google Cloud. The
// node SA holds nothing useful (package gke), the runner's own Kubernetes
// service account holds only read access to that one token, and the
// NetworkPolicy below stops jobs from asking the metadata server for even that.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/gke"
	"gitea-infra/pkg/secrets"
)

const (
	Namespace = "gitea-runner"
	// KSAName is the Kubernetes service account the runner pods run as. It is
	// spelled out in the Workload Identity principal, so renaming it means the
	// IAM grant moves with it.
	KSAName = "gitea-runner"

	appLabel  = "gitea-runner"
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

	// Fixed rather than random, so the NetworkPolicy can name it. Job
	// containers reach the runner's cache server on the pod IP at this port.
	cachePort = 8088

	// GCE's default VPC MTU. Docker defaults to 1500 inside, and the mismatch
	// shows up as TLS handshakes that hang on large certificates.
	mtu = 1460
)

type Runner struct {
	Provider    *kubernetes.Provider
	StatefulSet *appsv1.StatefulSet
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

	grant, err := grantToken(ctx, cfg, gk, opts)
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
			"config.yaml": pulumi.String(runnerConfig(cfg)),
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

	return &Runner{Provider: k8s, StatefulSet: sts}, nil
}

// provider builds a Kubernetes provider for the cluster from a bearer token,
// because the pulumi/pulumi-go image Cloud Build runs `pulumi up` in has no
// gcloud and no gke-gcloud-auth-plugin to exec.
func provider(ctx *pulumi.Context, gk *gke.Cluster) (*kubernetes.Provider, error) {
	// The output form, with a dependency on the cluster, so the token is
	// fetched after the cluster exists -- a token fetched at program start can
	// be within minutes of expiry by the time a ten-minute cluster create
	// finishes. Deliberately NOT on the node pool: a pool replacement would make
	// the kubeconfig unknown at preview, which replaces the provider and with it
	// every object in the cluster.
	token := organizations.GetClientConfigOutput(ctx, pulumi.DependsOn([]pulumi.Resource{gk.Cluster})).AccessToken()
	endpoint := gk.Cluster.ControlPlaneEndpointsConfig.DnsEndpointConfig().Endpoint()

	kubeconfig := pulumi.All(endpoint, token).ApplyT(func(args []interface{}) (string, error) {
		ep, _ := args[0].(*string)
		if ep == nil || *ep == "" {
			return "", errors.New("cluster has no DNS endpoint -- check controlPlaneEndpointsConfig")
		}
		tok, _ := args[1].(string)
		return kubeconfigJSON(*ep, tok)
	}).(pulumi.StringOutput)

	return kubernetes.NewProvider(ctx, "gitea-runners", &kubernetes.ProviderArgs{
		// ToSecret: the provider schema does not mark kubeconfig as secret, so
		// without this the token would sit in state in plain text.
		Kubeconfig: pulumi.ToSecret(kubeconfig).(pulumi.StringOutput),
		// DeleteUnreachable is deliberately left unset. With it on, a refresh
		// against a stale token in state reads as "cluster unreachable" and
		// silently drops every object from state; the next `up` then fails
		// with "already exists". DeletedWith covers the legitimate case.
	})
}

// kubeconfigJSON renders a kubeconfig. JSON is valid YAML, so no YAML library is
// needed. There is no certificate-authority-data: the DNS endpoint presents a
// certificate from a public CA.
func kubeconfigJSON(endpoint, token string) (string, error) {
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
			"user": map[string]interface{}{"token": token},
		}},
	}
	b, err := json.Marshal(kc)
	return string(b), err
}

// grantToken lets the runner's KSA -- and only it -- read the registration
// token, through a Workload Identity Federation principal. No Google service
// account is involved, so there is nothing to impersonate.
func grantToken(ctx *pulumi.Context, cfg *config.Config, gk *gke.Cluster, opts pulumi.ResourceOption) (*secretmanager.SecretIamMember, error) {
	proj, err := organizations.LookupProject(ctx, &organizations.LookupProjectArgs{
		ProjectId: pulumi.StringRef(cfg.Project),
	})
	if err != nil {
		return nil, err
	}
	return secretmanager.NewSecretIamMember(ctx, "runner-read-registration-token", &secretmanager.SecretIamMemberArgs{
		Project:  pulumi.String(cfg.Project),
		SecretId: pulumi.String(secrets.RunnerTokenName),
		Role:     pulumi.String("roles/secretmanager.secretAccessor"),
		Member: pulumi.String(fmt.Sprintf(
			"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
			proj.Number, cfg.Project, Namespace, KSAName)),
		// The <project>.svc.id.goog pool does not exist until the first
		// Workload Identity cluster does, and IAM rejects principals in a pool
		// it cannot find.
	}, opts, pulumi.DependsOn([]pulumi.Resource{gk.Cluster}))
}

// runnerConfig is the runner's config.yaml. Keys are from the v5.1.0
// config.example.yaml; anything not set here keeps its default.
func runnerConfig(cfg *config.Config) string {
	// Spot nodes get ~15s of notice before preemption, so there is no point in
	// waiting for jobs to finish: they will not. On on-demand nodes, a drain
	// (node upgrade, scale-down) waits up to an hour, so let running jobs use it.
	shutdown := "55m"
	if cfg.RunnerSpot {
		shutdown = "20s"
	}

	// docker.io paths rather than docker.gitea.com: identical digests, but only
	// Docker Hub can be served through the registry mirror in daemon.json.
	// Every job behind one NAT IP would otherwise share Docker Hub's anonymous
	// per-IP pull limit.
	labels := []string{
		"ubuntu-latest:docker://docker.io/gitea/runner-images:ubuntu-latest",
		"ubuntu-24.04:docker://docker.io/gitea/runner-images:ubuntu-24.04",
		"ubuntu-22.04:docker://docker.io/gitea/runner-images:ubuntu-22.04",
	}
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

container:
  # Empty: a fresh network per job.
  network: ""
  privileged: false
  # Empty allows none. Jobs that need host paths are a design smell here.
  valid_volumes: []
  # Empty: auto-detect DOCKER_HOST (the rootless unix socket) AND mount it into
  # job containers, which is what lets a job run `+"`docker build`"+`. "-" would
  # auto-detect without mounting.
  docker_host: ""
`, cfg.RunnerCapacity, shutdown, strings.TrimRight(lb.String(), "\n"), cachePort)
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

// networkPolicy is load-bearing, not defence in depth.
//
// Job containers run inside the runner pod's dockerd, so their traffic leaves
// from the runner pod's network namespace. The GKE metadata server identifies
// callers by pod IP -- it would hand any job the runner KSA's federated token,
// which can read the registration secret. Excluding 169.254.0.0/16 covers both
// addresses Dataplane V2 uses for it (.254 on port 80, .252 on port 988).
//
// The RFC1918 exclusions cover the node, pod and service ranges, the control
// plane's private address, and the VM's internal IP. Jobs reach Gitea the same
// way the rest of the internet does: its public address, through Caddy.
//
// Ingress is denied outright. That also covers the rootless image's Docker API,
// which listens on the pod IP at :2376 (mTLS) and cannot be switched off.
//
// The Secret Manager CSI mount is unaffected: the add-on fetches the token from
// its own node DaemonSet, not from inside this pod.
func networkPolicy(ctx *pulumi.Context, opts []pulumi.ResourceOption) (*networkingv1.NetworkPolicy, error) {
	runnerPods := &metav1.LabelSelectorArgs{MatchLabels: pulumi.StringMap{"app": pulumi.String(appLabel)}}
	cache := networkingv1.NetworkPolicyPortArray{
		&networkingv1.NetworkPolicyPortArgs{Protocol: pulumi.String("TCP"), Port: pulumi.Int(cachePort)},
	}

	return networkingv1.NewNetworkPolicy(ctx, "gitea-runner", &networkingv1.NetworkPolicyArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("gitea-runner"),
			Namespace: pulumi.String(Namespace),
		},
		Spec: &networkingv1.NetworkPolicySpecArgs{
			// Every pod in the namespace, not just the runner: nothing else
			// should be here, and anything that is gets the same fence.
			PodSelector: &metav1.LabelSelectorArgs{},
			PolicyTypes: pulumi.ToStringArray([]string{"Ingress", "Egress"}),

			Ingress: networkingv1.NetworkPolicyIngressRuleArray{
				// The runner's cache server, for jobs. Their traffic most likely
				// never leaves the pod's own namespace (and so is never
				// evaluated), but if the dataplane does see it, this is the one
				// thing that should be let through.
				&networkingv1.NetworkPolicyIngressRuleArgs{
					From:  networkingv1.NetworkPolicyPeerArray{&networkingv1.NetworkPolicyPeerArgs{PodSelector: runnerPods}},
					Ports: cache,
				},
			},

			Egress: networkingv1.NetworkPolicyEgressRuleArray{
				// Cluster DNS. Without it, the RFC1918 exclusion below would
				// break name resolution for the runner and every job.
				//
				// Both labels: GKE enables NodeLocal DNSCache by default on new
				// clusters, and then queries to the kube-dns Service IP are
				// answered by the node-local-dns pod on the same node, not by
				// kube-dns. Allowing kube-dns alone resolves nothing.
				&networkingv1.NetworkPolicyEgressRuleArgs{
					To: networkingv1.NetworkPolicyPeerArray{
						&networkingv1.NetworkPolicyPeerArgs{
							NamespaceSelector: &metav1.LabelSelectorArgs{
								MatchLabels: pulumi.StringMap{"kubernetes.io/metadata.name": pulumi.String("kube-system")},
							},
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
				},
				&networkingv1.NetworkPolicyEgressRuleArgs{
					To:    networkingv1.NetworkPolicyPeerArray{&networkingv1.NetworkPolicyPeerArgs{PodSelector: runnerPods}},
					Ports: cache,
				},
				// The internet, minus everything private and link-local.
				&networkingv1.NetworkPolicyEgressRuleArgs{
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
				},
			},
		},
	}, opts...)
}

func statefulSet(
	ctx *pulumi.Context,
	cfg *config.Config,
	ksa *corev1.ServiceAccount,
	cm *corev1.ConfigMap,
	opts []pulumi.ResourceOption,
) (*appsv1.StatefulSet, error) {
	labels := pulumi.StringMap{"app": pulumi.String(appLabel)}

	// Spot preemption gives pods ~15s regardless of what is asked for, so a
	// long grace period only slows rollouts. On-demand drains honour up to an
	// hour; it must stay above runner.shutdown_timeout.
	grace := 3600
	if cfg.RunnerSpot {
		grace = 25
	}

	pvc := func(name, size string) *corev1.PersistentVolumeClaimTypeArgs {
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
			// Scaling down deletes the disks too. Leaving them would orphan PDs
			// that keep billing after the pods are gone -- and a re-created
			// pod would come back with a .runner Gitea may already have pruned.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicyArgs{
				WhenDeleted: pulumi.String("Delete"),
				WhenScaled:  pulumi.String("Delete"),
			},
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
					// One runner per node, by construction rather than by
					// arithmetic on resource requests. Each pod's jobs then have
					// the whole node, gitea:runnerReplicas maps directly to
					// nodes, and changing the machine type cannot silently
					// stack two Docker daemons on one box.
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
							// The CPU request is what the scheduler and the
							// autoscaler reason about. There is deliberately no
							// memory limit: the pod has its node to itself
							// (anti-affinity above), and a limit would only
							// OOM-kill large builds that the node could have
							// carried.
							Resources: &corev1.ResourceRequirementsArgs{
								Requests: pulumi.StringMap{
									"cpu":               pulumi.String("1"),
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

// with returns base plus extra in a fresh slice. A bare append would share
// base's backing array between callers.
func with(base []pulumi.ResourceOption, extra ...pulumi.ResourceOption) []pulumi.ResourceOption {
	out := make([]pulumi.ResourceOption, 0, len(base)+len(extra))
	return append(append(out, base...), extra...)
}

func env(name, value string) *corev1.EnvVarArgs {
	return &corev1.EnvVarArgs{Name: pulumi.String(name), Value: pulumi.String(value)}
}
