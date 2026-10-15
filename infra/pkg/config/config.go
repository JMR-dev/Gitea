// Package config turns loose Pulumi stack config into one typed struct, so the
// rest of the program never reaches back into the config bag and every key has
// exactly one spelling.
package config

import (
	"errors"
	"fmt"
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

	RequireSigninView bool

	// WAFMode is the Coraza SecRuleEngine setting. It also decides whether the
	// fail2ban jail that acts on WAF verdicts is enabled at all.
	WAFMode string

	GitHubOwner          string
	GitHubRepo           string
	GitHubInstallationID int
	GitHubPATSecret      string

	InfraBuildServiceAccount string

	// RunnersEnabled gates the whole GKE runner slice: the cluster, its network,
	// and the runner workload. Off by default, because it depends on a secret
	// that only exists after scripts/bootstrap.sh has been re-run.
	RunnersEnabled    bool
	RunnerImageTag    string
	RunnerReplicas    int
	RunnerCapacity    int
	RunnerMachineType string
	RunnerMaxNodes    int
	RunnerSpot        bool
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

		RequireSigninView: c.GetBool("requireSigninView"),
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
		RunnerMaxNodes:    c.GetInt("runnerMaxNodes"),
	}

	// These two have non-zero defaults where zero/false is ALSO a meaningful
	// setting (replicas=0 is the clean-teardown step; spot=false is on-demand
	// nodes), so GetInt/GetBool -- which cannot tell "unset" from 0/false --
	// would make the explicit value impossible to express.
	var err error
	if cfg.RunnerReplicas, err = c.TryInt("runnerReplicas"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerReplicas: %w", err)
		}
		cfg.RunnerReplicas = 1
	}
	if cfg.RunnerSpot, err = c.TryBool("runnerSpot"); err != nil {
		if !errors.Is(err, config.ErrMissingVar) {
			return nil, fmt.Errorf("gitea:runnerSpot: %w", err)
		}
		cfg.RunnerSpot = true
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
		// 2 vCPU / 8 GB: enough for a docker build of a Go service. Each runner
		// pod is sized to fill one of these, so replicas map to nodes.
		c.RunnerMachineType = "e2-standard-2"
	}
	if c.RunnerMaxNodes == 0 {
		c.RunnerMaxNodes = 3
	}
}

func validate(c *Config) error {
	if !strings.Contains(c.Domain, ".") {
		return fmt.Errorf("gitea:domain %q does not look like a hostname", c.Domain)
	}
	if !strings.HasPrefix(c.Zone, c.Region) {
		return fmt.Errorf("gitea:zone %q is not in gcp:region %q", c.Zone, c.Region)
	}
	switch c.WAFMode {
	case "On", "DetectionOnly", "Off":
	default:
		return fmt.Errorf("gitea:wafMode %q must be On, DetectionOnly, or Off", c.WAFMode)
	}
	if c.RunnersEnabled {
		if c.RunnerMaxNodes < 1 || c.RunnerMaxNodes > 10 {
			// The upper bound is a cost guard, not a technical limit.
			return fmt.Errorf("gitea:runnerMaxNodes %d must be between 1 and 10", c.RunnerMaxNodes)
		}
		if c.RunnerReplicas < 0 || c.RunnerReplicas > c.RunnerMaxNodes {
			// One runner pod per node by construction (see pkg/runner), so more
			// replicas than nodes would leave pods Pending forever.
			return fmt.Errorf("gitea:runnerReplicas %d must be between 0 and gitea:runnerMaxNodes (%d)",
				c.RunnerReplicas, c.RunnerMaxNodes)
		}
		if c.RunnerCapacity < 1 {
			return fmt.Errorf("gitea:runnerCapacity %d must be at least 1", c.RunnerCapacity)
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

// RunnerImage is the Gitea Actions runner image the GKE runners use.
func (c *Config) RunnerImage() string { return "docker.io/gitea/runner:" + c.RunnerImageTag }

// FQDN returns the domain with a trailing dot, as Cloud DNS requires.
func (c *Config) FQDN() string { return c.Domain + "." }
