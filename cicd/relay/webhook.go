package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// maxBody bounds what a delivery may cost before its signature is checked.
// A push of PAYLOAD_COMMIT_LIMIT (15) commits is a few tens of KiB.
const maxBody = 2 << 20

// The modes a delivery can map to. Each is its own Pub/Sub topic and Cloud
// Build trigger, so the trigger -- not anything in the message -- decides
// whether approval is needed.
const (
	ModePreview = "preview" // a pull request: pulumi preview, image build without push
	ModeDeploy  = "deploy"  // a push to main: deploy what changed, no approval
	ModeGated   = "gated"   // a push to main that needs a person's approval first
)

// Pipelines name the repo's own Cloud Build files (cloudbuild/<name>.yaml).
// Infra always runs before image when both do.
const (
	PipelineInfra = "infra"
	PipelineImage = "image"
)

// Message is one Pub/Sub publish: the trigger binds each attribute to a
// substitution (_SHA, _REF, _PR, _PIPELINES, _MODE). Data carries nothing the
// build reads.
type Message struct {
	Topic      string
	Attributes map[string]string
}

// Publisher sends a message; *PubSub in production, a fake in tests.
type Publisher interface {
	Publish(r *http.Request, m Message) error
}

// Webhook turns a Gitea delivery into at most one Pub/Sub message.
type Webhook struct {
	Secret  []byte
	Repo    string   // owner/name; deliveries for any other repo are refused
	Authors []string // pull requests from anyone else get no preview
	Topics  map[string]string
	Pub     Publisher
	Status  StatusPoster // nil: post no statuses from here
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header) {
		// Nothing about the delivery is trusted or logged beyond this.
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-Gitea-Event")
	delivery := r.Header.Get("X-Gitea-Delivery")
	d, err := h.decide(event, body)
	if err != nil {
		slog.Warn("delivery refused", "event", event, "delivery", delivery, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if d.msg == nil {
		slog.Info("delivery ignored", "event", event, "delivery", delivery, "reason", d.reason)
		h.post(r, d.statuses)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d.msg.Attributes["delivery"] = delivery
	if err := h.Pub.Publish(r, *d.msg); err != nil {
		// 5xx, so Gitea marks the delivery failed and it can be redelivered
		// from the webhook's settings page.
		slog.Error("publish failed", "topic", d.msg.Topic, "error", err)
		http.Error(w, "publish failed", http.StatusBadGateway)
		return
	}
	slog.Info("published", "topic", d.msg.Topic, "attributes", d.msg.Attributes)
	h.post(r, d.statuses)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "%s %s\n", d.msg.Attributes["mode"], d.msg.Attributes["pipelines"])
}

// post is best effort: a status that fails to post never fails a delivery.
func (h *Webhook) post(r *http.Request, statuses []Status) {
	if h.Status == nil {
		return
	}
	for _, st := range statuses {
		if err := h.Status.Post(r.Context(), st); err != nil {
			slog.Error("posting status", "context", st.Context, "error", err)
		}
	}
}

// validSignature checks X-Gitea-Signature (hex HMAC-SHA256 of the raw body),
// or the GitHub-style X-Hub-Signature-256 Gitea also sends, in constant time.
func validSignature(secret, body []byte, hdr http.Header) bool {
	if len(secret) == 0 {
		return false // a missing secret must never mean "accept everything"
	}
	sig := hdr.Get("X-Gitea-Signature")
	if sig == "" {
		sig = strings.TrimPrefix(hdr.Get("X-Hub-Signature-256"), "sha256=")
	}
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

type decision struct {
	msg      *Message
	reason   string
	statuses []Status // posted whether or not anything is published
}

func (h *Webhook) decide(event string, body []byte) (decision, error) {
	switch event {
	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return decision{}, fmt.Errorf("push payload: %w", err)
		}
		if p.Repository.FullName != h.Repo {
			return decision{}, fmt.Errorf("delivery for %q, not %q", p.Repository.FullName, h.Repo)
		}
		return h.decidePush(p), nil
	case "pull_request":
		var p prPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return decision{}, fmt.Errorf("pull_request payload: %w", err)
		}
		if p.Repository.FullName != h.Repo {
			return decision{}, fmt.Errorf("delivery for %q, not %q", p.Repository.FullName, h.Repo)
		}
		return h.decidePR(p), nil
	default:
		// The hook subscribes to push and pull_request only; anything else
		// (a "ping" from the settings page, say) is acknowledged and dropped.
		return decision{reason: "event " + event}, nil
	}
}

const zeroSHA = "0000000000000000000000000000000000000000"

func (h *Webhook) decidePush(p pushPayload) decision {
	if p.Ref != "refs/heads/main" {
		return decision{reason: "push to " + p.Ref}
	}
	if p.After == "" || p.After == zeroSHA {
		return decision{reason: "main deleted"}
	}
	paths, complete := p.paths()
	mode, pipelines, why := Classify(paths, complete)
	drills := DrillStatuses(p.After, paths, complete)
	if len(pipelines) == 0 {
		return decision{
			reason: why,
			statuses: append([]Status{{SHA: p.After, Context: "cloudbuild/" + ModeDeploy, State: "success",
				Description: "nothing to deploy: " + why}}, drills...),
		}
	}
	return decision{statuses: drills, msg: &Message{
		Topic: h.Topics[mode],
		Attributes: map[string]string{
			"sha": p.After, "ref": "main", "pr": "",
			"mode": mode, "pipelines": strings.Join(pipelines, ","),
		},
	}}
}

func (h *Webhook) decidePR(p prPayload) decision {
	switch p.Action {
	case "opened", "synchronized", "reopened":
	default:
		return decision{reason: "pull request " + p.Action}
	}
	pr := p.PullRequest
	// Same-repo branches only: a fork's head is code nobody here reviewed,
	// and even a preview runs it (pulumi executes the program).
	if pr.Head.Repo.FullName != h.Repo {
		return decision{reason: "head is in " + pr.Head.Repo.FullName}
	}
	// Both the author and the pusher must be allowed. A preview runs the head
	// commit's code and pipeline file; on a synchronize the head is whatever
	// the pusher (p.Sender) just pushed, which need not be the PR's author, so
	// checking the author alone would let any repo writer preview their code on
	// someone else's PR branch.
	if !slices.Contains(h.Authors, pr.User.Login) {
		return decision{reason: "author " + pr.User.Login + " is not allowed"}
	}
	if !slices.Contains(h.Authors, p.Sender.Login) {
		return decision{reason: "pusher " + p.Sender.Login + " is not allowed"}
	}
	return decision{msg: &Message{
		Topic: h.Topics[ModePreview],
		Attributes: map[string]string{
			"sha": pr.Head.SHA, "ref": pr.Head.Ref, "pr": strconv.Itoa(p.Number),
			"mode": ModePreview, "pipelines": PipelineInfra + "," + PipelineImage,
		},
	}}
}

// Gated are the files whose change needs a person's approval before it
// deploys. The two version files are, by definition, a Gitea major or Alpine
// minor bump. vm/bootstrap.sh is the VM's startup script, so any edit to it
// replaces the VM (about 15 minutes down and a new Let's Encrypt certificate).
var Gated = []string{"image/gitea.version", "image/alpine.version", "vm/bootstrap.sh"}

// Classify maps the paths a push to main changed onto a mode and the
// pipelines to run. complete is false when Gitea truncated the commit list
// (PAYLOAD_COMMIT_LIMIT): the paths are then a sample, a gated file may be
// among the missing ones, so everything runs and a person approves it.
func Classify(paths []string, complete bool) (mode string, pipelines []string, reason string) {
	if !complete {
		return ModeGated, []string{PipelineInfra, PipelineImage}, "commit list truncated"
	}
	var infra, image, gated bool
	for _, p := range paths {
		switch {
		case slices.Contains(Gated, p):
			gated = true
			if strings.HasPrefix(p, "image/") {
				image = true
			} else {
				infra = true
			}
		case strings.HasPrefix(p, "infra/"), strings.HasPrefix(p, "vm/"), p == "cloudbuild/infra.yaml":
			infra = true
		case strings.HasPrefix(p, "image/"), p == "cloudbuild/image.yaml":
			image = true
		case strings.HasPrefix(p, "scripts/cicd/"):
			// scripts/cicd/ drives a deploy (the infra deploy driver, the
			// shared release gate), so a change there deploys through the
			// pipeline(s) that run the file. ReleasePaths says which, and the
			// release gate then makes that deploy wait for the file's drill.
			// Files there that drive nothing (bootstrap.py, tests) are not in
			// ReleasePaths, so they deploy nothing.
			for _, pl := range ReleasePaths[p] {
				switch pl {
				case PipelineInfra:
					infra = true
				case PipelineImage:
					image = true
				}
			}
		}
	}
	if infra {
		pipelines = append(pipelines, PipelineInfra)
	}
	if image {
		pipelines = append(pipelines, PipelineImage)
	}
	switch {
	case len(pipelines) == 0:
		return ModeDeploy, nil, "no infra/, vm/, image/, cloudbuild/ or scripts/cicd/ change"
	case gated:
		return ModeGated, pipelines, "gated file changed"
	default:
		return ModeDeploy, pipelines, ""
	}
}

// ReleasePaths are the files whose code decides what a release or a rollback
// does, by the pipeline that runs them. A change is live at the next release
// -- the unattended weekly rebuild, once automation is on -- so it must pass
// that pipeline's drills first, and scripts/cicd/release_gate.py refuses an
// automatic run until it has. Keep in step with that script's FILES: the
// engine is every file its ENGINE names, one line each.
var ReleasePaths = map[string][]string{
	"cloudbuild/image.yaml":                               {PipelineImage},
	"cloudbuild/infra.yaml":                               {PipelineInfra},
	"scripts/cicd/infra_deploy.py":                        {PipelineInfra},
	"scripts/cicd/release_gate.py":                        {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/ansible.cfg":                     {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/files/gitea-ops-lib.sh":          {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/ops.yml":                         {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/requirements.in":                 {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/requirements.txt":                {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/auto-update.yml":           {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/config-tarball.yml":        {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/dump.yml":                  {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/inventory.yml":             {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/journal-errors.yml":        {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/marker-check.yml":          {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/marker.yml":                {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/migrate.yml":               {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/pause-timers.yml":          {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/pre-restore-list.yml":      {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/pre-restore-remove.yml":    {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/pull.yml":                  {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/restart-if-not.yml":        {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/restarts.yml":              {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/restore-sync.yml":          {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/restore-test-dump.yml":     {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/restore-test-snapshot.yml": {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/resume-timers.yml":         {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/running-digest.yml":        {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/selftest.yml":              {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/start.yml":                 {PipelineImage, PipelineInfra},
	"scripts/ops/ansible/steps/stop.yml":                  {PipelineImage, PipelineInfra},
	"scripts/ops/gitea_ops.py":                            {PipelineImage, PipelineInfra},
	"scripts/ops/launch_job.py":                           {PipelineImage, PipelineInfra},
}

// Drills each pipeline's release code must pass (scripts/cicd/release_gate.py).
var Drills = map[string][]string{
	PipelineImage: {"fail-verify", "fail-after-migrate"},
	PipelineInfra: {"fail-verify"},
}

// DrillStatuses marks a push that changed release code with one pending
// status per drill it now needs, instead of letting a green "nothing to
// deploy" suggest it is done. A passing drill build turns its own one green.
func DrillStatuses(sha string, paths []string, complete bool) []Status {
	need := map[string]bool{}
	if !complete {
		need[PipelineImage], need[PipelineInfra] = true, true
	}
	for _, p := range paths {
		for _, pl := range ReleasePaths[p] {
			need[pl] = true
		}
	}
	var out []Status
	for _, pl := range []string{PipelineImage, PipelineInfra} {
		if !need[pl] {
			continue
		}
		for _, d := range Drills[pl] {
			out = append(out, Status{SHA: sha, Context: "cloudbuild/drill-" + pl + "-" + d, State: "pending",
				Description: "release code changed: make ci-drill PIPELINE=" + pl + " NAME=" + d})
		}
	}
	return out
}

// The parts of Gitea's webhook payloads (modules/structs/hook.go) read here.
type repoRef struct {
	FullName string `json:"full_name"`
}

type pushPayload struct {
	Ref          string   `json:"ref"`
	After        string   `json:"after"`
	TotalCommits int      `json:"total_commits"`
	Commits      []commit `json:"commits"`
	Repository   repoRef  `json:"repository"`
	HeadCommit   *commit  `json:"head_commit"`
}

type commit struct {
	ID       string   `json:"id"`
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
}

// paths is every file the listed commits touched, and whether the list is the
// whole push.
func (p pushPayload) paths() ([]string, bool) {
	var out []string
	for _, c := range p.Commits {
		out = append(append(append(out, c.Added...), c.Removed...), c.Modified...)
	}
	return out, p.TotalCommits <= len(p.Commits)
}

type prPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref  string  `json:"ref"`
			SHA  string  `json:"sha"`
			Repo repoRef `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository repoRef `json:"repository"`
	// Sender is who delivered this event -- for a synchronize, the pusher.
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

var errNoSecret = errors.New("GITEA_WEBHOOK_SECRET is empty")
