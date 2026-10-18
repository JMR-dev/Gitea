package runner

import (
	"encoding/json"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// tunnelAgents is how many konnectivity-agent pods the cluster runs, whatever
// its size. Two, so one agent restarting does not cut every stream at once.
const tunnelAgents = 2

// pinTunnelAgents fixes the number of konnectivity-agent pods. Every step of a
// pod-runner job streams through one of them (package gke), and GKE's default
// ladder runs one per node up to six. Each burst or heavy node joining or
// leaving therefore created or deleted an agent, and deleting one killed every
// job streaming through it, wherever that job ran.
//
// The ladder is GKE's to create but ours to edit: the addon manager owns the
// ConfigMap in EnsureExists mode, which creates it if missing and never
// reconciles it. The cluster-proportional-autoscaler rereads it on every
// poll. patchForce takes the field from the addon manager, which set it
// with a plain update. RetainOnDelete: deleting the patch would strip the
// field, and an autoscaler with no ladder at all is worse than GKE's.
func pinTunnelAgents(ctx *pulumi.Context, k8sOpts []pulumi.ResourceOption) error {
	ladder, err := json.Marshal(map[string]interface{}{
		"coresToReplicas": []interface{}{},
		"nodesToReplicas": [][2]int{{1, tunnelAgents}},
	})
	if err != nil {
		return err
	}
	_, err = corev1.NewConfigMapPatch(ctx, "konnectivity-agent-autoscaler-config", &corev1.ConfigMapPatchArgs{
		Metadata: &metav1.ObjectMetaPatchArgs{
			Name:      pulumi.String("konnectivity-agent-autoscaler-config"),
			Namespace: pulumi.String("kube-system"),
			Annotations: pulumi.StringMap{
				"pulumi.com/patchForce": pulumi.String("true"),
			},
		},
		Data: pulumi.StringMap{"ladder": pulumi.String(string(ladder))},
	}, with(k8sOpts, pulumi.RetainOnDelete(true))...)
	return err
}
