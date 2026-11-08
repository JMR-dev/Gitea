package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
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
		{"engine vm step deploys nothing", []string{"scripts/ops/ansible/steps/start.yml"}, true, ModeDeploy, nil},
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

// A truncated commit list is gated unless Gitea's API settles the push: from
// the payload alone, or when the head's first parent isn't the push's before
// (a fast-forward or rebase merge of several commits), whose own files are
// then only part of the push.
func TestTruncatedPushGoesToGated(t *testing.T) {
	for name, commits := range map[string]CommitReader{
		"payload alone": nil,
		"linear push": &fakeCommits{commits: map[string]fakeCommit{
			pushAfter: {parents: []string{otherParent}, files: []string{"docs/x.md"}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h, pub, st := newHook()
			h.Commits = commits
			body := pushBody(t, func(p map[string]any) { p["total_commits"] = 40 })
			if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
				t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
			}
			if pub.got[0].Topic != "projects/ci/topics/ci-gated" {
				t.Fatalf("topic %s", pub.got[0].Topic)
			}
			if got := statusList(st.got); !slices.Equal(got, allDrills) {
				t.Fatalf("statuses %v, want %v", got, allDrills)
			}
		})
	}
}

// When the head's first parent is the push's before, its first-parent files
// are the whole push however many commits Gitea left out.
func TestTruncatedMergeIsClassifiedByLookup(t *testing.T) {
	h, pub, st := newHook()
	h.Commits = &fakeCommits{commits: map[string]fakeCommit{
		pushAfter: {parents: []string{pushBefore, otherParent}, files: []string{"image/gitea/Dockerfile"}},
	}}
	body := pushBody(t, func(p map[string]any) { p["total_commits"] = 40 })
	checkPublished(t, h, pub, st, body, pushWant{ModeDeploy, "image", nil})
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
		p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"scripts/ops/ansible/ops.yml"}}}
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
		{"engine vm step", []string{"scripts/ops/ansible/steps/start.yml"}, true, 3},
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

// statusList is each posted status as "context=state", in posting order.
func statusList(got []Status) []string {
	var out []string
	for _, s := range got {
		out = append(out, s.Context+"="+s.State)
	}
	return out
}

// allDrills is what a push whose files aren't known in full owes.
var allDrills = []string{"cloudbuild/drill-image-fail-verify=pending",
	"cloudbuild/drill-image-fail-after-migrate=pending", "cloudbuild/drill-infra-fail-verify=pending"}

// pushWant is what one push to main should publish and post.
type pushWant struct {
	mode      string
	pipelines string
	statuses  []string // statusList's form
}

func checkPublished(t *testing.T, h *Webhook, pub *fakePub, st *fakeStatus, body []byte, want pushWant) {
	t.Helper()
	if rec := deliver(t, h, "push", body, sign); rec.Code != http.StatusAccepted || len(pub.got) != 1 {
		t.Fatalf("code %d, published %d", rec.Code, len(pub.got))
	}
	m := pub.got[0]
	if m.Topic != h.Topics[want.mode] || m.Attributes["mode"] != want.mode || m.Attributes["pipelines"] != want.pipelines {
		t.Errorf("published %s %s %q, want %s %q", m.Topic, m.Attributes["mode"], m.Attributes["pipelines"], want.mode, want.pipelines)
	}
	if got := statusList(st.got); !slices.Equal(got, want.statuses) {
		t.Errorf("statuses %v, want %v", got, want.statuses)
	}
}

// The four merges to main of 2026-11-07 whose branches had main merged into
// them. Gitea 28.3.0 lists every commit in before..after (at most 5, [ui]
// FEED_MAX_COMMIT_NUM), each with its files against its own first parent, so
// a "Merge branch 'main' into ..." commit lists what main gained since the
// branch last synced. In each, the merge's first parent is the push's before.
var olderBranchMerges = []struct {
	pr      int
	fixture string
	after   string
}{
	{47, "push_main_merge_pr47.json", "2ce57a7281102c50d1c6a1ca6c46cbbdb8c5d59b"}, // 4 of 4 commits
	{48, "push_main_merge_pr48.json", "f93e5b769c1f385a4072c51b9476ee18ab649154"}, // 3 of 3
	{43, "push_main_merge_pr43.json", "6fad9f8cb3e832eb460cd6ddc4add853247e27aa"}, // 5 of 6
	{49, "push_main_merge_pr49.json", "96776d039aca88cf58482966a72ca2ab7673c884"}, // 5 of 5
}

// From the payload alone (no commit reader) each merge redeploys what main
// already shipped, and #43's list is truncated, so it's gated. This is the
// relay's behavior whenever Gitea's API can't settle the push.
func TestOlderBranchMergesFromPayloadAlone(t *testing.T) {
	want := map[int]pushWant{
		47: {ModeDeploy, "infra,image", nil},
		48: {ModeDeploy, "infra,image", nil},
		43: {ModeGated, "infra,image", allDrills},
		49: {ModeDeploy, "infra,image", []string{"cloudbuild/drill-infra-fail-verify=pending"}},
	}
	for _, m := range olderBranchMerges {
		t.Run(fmt.Sprintf("#%d", m.pr), func(t *testing.T) {
			h, pub, st := newHook()
			checkPublished(t, h, pub, st, fixture(t, m.fixture), want[m.pr])
		})
	}
}

// What each merge should have deployed: its first-parent diff, which is
// exactly `git diff before after`.
var firstParentWant = map[int]pushWant{
	47: {ModeDeploy, "image", nil}, // image/caddy/Dockerfile
	48: {ModeDeploy, "infra", nil}, // infra/, docs, Makefile
	// cloudbuild/infra.yaml and scripts/cicd/infra_deploy.py are infra's release code.
	43: {ModeDeploy, "infra", []string{"cloudbuild/drill-infra-fail-verify=pending"}},
	49: {ModeDeploy, "infra", nil}, // infra/pkg/iam, infra/pkg/secrets, vm/, scripts/
}

// fakeCommits answers FirstParentFiles from a table keyed by sha, and
// records every call.
type fakeCommits struct {
	commits map[string]fakeCommit
	err     error
	block   bool // wait for the context to end, as a Gitea that hangs would
	calls   []fakeCall
}

type fakeCommit struct{ parents, files []string }

type fakeCall struct {
	sha       string
	remaining time.Duration // until the context's deadline; 0 without one
}

func (f *fakeCommits) FirstParentFiles(ctx context.Context, sha string) ([]string, []string, error) {
	call := fakeCall{sha: sha}
	if d, ok := ctx.Deadline(); ok {
		call.remaining = time.Until(d)
	}
	f.calls = append(f.calls, call)
	if f.block {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(5 * time.Second):
			// A lookup that ignored its timeout gets the answer, late.
		}
	}
	if f.err != nil {
		return nil, nil, f.err
	}
	c, ok := f.commits[sha]
	if !ok {
		return nil, nil, fmt.Errorf("no commit %s", sha)
	}
	return c.parents, c.files, nil
}

// fixtureCommit reads a Gitea commit answer saved in testdata.
func fixtureCommit(t *testing.T, name string) fakeCommit {
	t.Helper()
	var c struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(fixture(t, name), &c); err != nil {
		t.Fatal(err)
	}
	var out fakeCommit
	for _, p := range c.Parents {
		out.parents = append(out.parents, p.SHA)
	}
	for _, f := range c.Files {
		out.files = append(out.files, f.Filename)
	}
	return out
}

// mergeCommits holds what Gitea's API answered on 2026-11-07 for the four
// merges: GET /repos/JMR-dev/Gitea/git/commits/<sha>?files=true.
func mergeCommits(t *testing.T) *fakeCommits {
	return &fakeCommits{commits: map[string]fakeCommit{
		"2ce57a7281102c50d1c6a1ca6c46cbbdb8c5d59b": { // #47
			parents: []string{"202cf565c5d4f98059d3050aa55312da5ad29b83", "9bd5d317cda6aa0b348f8d2ea695d20747a9d0b4"},
			files:   []string{"image/caddy/Dockerfile"},
		},
		"f93e5b769c1f385a4072c51b9476ee18ab649154": { // #48
			parents: []string{"2ce57a7281102c50d1c6a1ca6c46cbbdb8c5d59b", "72f431042694fb10ca862c64a6f0a0c0c8f05b85"},
			files: []string{"Makefile", "docs/runbook.md", "infra/main.go", "infra/pkg/dns/dns.go",
				"infra/pkg/dns/dns_test.go", "infra/pkg/iam/iam.go", "infra/pkg/iam/iam_test.go"},
		},
		"6fad9f8cb3e832eb460cd6ddc4add853247e27aa": { // #43
			parents: []string{"f93e5b769c1f385a4072c51b9476ee18ab649154", "68ba0d9b82390c41c59719d6777cfb774298b56a"},
			files: []string{"Makefile", "cicd/pkg/identity/identity.go", "cicd/pkg/identity/identity_test.go",
				"cloudbuild/infra.yaml", "docs/cicd.md", "scripts/cicd/infra_deploy.py", "scripts/cicd/test_cicd.py"},
		},
		"96776d039aca88cf58482966a72ca2ab7673c884": fixtureCommit(t, "gitea_commit_96776d0.json"), // #49, 20 files
	}}
}

// With Gitea's API, each merge deploys its own change and nothing main
// already shipped.
func TestOlderBranchMergesClassifyByFirstParent(t *testing.T) {
	for _, m := range olderBranchMerges {
		t.Run(fmt.Sprintf("#%d", m.pr), func(t *testing.T) {
			h, pub, st := newHook()
			commits := mergeCommits(t)
			h.Commits = commits
			checkPublished(t, h, pub, st, fixture(t, m.fixture), firstParentWant[m.pr])
			if len(commits.calls) != 1 || commits.calls[0].sha != m.after {
				t.Fatalf("calls %+v, want one for %s", commits.calls, m.after)
			}
			// LookupTimeout unset: 2 s.
			if r := commits.calls[0].remaining; r <= time.Second || r > 2*time.Second {
				t.Fatalf("lookup deadline %s away, want about 2s", r)
			}
		})
	}
}

// push_main.json's before and after, and a sha that is neither.
const (
	pushBefore  = "fef8a62dc1751572cd52f76c112547b957e800af"
	pushAfter   = "26de5015d2ded3c765a798d354b8a56abe0a98a7"
	otherParent = "cccccccccccccccccccccccccccccccccccccccc"
)

// A fast-forward or rebase merge of several commits: the head's first parent
// is the push's second-newest commit, not before, so its own files are only
// part of the push and the payload's lists are used.
func TestLinearPushUsesPayload(t *testing.T) {
	h, pub, st := newHook()
	commits := &fakeCommits{commits: map[string]fakeCommit{
		pushAfter: {parents: []string{otherParent}, files: []string{"docs/x.md"}},
	}}
	h.Commits = commits
	body := pushBody(t, func(p map[string]any) {
		p["total_commits"] = 3
		p["commits"] = []any{
			map[string]any{"id": pushAfter, "modified": []any{"docs/x.md"}},
			map[string]any{"id": otherParent, "modified": []any{"image/gitea/Dockerfile"}},
			map[string]any{"id": "dddddddddddddddddddddddddddddddddddddddd", "added": []any{"infra/pkg/x/x.go"}},
		}
	})
	checkPublished(t, h, pub, st, body, pushWant{ModeDeploy, "infra,image", nil})
	if len(commits.calls) != 1 {
		t.Fatalf("calls %+v", commits.calls)
	}
}

// A lookup that fails or hangs leaves the push unsettled: gated, every
// pipeline, every drill, so a person looks at it.
func TestLookupFailureGoesToGated(t *testing.T) {
	merge47 := olderBranchMerges[0]
	for name, commits := range map[string]*fakeCommits{
		"error":   {err: &httpError{code: 502, body: "bad gateway"}},
		"timeout": {block: true, commits: mergeCommits(t).commits},
	} {
		t.Run(name, func(t *testing.T) {
			h, pub, st := newHook()
			h.Commits = commits
			h.LookupTimeout = 50 * time.Millisecond
			start := time.Now()
			checkPublished(t, h, pub, st, fixture(t, merge47.fixture), pushWant{ModeGated, "infra,image", allDrills})
			if took := time.Since(start); took > time.Second {
				t.Fatalf("took %s: LookupTimeout not applied", took)
			}
		})
	}
}

// Only a push to main that moved an existing head is looked up.
func TestNoLookupWithoutAPushToCompare(t *testing.T) {
	for name, tc := range map[string]struct {
		event string
		body  func(t *testing.T) []byte
	}{
		"before zero": {"push", func(t *testing.T) []byte {
			return pushBody(t, func(p map[string]any) { p["before"] = zeroSHA })
		}},
		"before empty": {"push", func(t *testing.T) []byte {
			return pushBody(t, func(p map[string]any) { delete(p, "before") })
		}},
		"other branch": {"push", func(t *testing.T) []byte {
			return pushBody(t, func(p map[string]any) { p["ref"] = "refs/heads/feature" })
		}},
		"main deleted": {"push", func(t *testing.T) []byte {
			return pushBody(t, func(p map[string]any) { p["after"] = zeroSHA })
		}},
		"pull request": {"pull_request", func(t *testing.T) []byte { return prBody(t, nil) }},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newHook()
			// An answer that would change the result if it were asked for.
			commits := &fakeCommits{commits: map[string]fakeCommit{
				pushAfter: {parents: []string{pushBefore}, files: []string{"image/gitea.version"}},
			}}
			h.Commits = commits
			if rec := deliver(t, h, tc.event, tc.body(t), sign); rec.Code >= 300 {
				t.Fatalf("code %d", rec.Code)
			}
			if len(commits.calls) != 0 {
				t.Fatalf("looked up %+v", commits.calls)
			}
		})
	}
}

// logLines captures the relay's slog output for one test.
func logLines(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			out = append(out, m)
		}
		return out
	}
}

func logLine(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	t.Fatalf("no %q line in %v", msg, lines)
	return nil
}

// A main push's log line says where its files came from, so the next merge
// of an older branch can be checked from the logs.
func TestMainPushLogsItsSource(t *testing.T) {
	t.Run("published, first parent", func(t *testing.T) {
		lines := logLines(t)
		h, _, _ := newHook()
		h.Commits = mergeCommits(t)
		deliver(t, h, "push", fixture(t, "push_main_merge_pr47.json"), sign)
		l := logLine(t, lines(), "published")
		if l["source"] != "first-parent" || l["before"] != "202cf565c5d4f98059d3050aa55312da5ad29b83" ||
			fmt.Sprint(l["files"]) != "[image/caddy/Dockerfile]" || l["files_total"] != 1.0 {
			t.Fatalf("line %v", l)
		}
	})
	t.Run("ignored, payload", func(t *testing.T) {
		lines := logLines(t)
		h, _, _ := newHook()
		deliver(t, h, "push", pushBody(t, func(p map[string]any) {
			p["commits"] = []any{map[string]any{"id": "x", "modified": []any{"README.md"}}}
		}), sign)
		l := logLine(t, lines(), "delivery ignored")
		if l["source"] != "payload" || l["before"] != pushBefore || fmt.Sprint(l["files"]) != "[README.md]" {
			t.Fatalf("line %v", l)
		}
	})
	t.Run("files capped", func(t *testing.T) {
		lines := logLines(t)
		h, _, _ := newHook()
		var many []string
		for i := range 150 {
			many = append(many, fmt.Sprintf("docs/%03d.md", i))
		}
		h.Commits = &fakeCommits{commits: map[string]fakeCommit{pushAfter: {parents: []string{pushBefore}, files: many}}}
		deliver(t, h, "push", fixture(t, "push_main.json"), sign)
		l := logLine(t, lines(), "delivery ignored")
		if files, _ := l["files"].([]any); len(files) != 100 || l["files_total"] != 150.0 {
			t.Fatalf("logged %d files of %v", len(files), l["files_total"])
		}
	})
}

// Against the real Gitea, read-only and anonymous: the repo is public, and
// decidePush publishes and posts nothing. Run with
// RELAY_LIVE_GITEA_URL=https://gitea.jasonmross.dev go test -run Live -v
func TestLiveOlderBranchMerges(t *testing.T) {
	base := os.Getenv("RELAY_LIVE_GITEA_URL")
	if base == "" {
		t.Skip("RELAY_LIVE_GITEA_URL not set")
	}
	h, _, _ := newHook()
	h.Commits = &Gitea{BaseURL: base, Repo: "JMR-dev/Gitea"}
	for _, m := range olderBranchMerges {
		t.Run(fmt.Sprintf("#%d", m.pr), func(t *testing.T) {
			var p pushPayload
			if err := json.Unmarshal(fixture(t, m.fixture), &p); err != nil {
				t.Fatal(err)
			}
			d := h.decidePush(context.Background(), p)
			want := firstParentWant[m.pr]
			if d.msg == nil {
				t.Fatalf("nothing published: %s", d.reason)
			}
			if d.msg.Attributes["mode"] != want.mode || d.msg.Attributes["pipelines"] != want.pipelines {
				t.Errorf("got %s %q, want %s %q", d.msg.Attributes["mode"], d.msg.Attributes["pipelines"], want.mode, want.pipelines)
			}
			if got := statusList(d.statuses); !slices.Equal(got, want.statuses) {
				t.Errorf("statuses %v, want %v", got, want.statuses)
			}
		})
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
