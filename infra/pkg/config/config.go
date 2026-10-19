// Package config turns loose Pulumi stack config into one typed struct, so the
// rest of the program never reaches back into the config bag and every key has
// exactly one spelling.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

type Config struct {
	Project string
	Region  string
	Zone    string

	Domain     string
	DNSZone    string
	AppName    string
	PodmanCIDR string

	MachineType string
	BootDiskGB  int
	DataDiskGB  int

	// RequireSigninView is Gitea's [service] REQUIRE_SIGNIN_VIEW: "false",
	// "true" (no anonymous access at all) or "expensive" (anonymous visitors
	// keep repo home pages, releases and profiles, but code, history, source
	// archives, issues, pull requests and wikis redirect to the login page).
	// Gitea refuses to start on any other value, so it is checked here.
	RequireSigninView string

	// WAFMode is the Coraza SecRuleEngine setting. It also decides whether the
	// fail2ban jail that acts on WAF verdicts is enabled at all.
	WAFMode string

	GitHubOwner          string
	GitHubRepo           string
	GitHubInstallationID int
	GitHubPATSecret      string

	InfraBuildServiceAccount string

	// RunnersEnabled gates the whole GKE runner slice: the cluster, its network,
	// and both runners. Off by default, because it depends on a secret that
	// only exists after scripts/bootstrap.sh has been re-run.
	RunnersEnabled    bool
	RunnerImageTag    string
	RunnerMachineType string

	// The base pool holds both runners and GKE's own pods: RunnerBaseNodes()
	// on-demand nodes, never autoscaled. RunnerBaseCommitment ("", "1y" or
	// "3y") buys a committed use discount covering ONE of them.
	RunnerBaseCommitment string

	// The burst pool holds nothing but ubuntu-* job pods: 0 to
	// RunnerBurstMaxNodes nodes, following the job queue. 0 removes it.
	RunnerBurstMaxNodes int
	RunnerBurstSpot     bool

	// The Docker-in-Docker runner, for `runs-on: docker` jobs: one pod per
	// node, each running up to RunnerCapacity jobs in its own dockerd.
	RunnerReplicas int
	RunnerCapacity int

	// The pod runner, for `runs-on: ubuntu-*` jobs: each job is its own pod,
	// so these jobs fan out over the node pool. RunnerJobCapacity is how many
	// run at once across the whole cluster (0 removes the pod runner); the
	// CPU and memory are each job pod's requests, which is what the node
	// autoscaler packs nodes by.
	RunnerJobCapacity int
	RunnerJobCPU      string
	RunnerJobMemory   string

	// The heavy pool, for `runs-on: heavy` jobs (Pulumi and other
	// memory-hungry builds): pod-runner jobs scheduled onto a node pool of
	// their own, which scales from zero to RunnerHeavyMaxNodes with the queue.
	// 0 removes the pool and the label. Each heavy job pod requests
	// RunnerHeavyCPU and RunnerHeavyMemory; the machine type must have room
	// for that after GKE's own reservations.
	RunnerHeavyMaxNodes    int
	RunnerHeavyMachineType string
	RunnerHeavyCPU         string
	RunnerHeavyMemory      string

	// OpsMachineType sizes the ops pool: the one on-demand node, scaled from
	// zero, that release-engine Jobs (scripts/ops) run on and nothing else
	// does. Part of the runner cluster, so it exists only with RunnersEnabled.
	OpsMachineType string
}

func Load(ctx *pulumi.Context) (*Config, error) {
	gcp := config.New(ctx, "gcp")
	c := config.New(ctx, "gitea")

	cfg := &Config{
		Project: gcp.Require("project"),
		Region:  gcp.Get("region"),

		Zone:       c.Get("zone"),
		Domain:     c.Require("domain"),
		DNSZone:    c.Require("dnsZone"),
		AppName:    c.Get("appName"),
		PodmanCIDR: c.Get("podmanSubnet"),

		MachineType: c.Get("machineType"),
		BootDiskGB:  c.GetInt("bootDiskGb"),
		DataDiskGB:  c.GetInt("dataDiskGb"),

		RequireSigninView: c.Get("requireSigninView"),
		WAFMode:           c.Get("wafMode"),

		GitHubOwner:          c.Get("githubOwner"),
		GitHubRepo:           c.Get("githubRepo"),
		GitHubInstallationID: c.GetInt("githubAppInstallationId"),
		GitHubPATSecret:      c.Get("githubPatSecret"),

		InfraBuildServiceAccount: c.Get("infraBuildServiceAccount"),

		RunnersEnabled:    c.GetBool("runnersEnabled"),
		RunnerImageTag:    c.Get("runnerImageTag"),
		RunnerCapacity:    c.GetInt("runnerCapacity"),
		RunnerMachineType: c.Get("runnerMachineType"),
		RunnerJobCPU:      c.Get("runnerJobCpu"),
		RunnerJobMemory:   c.Get("runnerJobMemory"),

		RunnerBaseCommitment: c.Get("runnerBaseCommitment"),
		RunnerBurstSpot:      c.GetBool("runnerBurstSpot"),

		RunnerHeavyMachineType: c.Get("runnerHeavyMachineType"),
		RunnerHeavyCPU:         c.Get("runnerHeavyCpu"),
		RunnerHeavyMemory:      c.Get("runnerHeavyMemory"),

		OpsMachineType: c.Get("opsMachineType"),
	}

	// Both meant something else before the node pool was split into base and
	// burst pools (the size of, and spot for, the one pool that held the
	// runners). Refused rather than ignored, so a stale stack config cannot
	// quietly keep the old behaviour in mind.
	for _, k := range []struct{ old, now string }{
		{"runnerMaxNodes", "runnerBurstMaxNodes (burst nodes only; the base pool is sized by runnerReplicas)"},
		{"runnerSpot", "runnerBurstSpot (the base pool is always on demand)"},
	} {
		if _, err := c.Try(k.old); err == nil {
			return nil, fmt.Errorf("gitea:%s is no longer read: use gitea:%s", k.old, k.now)
		}
	}

	// These have non-zero defaults where zero is ALSO a meaningful setting
	// (replicas=0 and jobCapacity=0 are the clean-teardown step; 0 burst or
	// heavy nodes removes that pool), so GetInt -- which cannot tell "unset"
	// from 0 -- would make the explicit value impossible to express.
	var err error
	if cfg.RunnerReplicas, err = c.TryInt("runnerReplicas"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerReplicas: %w", err)
		}
		cfg.RunnerReplicas = 1
	}
	if cfg.RunnerJobCapacity, err = c.TryInt("runnerJobCapacity"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerJobCapacity: %w", err)
		}
		cfg.RunnerJobCapacity = 6
	}
	if cfg.RunnerHeavyMaxNodes, err = c.TryInt("runnerHeavyMaxNodes"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerHeavyMaxNodes: %w", err)
		}
		cfg.RunnerHeavyMaxNodes = 2
	}
	if cfg.RunnerBurstMaxNodes, err = c.TryInt("runnerBurstMaxNodes"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerBurstMaxNodes: %w", err)
		}
		cfg.RunnerBurstMaxNodes = 2
	}

	applyDefaults(cfg)
	return cfg, validate(cfg)
}

func applyDefaults(c *Config) {
	if c.Region == "" {
		c.Region = "us-east1"
	}
	if c.Zone == "" {
		// "-b", not "-a": us-east1 has zones b, c and d and NO -a zone, so the
		// obvious default would generate an invalid zone in the very region this
		// stack targets. "-b" exists in every US region we would plausibly use.
		// Set gitea:zone explicitly if you care which one.
		c.Zone = c.Region + "-b"
	}
	if c.AppName == "" {
		c.AppName = "Gitea"
	}
	if c.PodmanCIDR == "" {
		c.PodmanCIDR = "10.89.10.0/24"
	}
	if c.MachineType == "" {
		c.MachineType = "e2-small"
	}
	if c.BootDiskGB == 0 {
		c.BootDiskGB = 20
	}
	if c.DataDiskGB == 0 {
		c.DataDiskGB = 30
	}
	if c.GitHubPATSecret == "" {
		c.GitHubPATSecret = "github-pat"
	}
	if c.RequireSigninView == "" {
		c.RequireSigninView = "false"
	}
	if c.WAFMode == "" {
		// Detect first, tune, then block. Starting a WAF in blocking mode in
		// front of an app that legitimately carries code and markdown in POST
		// bodies is how you get paged on day one.
		c.WAFMode = "DetectionOnly"
	}
	if c.RunnerImageTag == "" {
		// Pinned, not floating: the runner went 4.x -> 5.x in two weeks with
		// breaking config changes. Bump deliberately, like image/gitea.version.
		c.RunnerImageTag = "5.1.0-dind-rootless"
	}
	if c.RunnerCapacity == 0 {
		c.RunnerCapacity = 1
	}
	if c.RunnerMachineType == "" {
		// 2 vCPU / 8 GB, for both pools. A base node carries both runners
		// and all of GKE's own pods, which only just fits (docs/runners.md);
		// a burst node carries about three job pods.
		c.RunnerMachineType = "e2-standard-2"
	}
	if c.RunnerJobCPU == "" {
		// About three jobs on an otherwise empty e2-standard-2, after GKE's
		// own pods. A request, not a limit: an idle node's spare CPU is
		// still there for a job that wants it.
		c.RunnerJobCPU = "400m"
	}
	if c.RunnerJobMemory == "" {
		c.RunnerJobMemory = "1536Mi"
	}
	if c.RunnerHeavyMachineType == "" {
		// 4 vCPU / 32 GB: about 28 GiB allocatable after GKE's reservations,
		// so one 12Gi job per node with room to burst to its 24Gi limit.
		// Compiling a Pulumi Go program against pulumi-cloudflare v6 is what
		// needs it: that SDK is one package, and its one compile process peaks
		// at about 14.4 GB, more than an e2-standard-4 holds.
		c.RunnerHeavyMachineType = "e2-highmem-4"
	}
	if c.RunnerHeavyCPU == "" {
		// Also what keeps it to one heavy job per node: two 12Gi pods fit in
		// an e2-highmem-4's memory, but two 2-CPU pods do not fit in the CPU
		// GKE's own pods leave free.
		c.RunnerHeavyCPU = "2"
	}
	if c.RunnerHeavyMemory == "" {
		c.RunnerHeavyMemory = "12Gi"
	}
	if c.OpsMachineType == "" {
		// The engine itself is gcloud and SSH: the dump, the snapshot and the
		// restore copy all run on the VM or in Google's APIs, not here.
		c.OpsMachineType = "e2-standard-2"
	}
}

// quantity is a loose match for a Kubernetes resource quantity, enough to make a
// typo fail at preview rather than when the first job pod is created.
var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|Ki|Mi|Gi)?$`)

func validate(c *Config) error {
	if !strings.Contains(c.Domain, ".") {
		return fmt.Errorf("gitea:domain %q does not look like a hostname", c.Domain)
	}
	if !strings.HasPrefix(c.Zone, c.Region) {
		return fmt.Errorf("gitea:zone %q is not in gcp:region %q", c.Zone, c.Region)
	}
	switch c.RequireSigninView {
	case "false", "true", "expensive":
	default:
		return fmt.Errorf("gitea:requireSigninView %q must be false, true, or expensive", c.RequireSigninView)
	}
	switch c.WAFMode {
	case "On", "DetectionOnly", "Off":
	default:
		return fmt.Errorf("gitea:wafMode %q must be On, DetectionOnly, or Off", c.WAFMode)
	}
	if c.RunnersEnabled {
		if c.RunnerBurstMaxNodes < 0 || c.RunnerBurstMaxNodes > 10 {
			// The upper bound is a cost guard, not a technical limit.
			return fmt.Errorf("gitea:runnerBurstMaxNodes %d must be between 0 and 10", c.RunnerBurstMaxNodes)
		}
		if c.RunnerReplicas < 0 || c.RunnerReplicas > 10 {
			// Each replica is an always-on base node of its own (one DinD
			// runner per node, see pkg/runner). Also a cost guard.
			return fmt.Errorf("gitea:runnerReplicas %d must be between 0 and 10", c.RunnerReplicas)
		}
		switch c.RunnerBaseCommitment {
		case "", "1y", "3y":
		default:
			return fmt.Errorf("gitea:runnerBaseCommitment %q must be 1y, 3y, or unset", c.RunnerBaseCommitment)
		}
		if c.RunnerBaseCommitment != "" {
			if _, _, err := E2Shape(c.RunnerMachineType); err != nil {
				return fmt.Errorf("gitea:runnerBaseCommitment: %w", err)
			}
		}
		if c.RunnerCapacity < 1 {
			return fmt.Errorf("gitea:runnerCapacity %d must be at least 1", c.RunnerCapacity)
		}
		if c.RunnerJobCapacity < 0 {
			return fmt.Errorf("gitea:runnerJobCapacity %d must not be negative", c.RunnerJobCapacity)
		}
		if !quantity.MatchString(c.RunnerJobCPU) {
			return fmt.Errorf("gitea:runnerJobCpu %q is not a Kubernetes quantity (e.g. 500m, 1)", c.RunnerJobCPU)
		}
		if !quantity.MatchString(c.RunnerJobMemory) {
			return fmt.Errorf("gitea:runnerJobMemory %q is not a Kubernetes quantity (e.g. 1536Mi, 2Gi)", c.RunnerJobMemory)
		}
		if _, _, err := E2Shape(c.OpsMachineType); err != nil {
			return fmt.Errorf("gitea:opsMachineType: %w", err)
		}
		if c.RunnerHeavyMaxNodes < 0 || c.RunnerHeavyMaxNodes > 5 {
			// A cost guard: these are on-demand 32 GB machines.
			return fmt.Errorf("gitea:runnerHeavyMaxNodes %d must be between 0 and 5", c.RunnerHeavyMaxNodes)
		}
		if !quantity.MatchString(c.RunnerHeavyCPU) {
			return fmt.Errorf("gitea:runnerHeavyCpu %q is not a Kubernetes quantity (e.g. 2, 3500m)", c.RunnerHeavyCPU)
		}
		if !quantity.MatchString(c.RunnerHeavyMemory) {
			return fmt.Errorf("gitea:runnerHeavyMemory %q is not a Kubernetes quantity (e.g. 12Gi)", c.RunnerHeavyMemory)
		}
		if !strings.HasSuffix(c.RunnerImageTag, "-dind-rootless") {
			// The pod spec hard-codes the rootless image's uid and paths
			// (/home/rootless/...), and the security model depends on dockerd
			// NOT running as root. A plain or -dind tag would start and then
			// fail in confusing ways, or quietly run a root daemon.
			return fmt.Errorf("gitea:runnerImageTag %q must be a -dind-rootless tag", c.RunnerImageTag)
		}
	}
	return nil
}

// GitHubConfigured reports whether enough GitHub settings are present to wire up
// push triggers. Everything else still deploys without them, so a fresh stack can
// come up before the GitHub App install is done.
func (c *Config) GitHubConfigured() bool {
	return c.GitHubOwner != "" &&
		c.GitHubOwner != "CHANGEME" &&
		c.GitHubRepo != "" &&
		c.GitHubInstallationID > 0
}

// ARHost is the Artifact Registry endpoint for the configured region.
func (c *Config) ARHost() string { return c.Region + "-docker.pkg.dev" }

// RunnerBaseNodes is the base pool's fixed size: one node per DinD runner
// replica (pod anti-affinity allows no more per node), and never fewer than
// one, because the runners have to stay up to poll Gitea.
func (c *Config) RunnerBaseNodes() int { return max(1, c.RunnerReplicas) }

// CommitmentPlan is the Compute Engine plan for gitea:runnerBaseCommitment, or
// "" when there is none.
func (c *Config) CommitmentPlan() string {
	switch c.RunnerBaseCommitment {
	case "1y":
		return "TWELVE_MONTH"
	case "3y":
		return "THIRTY_SIX_MONTH"
	}
	return ""
}

var (
	e2Predefined = regexp.MustCompile(`^e2-(standard|highmem|highcpu)-([0-9]+)$`)
	e2Custom     = regexp.MustCompile(`^e2-custom-([0-9]+)-([0-9]+)$`)
	// Memory per vCPU, in MB, of each predefined E2 family.
	e2PerVCPU = map[string]int{"standard": 4096, "highmem": 8192, "highcpu": 1024}
)

// E2Shape returns the vCPUs and memory (MB) of an E2 machine type, which is
// what a resource-based commitment is bought in. Shared-core types
// (e2-micro/small/medium) cannot be committed to and are refused, as is
// anything that is not E2, whose commitments are a different type.
func E2Shape(machineType string) (vcpus, memoryMB int, err error) {
	if m := e2Custom.FindStringSubmatch(machineType); m != nil {
		vcpus, _ = strconv.Atoi(m[1])
		memoryMB, _ = strconv.Atoi(m[2])
		return vcpus, memoryMB, nil
	}
	if m := e2Predefined.FindStringSubmatch(machineType); m != nil {
		vcpus, _ = strconv.Atoi(m[2])
		return vcpus, vcpus * e2PerVCPU[m[1]], nil
	}
	return 0, 0, fmt.Errorf("%q is not an E2 machine type a commitment can be sized from "+
		"(e2-standard-N, e2-highmem-N, e2-highcpu-N or e2-custom-N-M)", machineType)
}

// RunnerImage is the Docker-in-Docker runner's image.
func (c *Config) RunnerImage() string { return "docker.io/gitea/runner:" + c.RunnerImageTag }

// RunnerPodsImage is the pod runner's image: the same release without a Docker
// daemon, which a runner that only creates job pods has no use for.
func (c *Config) RunnerPodsImage() string {
	return "docker.io/gitea/runner:" + strings.TrimSuffix(c.RunnerImageTag, "-dind-rootless")
}

// FQDN returns the domain with a trailing dot, as Cloud DNS requires.
func (c *Config) FQDN() string { return c.Domain + "." }
