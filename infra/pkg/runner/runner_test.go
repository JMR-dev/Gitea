package runner

import (
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
