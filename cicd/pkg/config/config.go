// Package config turns the cicd stack's config into one typed struct, so the
// rest of the program never reaches back into the config bag and every key has
// exactly one spelling.
package config

import (
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// region is a GCP region name, e.g. us-east4 or europe-west1.
var region = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)

type Config struct {
	// Project is this CI project (gcp:project), created by
	// scripts/cicd/bootstrap.py. Everything here lives in it, except the
	// grants it makes into the app project.
	Project string
	Region  string

	// PipelineRegions are where the launchers' pipeline builds run -- never
	// Region, where the launchers themselves run. The default pool caps build
	// vCPUs per region (10 for this project, and Google will not raise it),
	// and a launcher holds its region's vCPUs while it waits on its pipeline,
	// so a pipeline queued behind launchers in their own region could wait on
	// them forever. Each region fits one 8-vCPU pipeline build at a time; five
	// match the project's cap of 10 builds at once (five launcher+pipeline
	// pairs).
	PipelineRegions []string

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

	// WatchtowerReader is the service account our observability stack reads
	// this project's logs and metrics as. Unset, package telemetry creates
	// nothing and cb-infra gets no telemetry roles (docs/cicd.md).
	WatchtowerReader string

	// CertAlertDays is the certificate alert's threshold (package alerting):
	// it fires when Gitea's certificate has fewer days left than this.
	// gitea-cicd:certAlertDays, optional, a whole number from 1 to 365;
	// DefaultCertAlertDays when unset. Above the days any certificate has
	// left when Caddy renews it, it fires on a healthy one: a drill
	// (CertAlertIsDrill, docs/runbook.md).
	CertAlertDays int
}

// CertLifetimeDays is the lifetime of the certificates Let's Encrypt issues
// Gitea: 64 days from 2027-02-10. Caddy (certmagic) renews one when a third
// of it is left, about 21.3 days, and retries until it expires.
//
// Before 2028-02-16, when Let's Encrypt moves to 45-day certificates (renewal
// from 15 days left), set this to 45: alerting's
// TestDefaultThresholdLeavesCaddyItsRetries then fails until
// DefaultCertAlertDays drops to about 10.
const CertLifetimeDays = 64

// LongestCertLifetimeDays is the longest certificate Let's Encrypt issues
// Gitea: 90 days until 2027-02-10. Caddy renews one at 30 days left, the most
// any certificate has when it is renewed, so a threshold above 30 fires on a
// healthy certificate whatever Caddy does (CertAlertIsDrill). Once the last
// 90-day certificate has expired (by 2027-05-11), this can be
// CertLifetimeDays.
const LongestCertLifetimeDays = 90

// DefaultCertAlertDays alerts once Caddy's renewal has been failing for at
// least 3 days (21.3 - 18), and leaves 18 days to fix it.
const DefaultCertAlertDays = 18

// certAlertDays reads gitea-cicd:certAlertDays as Load gets it: unset is the
// default, anything but a whole number from 1 to 365 an error.
// config.GetInt would read "1.5" or "abc" as 0 without a word.
func certAlertDays(raw string) (int, error) {
	if raw == "" {
		return DefaultCertAlertDays, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > 365 {
		return 0, fmt.Errorf("gitea-cicd:certAlertDays %q must be a whole number of days from 1 to 365", raw)
	}
	return days, nil
}

// CertAlertIsDrill says whether a threshold fires on a healthy certificate:
// one above the days left when Caddy renews the longest-lived, which no
// certificate stays above. It fires at once if the certificate has fewer days
// left than that now, and before Caddy's next renewal otherwise.
func CertAlertIsDrill(days int) bool {
	return days > LongestCertLifetimeDays/3
}

func Load(ctx *pulumi.Context) (*Config, error) {
	gcp := config.New(ctx, "gcp")
	c := config.New(ctx, "gitea-cicd")

	cfg := &Config{
		Project:         gcp.Require("project"),
		Region:          gcp.Get("region"),
		PipelineRegions: strings.Fields(strings.ReplaceAll(c.Get("pipelineRegions"), ",", " ")),
		AppProject:      c.Get("appProject"),
		AppZone:         c.Get("appZone"),
		AppVM:           c.Get("appVm"),
		RunnerCluster:   c.Get("runnerCluster"),
		GiteaURL:        c.Get("giteaUrl"),
		GiteaRepo:       c.Get("giteaRepo"),
		Authors:         strings.Fields(strings.ReplaceAll(c.Get("allowedAuthors"), ",", " ")),
		OperatorEmail:   c.Get("operatorEmail"),
		AutoDeploy:      c.GetBool("autoDeploy"),

		WatchtowerReader: c.Get("watchtowerReader"),
	}
	applyDefaults(cfg)
	if err := validate(cfg, ctx.IsConfigSecret("gitea-cicd:operatorEmail")); err != nil {
		return nil, err
	}
	days, err := certAlertDays(c.Get("certAlertDays"))
	if err != nil {
		return nil, err
	}
	cfg.CertAlertDays = days
	if CertAlertIsDrill(days) {
		if err := ctx.Log.Warn(fmt.Sprintf("gitea-cicd:certAlertDays %d is a drill value: this alert will fire, "+
			"at once if the certificate has fewer days left than that, otherwise before Caddy next renews it "+
			"(at %d days left at most). Once the drill is done, `pulumi config rm gitea-cicd:certAlertDays` "+
			"and `make ci-up` (docs/runbook.md)", days, LongestCertLifetimeDays/3), nil); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func applyDefaults(c *Config) {
	if c.Region == "" {
		c.Region = "us-east1"
	}
	if len(c.PipelineRegions) == 0 {
		// US regions whose default pool runs E2_HIGHCPU_8 and publishes to
		// this project's cloud-builds topic (probed 2026-10-24).
		c.PipelineRegions = []string{"us-east4", "us-east5", "us-central1", "us-south1", "us-west1"}
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
	seen := map[string]bool{}
	for _, r := range c.PipelineRegions {
		switch {
		case r == c.Region:
			return fmt.Errorf("gitea-cicd:pipelineRegions must not include gcp:region %q, where the launchers run", r)
		case !region.MatchString(r):
			return fmt.Errorf("gitea-cicd:pipelineRegions: %q is not a region name", r)
		case seen[r]:
			return fmt.Errorf("gitea-cicd:pipelineRegions lists %q twice", r)
		}
		seen[r] = true
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
	if r := c.WatchtowerReader; r != "" && (!strings.HasSuffix(r, ".iam.gserviceaccount.com") || strings.Count(r, "@") != 1) {
		return fmt.Errorf("gitea-cicd:watchtowerReader %q is not a service account email", r)
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
