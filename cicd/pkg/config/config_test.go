package config

import (
	"strings"
	"testing"
)

func valid() *Config {
	c := &Config{Project: "gitea-ci-123456", OperatorEmail: "op@example.com"}
	applyDefaults(c)
	return c
}

func TestDefaults(t *testing.T) {
	c := valid()
	if c.AppProject != "gitea-496920" || c.AppZone != "us-east1-b" || c.GiteaRepo != "JMR-dev/Gitea" ||
		len(c.Authors) != 1 || c.Authors[0] != "JMR-dev" || c.AutoDeploy || len(c.PipelineRegions) != 5 {
		t.Fatalf("defaults %+v", c)
	}
	if err := validate(c, true); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		mut    func(*Config)
		secret bool
		want   string
	}{
		"same project":                       {func(c *Config) { c.Project = c.AppProject }, true, "is the app project"},
		"pipelines in the launchers' region": {func(c *Config) { c.PipelineRegions = append(c.PipelineRegions, c.Region) }, true, "must not include gcp:region"},
		"pipeline region typo":               {func(c *Config) { c.PipelineRegions = []string{"us-east-4"} }, true, "not a region name"},
		"pipeline region twice":              {func(c *Config) { c.PipelineRegions = []string{"us-east4", "us-east4"} }, true, "twice"},
		"zone not region":                    {func(c *Config) { c.AppZone = "us-west1-a" }, true, "not in gcp:region"},
		"http":                               {func(c *Config) { c.GiteaURL = "http://x" }, true, "must be https"},
		"repo":                               {func(c *Config) { c.GiteaRepo = "nope" }, true, "owner/name"},
		"no email":                           {func(c *Config) { c.OperatorEmail = "" }, true, "is required"},
		"email in clear":                     {func(c *Config) {}, false, "must be set with --secret"},
		"not an address":                     {func(c *Config) { c.OperatorEmail = "Op <op@example.com>" }, true, "bare email"},
		"reader not a service account":       {func(c *Config) { c.WatchtowerReader = "someone@example.net" }, true, "not a service account"},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			tc.mut(c)
			err := validate(c, tc.secret)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "op@example.com") {
				t.Fatal("a secret leaked into an error")
			}
		})
	}
}

func TestCertAlertDaysDefault(t *testing.T) {
	if got, err := certAlertDays(""); err != nil || got != 18 {
		t.Fatalf("unset: got %d, %v; want 18", got, err)
	}
}

func TestCertAlertDaysRejects(t *testing.T) {
	for _, raw := range []string{"0", "-1", "366", "1.5", "18.0", "abc", "18d", " 18", "1e2"} {
		if got, err := certAlertDays(raw); err == nil || !strings.Contains(err.Error(), "gitea-cicd:certAlertDays") {
			t.Errorf("%q: got %d, %v; want an error naming gitea-cicd:certAlertDays", raw, got, err)
		}
	}
}

func TestCertAlertDaysAccepts(t *testing.T) {
	for raw, want := range map[string]int{"1": 1, "18": 18, "120": 120, "365": 365} {
		if got, err := certAlertDays(raw); err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", raw, got, err, want)
		}
	}
}

// A threshold above the days any healthy certificate has left when Caddy
// renews it fires whatever Caddy does: that is a drill, and `pulumi up` says
// so. The longest certificate Let's Encrypt issues is 90 days (until
// 2027-02-10), renewed at 30 days left, so 30 itself never fires on a healthy
// one and 31 always does.
func TestCertAlertIsDrill(t *testing.T) {
	for days, want := range map[int]bool{18: false, 21: false, 25: false, 30: false, 31: true, 120: true} {
		if got := CertAlertIsDrill(days); got != want {
			t.Errorf("CertAlertIsDrill(%d) = %v, want %v", days, got, want)
		}
	}
}
