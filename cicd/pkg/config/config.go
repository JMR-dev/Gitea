// Package config turns the cicd stack's config into one typed struct, so the
// rest of the program never reaches back into the config bag and every key has
// exactly one spelling.
package config

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

type Config struct {
	// Project is this CI project (gcp:project), created by
	// scripts/cicd/bootstrap.py. Everything here lives in it, except the
	// grants it makes into the app project.
	Project string
	Region  string

	// The app project and what in it the pipelines touch. Defaults match
	// infra/ (gitea-496920, the VM, the runner cluster).
	AppProject    string
	AppZone       string
	AppVM         string
	RunnerCluster string

	// GiteaURL and GiteaRepo are where the webhook comes from and the
	// statuses go. The launcher clones the repo anonymously over HTTPS: it is
	// public, and Caddy's @gittransport matcher keeps git off the WAF.
	GiteaURL  string
	GiteaRepo string
	// Authors whose pull requests get a preview. A preview runs the PR's
	// code (pulumi executes the program), so this is a trust list.
	Authors []string

	// OperatorEmail receives the alerts and may approve gated builds. A
	// secret, because this repo is mirrored publicly.
	OperatorEmail string

	// AutoDeploy is the greenlight gate and the kill switch: off, the deploy,
	// gated and rebuild triggers are disabled and the weekly schedule paused,
	// so nothing reaches prod except by hand. It stays off until drills D1-D6
	// have passed (docs/cicd.md).
	AutoDeploy bool
}

func Load(ctx *pulumi.Context) (*Config, error) {
	gcp := config.New(ctx, "gcp")
	c := config.New(ctx, "gitea-cicd")

	cfg := &Config{
		Project:       gcp.Require("project"),
		Region:        gcp.Get("region"),
		AppProject:    c.Get("appProject"),
		AppZone:       c.Get("appZone"),
		AppVM:         c.Get("appVm"),
		RunnerCluster: c.Get("runnerCluster"),
		GiteaURL:      c.Get("giteaUrl"),
		GiteaRepo:     c.Get("giteaRepo"),
		Authors:       strings.Fields(strings.ReplaceAll(c.Get("allowedAuthors"), ",", " ")),
		OperatorEmail: c.Get("operatorEmail"),
		AutoDeploy:    c.GetBool("autoDeploy"),
	}
	applyDefaults(cfg)
	return cfg, validate(cfg, ctx.IsConfigSecret("gitea-cicd:operatorEmail"))
}

func applyDefaults(c *Config) {
	if c.Region == "" {
		c.Region = "us-east1"
	}
	if c.AppProject == "" {
		c.AppProject = "gitea-496920"
	}
	if c.AppZone == "" {
		c.AppZone = c.Region + "-b"
	}
	if c.AppVM == "" {
		c.AppVM = "gitea-vm"
	}
	if c.RunnerCluster == "" {
		c.RunnerCluster = "gitea-runners"
	}
	if c.GiteaURL == "" {
		c.GiteaURL = "https://gitea.jasonmross.dev"
	}
	c.GiteaURL = strings.TrimRight(c.GiteaURL, "/")
	if c.GiteaRepo == "" {
		c.GiteaRepo = "JMR-dev/Gitea"
	}
	if len(c.Authors) == 0 {
		c.Authors = []string{strings.SplitN(c.GiteaRepo, "/", 2)[0]}
	}
}

func validate(c *Config, emailIsSecret bool) error {
	if c.Project == c.AppProject {
		return fmt.Errorf("gcp:project %q is the app project; the CI stack belongs in its own (scripts/cicd/bootstrap.py)", c.Project)
	}
	if !strings.HasPrefix(c.AppZone, c.Region) {
		return fmt.Errorf("gitea-cicd:appZone %q is not in gcp:region %q", c.AppZone, c.Region)
	}
	if !strings.HasPrefix(c.GiteaURL, "https://") {
		return fmt.Errorf("gitea-cicd:giteaUrl %q must be https", c.GiteaURL)
	}
	if strings.Count(c.GiteaRepo, "/") != 1 {
		return fmt.Errorf("gitea-cicd:giteaRepo %q must be owner/name", c.GiteaRepo)
	}
	if c.OperatorEmail == "" {
		return fmt.Errorf("gitea-cicd:operatorEmail is required: `pulumi config set --secret gitea-cicd:operatorEmail <you>`")
	}
	if !emailIsSecret {
		return fmt.Errorf("gitea-cicd:operatorEmail must be set with --secret; this repo is mirrored publicly")
	}
	if a, err := mail.ParseAddress(c.OperatorEmail); err != nil || a.Address != c.OperatorEmail {
		// Never echoed: it is a secret.
		return fmt.Errorf("gitea-cicd:operatorEmail is not a bare email address")
	}
	return nil
}

// AppSA is a service account email in the app project.
func (c *Config) AppSA(name string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", name, c.AppProject)
}

// SA is a service account email in this project.
func (c *Config) SA(name string) string {
	return fmt.Sprintf("%s@%s.iam.gserviceaccount.com", name, c.Project)
}
