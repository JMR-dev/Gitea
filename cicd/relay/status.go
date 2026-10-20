package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tags the pipeline puts on its builds (package triggers in cicd/, and the
// launcher's `gcloud builds submit --tags`). Builds without one are not ours
// -- a Cloud Run functions deploy builds in this project too -- and are
// ignored.
const (
	TagLaunch   = "gitea-launch"   // the trigger's own build: clones and submits
	TagPipeline = "gitea-pipeline" // a cloudbuild/<pipeline>.yaml build it submitted
	// PipelineTagPrefix + infra|image names which one.
	PipelineTagPrefix = "pipeline-"
)

// Status is one Gitea commit status.
type Status struct {
	SHA         string
	Context     string
	State       string // pending, success, error, failure
	Description string
	TargetURL   string
}

// StatusPoster posts commit statuses; *Gitea in production.
type StatusPoster interface {
	Post(ctx context.Context, s Status) error
}

// build is the part of a Cloud Build Build resource read here, as published
// on the cloud-builds topic.
type build struct {
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	Substitutions map[string]string `json:"substitutions"`
	Tags          []string          `json:"tags"`
	LogURL        string            `json:"logUrl"`
	FailureInfo   *struct {
		Detail string `json:"detail"`
	} `json:"failureInfo"`
	Approval *struct {
		State  string `json:"state"`
		Result *struct {
			Decision string `json:"decision"`
		} `json:"result"`
	} `json:"approval"`
}

// pushEnvelope is what a Pub/Sub push subscription POSTs.
type pushEnvelope struct {
	Message struct {
		Data       []byte            `json:"data"`
		Attributes map[string]string `json:"attributes"`
		MessageID  string            `json:"messageId"`
	} `json:"message"`
}

// FailureMarker starts the log line written for every pipeline build that
// fails; a log-based metric in cicd/pkg/alerting counts it.
const FailureMarker = "GITEA_PIPELINE_FAILED"

// StatusFor maps a build onto the commit status it should produce, or ok=false
// for a build that is not ours or carries no commit.
func StatusFor(b build) (Status, bool) {
	sha := b.Substitutions["_SHA"]
	if sha == "" {
		return Status{}, false
	}
	var ctxName string
	for _, t := range b.Tags {
		switch {
		case t == TagLaunch:
			if ctxName == "" {
				ctxName = "cloudbuild/" + b.Substitutions["_MODE"]
			}
		case strings.HasPrefix(t, PipelineTagPrefix):
			ctxName = "cloudbuild/" + strings.TrimPrefix(t, PipelineTagPrefix)
		}
	}
	if ctxName == "" || ctxName == "cloudbuild/" {
		return Status{}, false
	}
	s := Status{SHA: sha, Context: ctxName, TargetURL: b.LogURL}
	short := b.ID
	if len(short) > 8 {
		short = short[:8]
	}
	switch b.Status {
	case "PENDING":
		s.State, s.Description = "pending", "awaiting approval: make approve"
		if rejected(b) {
			s.State, s.Description = "error", "approval rejected"
		}
	case "QUEUED":
		s.State, s.Description = "pending", "queued"
	case "WORKING":
		s.State, s.Description = "pending", "running"
	case "SUCCESS":
		s.State, s.Description = "success", "passed"
	case "FAILURE", "TIMEOUT", "INTERNAL_ERROR":
		s.State, s.Description = "failure", strings.ToLower(b.Status)
		if b.FailureInfo != nil && b.FailureInfo.Detail != "" {
			s.Description += ": " + b.FailureInfo.Detail
		}
	case "CANCELLED", "EXPIRED":
		s.State, s.Description = "error", strings.ToLower(b.Status)
		if rejected(b) {
			s.Description = "approval rejected"
		}
	default:
		return Status{}, false
	}
	// The build id makes a status traceable, and lets a late, out-of-order
	// "running" be told apart from a newer build (see BuildStatus.ServeHTTP).
	s.Description = truncate(s.Description, 120) + " (build " + short + ")"
	return s, true
}

func rejected(b build) bool {
	return b.Approval != nil && (b.Approval.State == "REJECTED" ||
		b.Approval.Result != nil && b.Approval.Result.Decision == "REJECTED")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func terminal(state string) bool { return state != "pending" }

// BuildStatus receives the cloud-builds topic through a push subscription.
// Cloud Run has already checked the push's OIDC token before this runs: the
// function is private and only the push identity may invoke it.
type BuildStatus struct {
	Gitea interface {
		StatusPoster
		Latest(ctx context.Context, sha, context string) (Status, bool, error)
	}
}

func (h *BuildStatus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var env pushEnvelope
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&env); err != nil {
		// A malformed push will never decode; acknowledge so it is not retried.
		slog.Error("push envelope", "error", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var b build
	if err := json.Unmarshal(env.Message.Data, &b); err != nil {
		slog.Error("build JSON", "message", env.Message.MessageID, "error", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s, ok := StatusFor(b)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.State == "failure" {
		slog.Error(FailureMarker, "build", b.ID, "context", s.Context, "sha", s.SHA, "status", b.Status)
	}
	if !terminal(s.State) {
		// Pub/Sub does not keep order. A "running" delivered after the same
		// build's result must not turn a red or green check back to yellow.
		cur, found, err := h.Gitea.Latest(r.Context(), s.SHA, s.Context)
		if err != nil {
			slog.Error("reading statuses", "error", err)
			http.Error(w, "gitea unavailable", http.StatusServiceUnavailable)
			return
		}
		if found && terminal(cur.State) && sameBuild(cur.Description, s.Description) {
			slog.Info("stale event skipped", "build", b.ID, "status", b.Status, "current", cur.State)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if err := h.Gitea.Post(r.Context(), s); err != nil {
		var he *httpError
		if asHTTPError(err, &he) && he.code >= 400 && he.code < 500 {
			// Retrying a 4xx (a bad token, an unknown commit) changes nothing.
			slog.Error("status refused", "code", he.code, "error", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		slog.Error("posting status", "error", err)
		http.Error(w, "gitea unavailable", http.StatusServiceUnavailable) // Pub/Sub retries
		return
	}
	slog.Info("status posted", "sha", s.SHA, "context", s.Context, "state", s.State, "build", b.ID)
	w.WriteHeader(http.StatusNoContent)
}

func sameBuild(a, b string) bool {
	i, j := strings.LastIndex(a, "(build "), strings.LastIndex(b, "(build ")
	return i >= 0 && j >= 0 && a[i:] == b[j:]
}

// Gitea is the slice of Gitea's API used here: commit statuses on one repo.
type Gitea struct {
	BaseURL string // https://gitea.example
	Repo    string // owner/name
	Token   string // write:repository
	Client  *http.Client
}

type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.body) }

func asHTTPError(err error, target **httpError) bool {
	he, ok := err.(*httpError)
	if ok {
		*target = he
	}
	return ok
}

func (g *Gitea) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (g *Gitea) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.BaseURL, "/")+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+g.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (g *Gitea) Post(ctx context.Context, s Status) error {
	return g.do(ctx, http.MethodPost, "/repos/"+g.Repo+"/statuses/"+url.PathEscape(s.SHA), map[string]string{
		"state": s.State, "context": s.Context, "description": s.Description, "target_url": s.TargetURL,
	}, nil)
}

// Latest is the newest status for one context on a commit.
func (g *Gitea) Latest(ctx context.Context, sha, context string) (Status, bool, error) {
	var list []struct {
		Status      string `json:"status"`
		Context     string `json:"context"`
		Description string `json:"description"`
	}
	// Newest first; one page is plenty for the handful of contexts we post.
	q := "/repos/" + g.Repo + "/commits/" + url.PathEscape(sha) + "/statuses?sort=recentupdate&limit=50"
	if err := g.do(ctx, http.MethodGet, q, nil, &list); err != nil {
		return Status{}, false, err
	}
	for _, s := range list {
		if s.Context == context {
			return Status{SHA: sha, Context: s.Context, State: s.Status, Description: s.Description}, true, nil
		}
	}
	return Status{}, false, nil
}
