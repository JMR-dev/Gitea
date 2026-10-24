package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

type fakePub struct{ got []Message }

func (f *fakePub) Publish(_ *http.Request, m Message) error { f.got = append(f.got, m); return nil }

type fakeStatus struct{ got []Status }

func (f *fakeStatus) Post(_ context.Context, s Status) error { f.got = append(f.got, s); return nil }

var secret = []byte("test-secret")

func sign(body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func newHook() (*Webhook, *fakePub, *fakeStatus) {
	p, s := &fakePub{}, &fakeStatus{}
	return &Webhook{
		Secret: secret, Repo: "JMR-dev/Gitea", Authors: []string{"JMR-dev"},
		Topics: map[string]string{
			ModePreview: "projects/ci/topics/ci-preview",
			ModeDeploy:  "projects/ci/topics/ci-deploy",
			ModeGated:   "projects/ci/topics/ci-gated",
		},
		Pub: p, Status: s,
	}, p, s
}

func deliver(t *testing.T, h *Webhook, event string, body []byte, sig func([]byte) string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", event)
	req.Header.Set("X-Gitea-Delivery", "d-1")
	if sig != nil {
		req.Header.Set("X-Gitea-Signature", sig(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignature(t *testing.T) {
	body := fixture(t, "push_main.json")
	for _, tc := range []struct {
		name string
		sig  func([]byte) string
		want int
	}{
		{"good", sign, http.StatusAccepted},
		{"missing", nil, http.StatusUnauthorized},
		{"wrong key", func(b []byte) string {
			m := hmac.New(sha256.New, []byte("other"))
			m.Write(b)
			return hex.EncodeToString(m.Sum(nil))
		}, http.StatusUnauthorized},
		{"not hex", func([]byte) string { return "zz" }, http.StatusUnauthorized},
		{"truncated", func(b []byte) string { return sign(b)[:32] }, http.StatusUnauthorized},
		{"other body", func([]byte) string { return sign([]byte("{}")) }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pub, _ := newHook()
			if rec := deliver(t, h, "push", body, tc.sig); rec.Code != tc.want {
				t.Fatalf("code %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusAccepted && len(pub.got) != 0 {
				t.Fatal("published despite a bad signature")
			}
		})
	}
}

func TestHubSignatureHeader(t *testing.T) {
	h, pub, _ := newHook()
	body := fixture(t, "push_main.json")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sign(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
}

func TestEmptySecretRefusesEverything(t *testing.T) {
	h, _, _ := newHook()
	h.Secret = nil
	empty := func(b []byte) string {
		m := hmac.New(sha256.New, nil)
		m.Write(b)
		return hex.EncodeToString(m.Sum(nil))
	}
	if rec := deliver(t, h, "push", fixture(t, "push_main.json"), empty); rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestPushFixture(t *testing.T) {
	h, pub, _ := newHook()
	rec := deliver(t, h, "push", fixture(t, "push_main.json"), sign)
	if rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	m := pub.got[0]
	want := map[string]string{
		"sha": "26de5015d2ded3c765a798d354b8a56abe0a98a7", "ref": "main", "pr": "",
		"mode": ModeDeploy, "pipelines": "infra", "delivery": "d-1",
	}
	if m.Topic != "projects/ci/topics/ci-deploy" || !equal(m.Attributes, want) {
		t.Fatalf("got %s %v", m.Topic, m.Attributes)
	}
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func pushBody(t *testing.T, mut func(map[string]any)) []byte {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "push_main.json"), &p); err != nil {
		t.Fatal(err)
	}
	mut(p)
	b, _ := json.Marshal(p)
	return b
}

func TestPushIgnored(t *testing.T) {
	for name, mut := range map[string]func(map[string]any){
		"other branch": func(p map[string]any) { p["ref"] = "refs/heads/feature" },
		"tag":          func(p map[string]any) { p["ref"] = "refs/tags/v1" },
		"deleted":      func(p map[string]any) { p["after"] = zeroSHA },
	} {
		t.Run(name, func(t *testing.T) {
			h, pub, _ := newHook()
			if rec := deliver(t, h, "push", pushBody(t, mut), sign); rec.Code != http.StatusNoContent || len(pub.got) != 0 {
				t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
			}
		})
	}
}

func TestOtherRepoRefused(t *testing.T) {
	h, pub, _ := newHook()
	body := pushBody(t, func(p map[string]any) { p["repository"] = map[string]any{"full_name": "JMR-dev/other"} })
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusBadRequest || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
}

func TestNothingToDeployPostsSuccess(t *testing.T) {
	h, pub, st := newHook()
	body := pushBody(t, func(p map[string]any) {
		p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"README.md", "docs/cicd.md", "cicd/main.go"}}}
	})
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusNoContent || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	if len(st.got) != 1 || st.got[0].State != "success" || st.got[0].Context != "cloudbuild/deploy" {
		t.Fatalf("statuses %+v", st.got)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name      string
		paths     []string
		complete  bool
		mode      string
		pipelines []string
	}{
		{"infra code", []string{"infra/pkg/compute/compute.go"}, true, ModeDeploy, []string{"infra"}},
		{"vm config", []string{"vm/config/app.ini.tmpl"}, true, ModeDeploy, []string{"infra"}},
		{"infra pipeline", []string{"cloudbuild/infra.yaml"}, true, ModeDeploy, []string{"infra"}},
		{"image only", []string{"image/gitea/Dockerfile"}, true, ModeDeploy, []string{"image"}},
		{"caddy pin", []string{"image/caddy.version"}, true, ModeDeploy, []string{"image"}},
		{"image pipeline", []string{"cloudbuild/image.yaml"}, true, ModeDeploy, []string{"image"}},
		{"both, infra first", []string{"image/gitea/Dockerfile", "infra/main.go"}, true, ModeDeploy, []string{"infra", "image"}},
		{"gitea major", []string{"image/gitea.version"}, true, ModeGated, []string{"image"}},
		{"alpine minor", []string{"image/alpine.version", "docs/x.md"}, true, ModeGated, []string{"image"}},
		{"bootstrap replaces the vm", []string{"vm/bootstrap.sh"}, true, ModeGated, []string{"infra"}},
		{"gated wins over deploy", []string{"infra/main.go", "image/gitea.version"}, true, ModeGated, []string{"infra", "image"}},
		{"docs only", []string{"docs/runbook.md", "README.md"}, true, ModeDeploy, nil},
		{"engine only", []string{"scripts/ops/gitea_ops.py"}, true, ModeDeploy, nil},
		{"ci program only", []string{"cicd/pkg/relay/relay.go", "cicd/relay/webhook.go"}, true, ModeDeploy, nil},
		{"infra deploy driver", []string{"scripts/cicd/infra_deploy.py"}, true, ModeDeploy, []string{"infra"}},
		{"shared release gate", []string{"scripts/cicd/release_gate.py"}, true, ModeDeploy, []string{"infra", "image"}},
		{"cicd bootstrap deploys nothing", []string{"scripts/cicd/bootstrap.py"}, true, ModeDeploy, nil},
		{"cicd tests deploy nothing", []string{"scripts/cicd/test_cicd.py"}, true, ModeDeploy, nil},
		{"lookalike prefix", []string{"infrastructure.md", "images/logo.png", "vmware.txt"}, true, ModeDeploy, nil},
		{"truncated", []string{"docs/x.md"}, false, ModeGated, []string{"infra", "image"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, pipelines, _ := Classify(tc.paths, tc.complete)
			if mode != tc.mode || !slices.Equal(pipelines, tc.pipelines) {
				t.Fatalf("got %s %v, want %s %v", mode, pipelines, tc.mode, tc.pipelines)
			}
		})
	}
}

func TestTruncatedPushGoesToGated(t *testing.T) {
	h, pub, _ := newHook()
	body := pushBody(t, func(p map[string]any) { p["total_commits"] = 40 })
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	if pub.got[0].Topic != "projects/ci/topics/ci-gated" {
		t.Fatalf("topic %s", pub.got[0].Topic)
	}
}

func prBody(t *testing.T, mut func(map[string]any)) []byte {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "pull_request_synchronized.json"), &p); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(p)
	}
	b, _ := json.Marshal(p)
	return b
}

func TestPullRequestPreview(t *testing.T) {
	for _, action := range []string{"opened", "synchronized", "reopened"} {
		t.Run(action, func(t *testing.T) {
			h, pub, _ := newHook()
			body := prBody(t, func(p map[string]any) { p["action"] = action })
			if rec := deliver(t, h, "pull_request", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
				t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
			}
			m := pub.got[0]
			if m.Topic != "projects/ci/topics/ci-preview" || m.Attributes["mode"] != ModePreview ||
				m.Attributes["sha"] != "1111111111111111111111111111111111111111" || m.Attributes["pr"] != "12" ||
				m.Attributes["ref"] != "cicd-cloud-build-project" || m.Attributes["pipelines"] != "infra,image" {
				t.Fatalf("got %s %v", m.Topic, m.Attributes)
			}
		})
	}
}

func TestPullRequestNoPreview(t *testing.T) {
	for name, mut := range map[string]func(map[string]any){
		"closed": func(p map[string]any) { p["action"] = "closed" },
		"edited": func(p map[string]any) { p["action"] = "edited" },
		"fork": func(p map[string]any) {
			pr := p["pull_request"].(map[string]any)
			pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "mallory/Gitea"}
		},
		"stranger": func(p map[string]any) {
			p["pull_request"].(map[string]any)["user"] = map[string]any{"login": "mallory"}
		},
		// Allowed author, but a different (disallowed) account pushed the head.
		"stranger-pusher": func(p map[string]any) {
			p["sender"] = map[string]any{"login": "mallory"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, pub, _ := newHook()
			if rec := deliver(t, h, "pull_request", prBody(t, mut), sign); rec.Code != http.StatusNoContent || len(pub.got) != 0 {
				t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
			}
		})
	}
}

func TestOtherEventsIgnored(t *testing.T) {
	h, pub, _ := newHook()
	if rec := deliver(t, h, "issues", []byte(`{}`), sign); rec.Code != http.StatusNoContent || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
}

func TestMalformedPayloadRefused(t *testing.T) {
	h, pub, _ := newHook()
	if rec := deliver(t, h, "push", []byte(`{"ref":`), sign); rec.Code != http.StatusBadRequest || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
}

func TestGetRefused(t *testing.T) {
	h, _, _ := newHook()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestOversizedBodyRefused(t *testing.T) {
	h, pub, _ := newHook()
	body := []byte(`{"ref":"` + strings.Repeat("a", maxBody) + `"}`)
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusRequestEntityTooLarge || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
}

func TestReleaseCodeNeedsDrills(t *testing.T) {
	h, pub, st := newHook()
	body := pushBody(t, func(p map[string]any) {
		p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"scripts/ops/vm-steps.sh"}}}
	})
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusNoContent || len(pub.got) != 0 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	var contexts []string
	for _, s := range st.got {
		contexts = append(contexts, s.Context+"="+s.State)
	}
	want := []string{"cloudbuild/deploy=success", "cloudbuild/drill-image-fail-verify=pending",
		"cloudbuild/drill-image-fail-after-migrate=pending", "cloudbuild/drill-infra-fail-verify=pending"}
	if !slices.Equal(contexts, want) {
		t.Fatalf("statuses %v, want %v", contexts, want)
	}
}

func TestDrillStatusesByPipeline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		paths    []string
		complete bool
		want     int
	}{
		{"image pipeline only", []string{"cloudbuild/image.yaml"}, true, 2},
		{"infra deploy script", []string{"scripts/cicd/infra_deploy.py"}, true, 1},
		{"shared engine", []string{"scripts/ops/gitea_ops.py"}, true, 3},
		{"not release code", []string{"infra/main.go", "image/gitea/Dockerfile", "scripts/cicd/bootstrap.py"}, true, 0},
		{"truncated", nil, false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DrillStatuses("s", tc.paths, tc.complete); len(got) != tc.want {
				t.Fatalf("got %d statuses %+v, want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestDeployStillPublishesWithDrillStatuses(t *testing.T) {
	h, pub, st := newHook()
	body := pushBody(t, func(p map[string]any) {
		p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"cloudbuild/infra.yaml"}}}
	})
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	if len(st.got) != 1 || st.got[0].Context != "cloudbuild/drill-infra-fail-verify" {
		t.Fatalf("statuses %+v", st.got)
	}
}

// A change to the infra deploy driver under scripts/cicd/ now triggers the
// infra pipeline (not "nothing to deploy"), and still re-posts its drill
// reminder so the gate holds the deploy until that drill is re-run.
func TestCicdScriptTriggersDeploy(t *testing.T) {
	h, pub, st := newHook()
	body := pushBody(t, func(p map[string]any) {
		p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"scripts/cicd/infra_deploy.py"}}}
	})
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	if m := pub.got[0]; m.Attributes["mode"] != ModeDeploy || m.Attributes["pipelines"] != PipelineInfra {
		t.Fatalf("attrs %+v", m.Attributes)
	}
	if len(st.got) != 1 || st.got[0].Context != "cloudbuild/drill-infra-fail-verify" || st.got[0].State != "pending" {
		t.Fatalf("statuses %+v", st.got)
	}
}
