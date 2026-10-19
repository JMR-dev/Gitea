// Package ops is where the release engine (scripts/ops/gitea_ops.py) runs:
// a Kubernetes Job on the runner cluster's ops pool, under its own identity.
//
// Why a Job and not the operator's laptop: the engine's automatic rollback is
// code in the same process as the release. If that process dies mid-release
// -- a laptop sleeps, a terminal closes -- nothing rolls back, and Gitea can be
// left stopped with its timers paused. A Job keeps running when the launcher
// disconnects, and the ops pool is on-demand and protected from eviction.
//
// Why not a Gitea Actions job: the release stops Gitea, and an Actions job
// reports to Gitea. It must not depend on the thing it is restarting.
//
// Threat model: this identity can stop Gitea, move :prod and restore its disk,
// so it never shares a node with CI. Its pods run only on the tainted ops pool
// (package gke), so a CI pod that escapes to its node cannot reach them -- the
// node authorizer limits a kubelet to its own pods' tokens. No runner KSA has
// any access to this namespace. Job pods get no Kubernetes API token at all.
package ops

import (
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	policyv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/policy/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/gke"
)

const (
	// Namespace holds release-engine Jobs and nothing else.
	Namespace = "gitea-ops"
	// KSAName is the Kubernetes service account the Jobs run as. It is spelled
	// out in the Workload Identity binding, so renaming it moves the grant.
	KSAName = "gitea-release"
	// AppLabel selects engine pods, for the PodDisruptionBudget.
	AppLabel = "gitea-ops"
)

type Ops struct {
	ServiceAccount *serviceaccount.Account
}

// New returns nil (without error) when the runner cluster was not created.
func New(
	ctx *pulumi.Context,
	cfg *config.Config,
	gk *gke.Cluster,
	k8s *kubernetes.Provider,
	vmSA *serviceaccount.Account,
	repo *artifactregistry.Repository,
	backupBucket *storage.Bucket,
	deps []pulumi.Resource,
) (*Ops, error) {
	if gk == nil || k8s == nil {
		return nil, nil
	}
	opts := pulumi.DependsOn(deps)

	sa, err := serviceaccount.NewAccount(ctx, "gitea-release-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String("gitea-release"),
		DisplayName: pulumi.String("Gitea release engine"),
		Description: pulumi.String("Runs scripts/ops/gitea_ops.py as a Job on the ops pool"),
	}, opts)
	if err != nil {
		return nil, err
	}
	member := pulumi.Sprintf("serviceAccount:%s", sa.Email)

	if err := grant(ctx, cfg, member, vmSA, repo, backupBucket, opts); err != nil {
		return nil, err
	}

	// Workload Identity: the KSA below becomes this account. A Google service
	// account rather than a direct principal grant, because OS Login -- which
	// is how the engine reaches the VM -- needs an account with a login profile.
	if _, err := serviceaccount.NewIAMMember(ctx, "gitea-release-wi", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: sa.Name,
		Role:             pulumi.String("roles/iam.workloadIdentityUser"),
		Member: pulumi.Sprintf("serviceAccount:%s.svc.id.goog[%s/%s]",
			cfg.Project, Namespace, KSAName),
	}, opts); err != nil {
		return nil, err
	}

	k8sOpts := []pulumi.ResourceOption{opts, pulumi.Provider(k8s), pulumi.DeletedWith(gk.Cluster)}
	ns, err := corev1.NewNamespace(ctx, "gitea-ops", &corev1.NamespaceArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name: pulumi.String(Namespace),
			Labels: pulumi.StringMap{
				// The gcloud image runs as root; baseline still refuses
				// privileged pods and host namespaces.
				"pod-security.kubernetes.io/enforce": pulumi.String("baseline"),
			},
		},
	}, k8sOpts...)
	if err != nil {
		return nil, err
	}
	inNS := append(append([]pulumi.ResourceOption{}, k8sOpts...), pulumi.DependsOn([]pulumi.Resource{ns}))

	if _, err := corev1.NewServiceAccount(ctx, "gitea-release", &corev1.ServiceAccountArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String(KSAName),
			Namespace: pulumi.String(Namespace),
			Annotations: pulumi.StringMap{
				"iam.gke.io/gcp-service-account": sa.Email,
			},
		},
		// The engine talks to Google APIs and the VM, never to Kubernetes.
		AutomountServiceAccountToken: pulumi.Bool(false),
	}, inNS...); err != nil {
		return nil, err
	}

	// Nothing has any business connecting to an engine pod. Egress stays
	// open: it needs Google APIs, the IAP tunnel, Gitea itself, and the GKE
	// metadata server for its Workload Identity token.
	if _, err := networkingv1.NewNetworkPolicy(ctx, "gitea-ops", &networkingv1.NetworkPolicyArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("deny-ingress"),
			Namespace: pulumi.String(Namespace),
		},
		Spec: &networkingv1.NetworkPolicySpecArgs{
			PodSelector: &metav1.LabelSelectorArgs{},
			PolicyTypes: pulumi.ToStringArray([]string{"Ingress"}),
		},
	}, inNS...); err != nil {
		return nil, err
	}

	// A release must not be evicted halfway: maxUnavailable 0 blocks the
	// autoscaler's scale-down and a node upgrade's drain while one runs (an
	// upgrade waits up to an hour before forcing it, longer than a release).
	if _, err := policyv1.NewPodDisruptionBudget(ctx, "gitea-ops", &policyv1.PodDisruptionBudgetArgs{
		Metadata: &metav1.ObjectMetaArgs{
			Name:      pulumi.String("gitea-ops"),
			Namespace: pulumi.String(Namespace),
		},
		Spec: &policyv1.PodDisruptionBudgetSpecArgs{
			MaxUnavailable: pulumi.Int(0),
			Selector: &metav1.LabelSelectorArgs{
				MatchLabels: pulumi.StringMap{"app": pulumi.String(AppLabel)},
			},
		},
	}, inNS...); err != nil {
		return nil, err
	}

	return &Ops{ServiceAccount: sa}, nil
}

// grant gives the engine exactly what gitea_ops.py does, resource by resource
// where the API allows it. Deletes are fenced by name: the engine deletes only
// the restore disks and the backup/pre-release snapshots it creates itself.
func grant(
	ctx *pulumi.Context,
	cfg *config.Config,
	member pulumi.StringInput,
	vmSA *serviceaccount.Account,
	repo *artifactregistry.Repository,
	backupBucket *storage.Bucket,
	opts pulumi.ResourceOption,
) error {
	project := pulumi.String(cfg.Project)

	for _, role := range []string{
		// Reaching the VM without a public SSH port.
		"roles/iap.tunnelResourceAccessor",
		// An ephemeral SSH key via OS Login; the steps run under sudo.
		"roles/compute.osAdminLogin",
		"roles/logging.logWriter",
		// Secret version metadata for the manifest -- never the values.
		"roles/secretmanager.viewer",
	} {
		if _, err := projects.NewIAMMember(ctx, "release-"+role, &projects.IAMMemberArgs{
			Project: project, Role: pulumi.String(role), Member: member,
		}, opts); err != nil {
			return err
		}
	}

	release, err := projects.NewIAMCustomRole(ctx, "gitea-release", &projects.IAMCustomRoleArgs{
		Project:     project,
		RoleId:      pulumi.String("giteaRelease"),
		Title:       pulumi.String("Gitea release engine"),
		Description: pulumi.String("Snapshots, restore disks and exports for scripts/ops/gitea_ops.py"),
		Permissions: pulumi.ToStringArray([]string{
			"compute.disks.get",
			"compute.disks.create",
			"compute.disks.createSnapshot",
			"compute.disks.use",
			"compute.snapshots.create",
			"compute.snapshots.get",
			"compute.snapshots.list",
			"compute.snapshots.setLabels",
			"compute.snapshots.useReadOnly",
			"compute.instances.get",
			"compute.instances.attachDisk",
			"compute.instances.detachDisk",
			"compute.zoneOperations.get",
			"compute.globalOperations.get",
			"resourcemanager.projects.getIamPolicy",
		}),
	}, opts)
	if err != nil {
		return err
	}
	if _, err := projects.NewIAMMember(ctx, "release-custom", &projects.IAMMemberArgs{
		Project: project, Role: release.Name, Member: member,
	}, opts); err != nil {
		return err
	}

	// Deletes, each in its own role so a condition can fence it by name.
	deleter := func(name, roleID, perm, title string, expr pulumi.StringInput) error {
		role, err := projects.NewIAMCustomRole(ctx, name, &projects.IAMCustomRoleArgs{
			Project: project, RoleId: pulumi.String(roleID), Title: pulumi.String(title),
			Permissions: pulumi.ToStringArray([]string{perm}),
		}, opts)
		if err != nil {
			return err
		}
		_, err = projects.NewIAMMember(ctx, name, &projects.IAMMemberArgs{
			Project: project, Role: role.Name, Member: member,
			Condition: &projects.IAMMemberConditionArgs{Title: pulumi.String(title), Expression: expr},
		}, opts)
		return err
	}
	if err := deleter("release-delete-restore-disks", "giteaReleaseRestoreDiskDeleter",
		"compute.disks.delete", "Only restore disks",
		pulumi.Sprintf(`resource.name.startsWith("projects/%s/zones/%s/disks/gitea-data-restore-")`,
			cfg.Project, cfg.Zone)); err != nil {
		return err
	}
	// The daily scheduled snapshots (gitea-data-us-east1-...) stay out of reach.
	if err := deleter("release-delete-snapshots", "giteaReleaseSnapshotDeleter",
		"compute.snapshots.delete", "Only engine snapshots",
		pulumi.Sprintf(`resource.name.startsWith("projects/%s/global/snapshots/gitea-data-backup-") || `+
			`resource.name.startsWith("projects/%s/global/snapshots/gitea-data-pre-release-")`,
			cfg.Project, cfg.Project)); err != nil {
		return err
	}

	// OS Login on an instance that runs as gitea-vm@ requires acting as it.
	if _, err := serviceaccount.NewIAMMember(ctx, "release-act-as-vm", &serviceaccount.IAMMemberArgs{
		ServiceAccountId: vmSA.Name,
		Role:             pulumi.String("roles/iam.serviceAccountUser"),
		Member:           member,
	}, opts); err != nil {
		return err
	}

	// :prod, :previous and :last-good are tag moves; labels are read through
	// the registry API.
	if _, err := artifactregistry.NewRepositoryIamMember(ctx, "release-ar-writer", &artifactregistry.RepositoryIamMemberArgs{
		Project: project, Location: repo.Location, Repository: repo.Name,
		Role: pulumi.String("roles/artifactregistry.writer"), Member: member,
	}, opts); err != nil {
		return err
	}

	// Manifests and exports in, dumps listed and read; never deleted.
	for name, role := range map[string]string{
		"release-backup-viewer":  "roles/storage.objectViewer",
		"release-backup-creator": "roles/storage.objectCreator",
	} {
		if _, err := storage.NewBucketIAMMember(ctx, name, &storage.BucketIAMMemberArgs{
			Bucket: backupBucket.Name, Role: pulumi.String(role), Member: member,
		}, opts); err != nil {
			return err
		}
	}
	return nil
}
