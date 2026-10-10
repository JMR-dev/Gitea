// Package config turns loose Pulumi stack config into one typed struct, so the
// rest of the program never reaches back into the config bag and every key has
// exactly one spelling.
package config

import (
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

// FQDN returns the domain with a trailing dot, as Cloud DNS requires.
func (c *Config) FQDN() string { return c.Domain + "." }
