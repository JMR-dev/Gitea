package runner

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gitea-infra/pkg/config"
)

// Job output staying out of the runners' own log, and so out of the sink to
// Watchtower (package telemetry), was measured at log.level info only. A more
// verbose level needs that measured again first (docs/runners.md).
func TestRunnerLogLevelIsInfo(t *testing.T) {
	cfg := &config.Config{
		RunnerCapacity: 1, RunnerJobCapacity: 1, RunnerArmMaxNodes: 1, RunnerHeavyMaxNodes: 1,
		RunnerJobCPU: "400m", RunnerJobMemory: "1536Mi", RunnerHeavyCPU: "2", RunnerHeavyMemory: "12Gi",
	}
	for name, yaml := range map[string]string{
		"dind": dindConfig(cfg),
		"pods": podsConfig(cfg),
		"arm":  armConfig(cfg),
	} {
		if !strings.Contains(yaml, "\nlog:\n  level: info\n") {
			t.Errorf("%s: log.level is not info:\n%s", name, yaml)
		}
		if n := strings.Count(yaml, "level:"); n != 1 {
			t.Errorf("%s: %d level keys, want the one", name, n)
		}
	}
}

// The DinD runner's prune loop: its scripts reach the pod through the
// ConfigMap, start.sh hands over to the image's entrypoint, and pruneEnv sets
// what image-prune.sh needs to the values the rest of this package uses.
func TestDinDImagePrune(t *testing.T) {
	cfg := &config.Config{RunnerCapacity: 1}
	data := dindConfigMapData(cfg, "{}")
	env := pruneEnv()

	for _, c := range []struct {
		want string
		ok   bool
	}{
		{"the ConfigMap carries start.sh", startScript != "" && data["start.sh"] == startScript},
		{"the ConfigMap carries image-prune.sh", pruneScript != "" && data["image-prune.sh"] == pruneScript},
		{"start.sh runs image-prune.sh from configDir", strings.Contains(startScript, `prune_script="${START_PRUNE_SCRIPT:-`+configDir+`/image-prune.sh}"`+"\n")},
		{"start.sh ends by exec'ing s6-svscan", strings.HasSuffix(strings.TrimSpace(startScript), `exec s6-svscan "$scan_dir"`)},
		{"start.sh writes no finish script (it would let a prune failure stop the runner)", !strings.Contains(startScript, "/finish")},
		{"the line is the card's 4 GB, as 4 GiB", env["IMAGE_PRUNE_MIN_FREE_KIB"] == "4194304"},
		{"the kept image is the docker label's", strings.Contains(dindConfig(cfg), `- "docker:docker://`+env["IMAGE_PRUNE_KEEP"]+`"`)},
		{"free space is measured on dockerd's data root", env["IMAGE_PRUNE_DATA_ROOT"] == dockerDataRoot},
		{"the state is on the runner-data PVC (/data)", strings.HasPrefix(env["IMAGE_PRUNE_STATE"], "/data/")},
	} {
		if !c.ok {
			t.Errorf("want: %s", c.want)
		}
	}

	// The script's required settings (${NAME:?}) are exactly the ones Go sets.
	var required []string
	for _, m := range regexp.MustCompile(`\$\{(IMAGE_PRUNE_[A-Z_]+):\?\}`).FindAllStringSubmatch(pruneScript, -1) {
		required = append(required, m[1])
	}
	slices.Sort(required)
	if set := slices.Sorted(maps.Keys(env)); !slices.Equal(required, set) {
		t.Errorf("image-prune.sh requires %v, pruneEnv sets %v", required, set)
	}
}
