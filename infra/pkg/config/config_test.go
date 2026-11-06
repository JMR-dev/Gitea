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

func TestWatchtowerPeerSecret(t *testing.T) {
	for name, ok := range map[string]bool{
		"": true,
		"projects/watchtower-observe/secrets/wireguard-gitea-vm-peer": true,
		"projects/example-observe/secrets/wireguard_example_peer":     true,
		// The id alone: the project is watchtower's, not this one.
		"wireguard-gitea-vm-peer": false,
		// A version would pin it; the VM reads the latest.
		"projects/watchtower-observe/secrets/wireguard-gitea-vm-peer/versions/latest": false,
		"projects/x/secrets/wireguard-gitea-vm-peer":                                  false,
		"projects/watchtower-observe/secrets/a b":                                     false,
		"projects/watchtower-observe/secrets/x;id":                                    false,
		" projects/watchtower-observe/secrets/wireguard-gitea-vm-peer":                false,
	} {
		c := valid()
		c.WatchtowerPeerSecret = name
		err := validate(c)
		switch {
		case ok && err != nil:
			t.Errorf("%q: %v", name, err)
		case !ok && (err == nil || !strings.Contains(err.Error(), "resource name")):
			t.Errorf("%q: got %v, want a resource name error", name, err)
		}
	}
}
