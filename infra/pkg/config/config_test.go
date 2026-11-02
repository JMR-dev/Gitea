package config

import (
	"strings"
	"testing"
)

func valid() *Config {
	c := &Config{Project: "p", Domain: "gitea.example.com"}
	applyDefaults(c)
	return c
}

func TestWatchtowerReader(t *testing.T) {
	for reader, want := range map[string]string{
		"":                              "",
		"r@p.iam.gserviceaccount.com":   "",
		"someone@example.net":           "not a service account",
		"r.iam.gserviceaccount.com":     "not a service account",
		"a@b@p.iam.gserviceaccount.com": "not a service account",
	} {
		c := valid()
		c.WatchtowerReader = reader
		err := validate(c)
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", reader, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: got %v, want %q", reader, err, want)
		}
	}
}
