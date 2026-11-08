package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const testSHA = "26de5015d2ded3c765a798d354b8a56abe0a98a7"

func buildJSON(t *testing.T, b map[string]any) []byte {
	t.Helper()
	data, _ := json.Marshal(b)
	env, _ := json.Marshal(map[string]any{"message": map[string]any{"data": data, "messageId": "m1"}})
	return env
}

func pipelineBuild(status string) map[string]any {
	return map[string]any{
		"id": "0123456789abcdef", "status": status, "logUrl": "https://console.cloud.google.com/cloud-build/builds;region=us-east1/0123456789abcdef",
		"substitutions": map[string]any{"_SHA": "26de5015d2ded3c765a798d354b8a56abe0a98a7", "_MODE": "deploy"},
		"tags":          []any{TagPipeline, PipelineTagPrefix + "infra", "sha-26de501"},
	}
}

func TestStatusFor(t *testing.T) {
	for _, tc := range []struct {
		status, want string
	}{
		{"QUEUED", "pending"}, {"WORKING", "pending"}, {"PENDING", "pending"},
		{"SUCCESS", "success"},
		{"FAILURE", "failure"}, {"TIMEOUT", "failure"}, {"INTERNAL_ERROR", "failure"},
		{"CANCELLED", "error"}, {"EXPIRED", "error"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			raw, _ := json.Marshal(pipelineBuild(tc.status))
			var b build
			_ = json.Unmarshal(raw, &b)
			s, ok := StatusFor(b)
			if !ok || s.State != tc.want || s.Context != "cloudbuild/infra" ||
				!strings.HasSuffix(s.Description, "(build 01234567)") || !strings.HasPrefix(s.TargetURL, "https://console") {
				t.Fatalf("got %+v ok=%v", s, ok)
			}
		})
	}
}

func TestStatusForLauncherAndApproval(t *testing.T) {
	b := build{ID: "abc", Status: "PENDING",
		Substitutions: map[string]string{"_SHA": testSHA, "_MODE": "gated"}, Tags: []string{TagLaunch}}
	s, ok := StatusFor(b)
	if !ok || s.Context != "cloudbuild/gated" || s.State != "pending" || !strings.Contains(s.Description, "awaiting approval") {
		t.Fatalf("got %+v", s)
	}
	raw := []byte(`{"id":"abc","status":"CANCELLED","substitutions":{"_SHA":"` + testSHA + `","_MODE":"gated"},"tags":["gitea-launch"],
		"approval":{"state":"REJECTED","result":{"decision":"REJECTED"}}}`)
	_ = json.Unmarshal(raw, &b)
	if s, _ := StatusFor(b); s.State != "error" || !strings.Contains(s.Description, "rejected") {
		t.Fatalf("got %+v", s)
	}
}

// A drill build must report under the same context the relay's reminder
// (DrillStatuses) uses, so a passing drill clears its own pending status
// instead of leaving the commit yellow forever.
func TestStatusForDrillMatchesReminder(t *testing.T) {
	for _, tc := range []struct{ pipeline, drill string }{
		{PipelineInfra, "fail-verify"},
		{PipelineImage, "fail-verify"},
		{PipelineImage, "fail-after-migrate"},
	} {
		b := build{ID: "abc", Status: "SUCCESS", Tags: []string{TagPipeline, PipelineTagPrefix + tc.pipeline},
			Substitutions: map[string]string{"_SHA": testSHA, "_DRILL": tc.drill}}
		s, ok := StatusFor(b)
		want := "cloudbuild/drill-" + tc.pipeline + "-" + tc.drill
		if !ok || s.Context != want || s.State != "success" {
			t.Fatalf("%s/%s: got %+v, want context %q success", tc.pipeline, tc.drill, s, want)
		}
		// The reminder the relay posts for this pipeline must include that context.
		found := false
		for _, r := range DrillStatuses(testSHA, []string{"cloudbuild/" + tc.pipeline + ".yaml"}, true) {
			if r.Context == s.Context {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s/%s: drill context %q matches no DrillStatuses reminder", tc.pipeline, tc.drill, s.Context)
		}
	}
}

func TestStatusForIgnoresForeignBuilds(t *testing.T) {
	for name, b := range map[string]build{
		"untagged":            {ID: "x", Status: "SUCCESS", Substitutions: map[string]string{"_SHA": "s"}},
		"no sha":              {ID: "x", Status: "SUCCESS", Tags: []string{TagPipeline, "pipeline-image"}},
		"functions":           {ID: "x", Status: "SUCCESS", Tags: []string{"gcp-cloud-functions"}},
		"unknown st":          {ID: "x", Status: "STATUS_UNKNOWN", Substitutions: map[string]string{"_SHA": testSHA}, Tags: []string{TagLaunch}},
		"rebuild placeholder": {ID: "x", Status: "WORKING", Substitutions: map[string]string{"_SHA": "main", "_MODE": "rebuild"}, Tags: []string{TagLaunch}},
		"short sha":           {ID: "x", Status: "SUCCESS", Substitutions: map[string]string{"_SHA": "26de501"}, Tags: []string{TagLaunch}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := StatusFor(b); ok {
				t.Fatal("should be ignored")
			}
		})
	}
}

type fakeGitea struct {
	posted []Status
	latest *Status
	err    error
}

func (f *fakeGitea) Post(_ context.Context, s Status) error {
	if f.err != nil {
		return f.err
	}
	f.posted = append(f.posted, s)
	return nil
}

func (f *fakeGitea) Latest(context.Context, string, string) (Status, bool, error) {
	if f.latest == nil {
		return Status{}, false, nil
	}
	return *f.latest, true, nil
}

func push(t *testing.T, h *BuildStatus, body []byte) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	return rec.Code
}

func TestBuildStatusPosts(t *testing.T) {
	g := &fakeGitea{}
	if code := push(t, &BuildStatus{Gitea: g}, buildJSON(t, pipelineBuild("SUCCESS"))); code != http.StatusNoContent {
		t.Fatalf("code %d", code)
	}
	if len(g.posted) != 1 || g.posted[0].State != "success" {
		t.Fatalf("posted %+v", g.posted)
	}
}

func TestBuildStatusSkipsStaleRunning(t *testing.T) {
	done := Status{State: "success", Description: "passed (build 01234567)"}
	g := &fakeGitea{latest: &done}
	push(t, &BuildStatus{Gitea: g}, buildJSON(t, pipelineBuild("WORKING")))
	if len(g.posted) != 0 {
		t.Fatalf("a late WORKING overwrote the result: %+v", g.posted)
	}
	// A newer build of the same commit does move the check back to pending.
	other := Status{State: "failure", Description: "failure (build 99999999)"}
	g = &fakeGitea{latest: &other}
	push(t, &BuildStatus{Gitea: g}, buildJSON(t, pipelineBuild("WORKING")))
	if len(g.posted) != 1 || g.posted[0].State != "pending" {
		t.Fatalf("posted %+v", g.posted)
	}
}

func TestBuildStatusRetriesOnlyServerErrors(t *testing.T) {
	g := &fakeGitea{err: &httpError{code: 502}}
	if code := push(t, &BuildStatus{Gitea: g}, buildJSON(t, pipelineBuild("SUCCESS"))); code != http.StatusServiceUnavailable {
		t.Fatalf("5xx should be retried, code %d", code)
	}
	g = &fakeGitea{err: &httpError{code: 401}}
	if code := push(t, &BuildStatus{Gitea: g}, buildJSON(t, pipelineBuild("SUCCESS"))); code != http.StatusNoContent {
		t.Fatalf("4xx should be acknowledged, code %d", code)
	}
}

func TestBuildStatusAcksGarbage(t *testing.T) {
	g := &fakeGitea{}
	for _, body := range [][]byte{[]byte("not json"), []byte(`{"message":{"data":"bm90IGpzb24="}}`)} {
		if code := push(t, &BuildStatus{Gitea: g}, body); code != http.StatusNoContent {
			t.Fatalf("code %d", code)
		}
	}
	if len(g.posted) != 0 {
		t.Fatal("posted for garbage")
	}
}

func TestGiteaClient(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"status":"failure","context":"cloudbuild/image","description":"x (build 1)"},
				{"status":"success","context":"cloudbuild/infra","description":"passed (build 2)"}]`))
			return
		}
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		gotBody = b.String()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	g := &Gitea{BaseURL: srv.URL + "/", Repo: "JMR-dev/Gitea", Token: "t0k"}
	if err := g.Post(context.Background(), Status{SHA: "abc", Context: "cloudbuild/infra", State: "success", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "token t0k" || gotPath != "/api/v1/repos/JMR-dev/Gitea/statuses/abc" || !strings.Contains(gotBody, `"context":"cloudbuild/infra"`) {
		t.Fatalf("auth %q path %q body %q", gotAuth, gotPath, gotBody)
	}
	s, found, err := g.Latest(context.Background(), "abc", "cloudbuild/infra")
	if err != nil || !found || s.State != "success" {
		t.Fatalf("latest %+v %v %v", s, found, err)
	}
}

func TestGiteaFirstParentFiles(t *testing.T) {
	const sha = "96776d039aca88cf58482966a72ca2ab7673c884"
	answer := fixture(t, "gitea_commit_96776d0.json")
	var gotPath, gotAuth string
	var hasAuth bool
	var reply func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.RequestURI()
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		reply(w)
	}))
	defer srv.Close()
	serve := func(code int, body []byte) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { w.WriteHeader(code); _, _ = w.Write(body) }
	}

	t.Run("answer", func(t *testing.T) {
		reply = serve(http.StatusOK, answer)
		g := &Gitea{BaseURL: srv.URL + "/", Repo: "JMR-dev/Gitea", Token: "t0k"}
		parents, files, err := g.FirstParentFiles(context.Background(), sha)
		if err != nil {
			t.Fatal(err)
		}
		if gotPath != "/api/v1/repos/JMR-dev/Gitea/git/commits/"+sha+"?stat=false&verification=false&files=true" ||
			gotAuth != "token t0k" {
			t.Fatalf("path %q auth %q", gotPath, gotAuth)
		}
		if len(parents) != 2 || parents[0] != "6fad9f8cb3e832eb460cd6ddc4add853247e27aa" ||
			parents[1] != "7ff334149cdf54c8ca9dbaba8364d8bfc08417c4" {
			t.Fatalf("parents %v", parents)
		}
		if len(files) != 20 || !slices.Contains(files, "vm/nftables/gitea.nft") || !slices.Contains(files, "scripts/bats/bootstrap.bats") {
			t.Fatalf("%d files %v", len(files), files)
		}
	})
	t.Run("anonymous", func(t *testing.T) {
		reply = serve(http.StatusOK, answer)
		g := &Gitea{BaseURL: srv.URL, Repo: "JMR-dev/Gitea"}
		if _, _, err := g.FirstParentFiles(context.Background(), sha); err != nil {
			t.Fatal(err)
		}
		if hasAuth {
			t.Fatalf("sent Authorization %q without a token", gotAuth)
		}
	})
	for name, r := range map[string]func(http.ResponseWriter){
		"not found": serve(http.StatusNotFound, []byte(`{"message":"sha not found"}`)),
		"bad json":  serve(http.StatusOK, []byte(`{"sha":`)),
		// Without a file list, "no files" would read as "nothing to deploy".
		"no files":      serve(http.StatusOK, []byte(`{"sha":"`+sha+`","parents":[{"sha":"6fad9f8cb3e832eb460cd6ddc4add853247e27aa"}]}`)),
		"null files":    serve(http.StatusOK, []byte(`{"sha":"`+sha+`","parents":[],"files":null}`)),
		"other commit":  serve(http.StatusOK, []byte(strings.Replace(string(answer), sha, "0123456789012345678901234567890123456789", 1))),
		"server failed": serve(http.StatusBadGateway, []byte("bad gateway")),
	} {
		t.Run(name, func(t *testing.T) {
			reply = r
			g := &Gitea{BaseURL: srv.URL, Repo: "JMR-dev/Gitea", Token: "t0k"}
			if parents, files, err := g.FirstParentFiles(context.Background(), sha); err == nil {
				t.Fatalf("no error; parents %v files %v", parents, files)
			}
		})
	}
}
