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
