// Package relay is the two Cloud Run functions between Gitea and Cloud Build.
//
//   - GiteaWebhook (public): checks a Gitea delivery's HMAC signature and turns
//     it into one Pub/Sub message on the topic for its mode -- ci-preview,
//     ci-deploy or ci-gated -- whose Cloud Build trigger does the work. The
//     policy (what needs approval, what deploys) lives in Classify.
//   - BuildStatus (private): receives the cloud-builds topic through an
//     authenticated push subscription and posts each pipeline build's state to
//     Gitea as a commit status.
//
// Neither ever sees a Google credential beyond its own runtime identity, and
// neither holds anything Gitea could not revoke: the webhook secret and a
// write:repository token, both injected from Secret Manager as environment
// variables.
package relay

import (
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
)

func init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	functions.HTTP("GiteaWebhook", serveWebhook)
	functions.HTTP("BuildStatus", serveBuildStatus)
}

func gitea() *Gitea {
	return &Gitea{
		BaseURL: os.Getenv("GITEA_URL"),
		Repo:    os.Getenv("GITEA_REPO"),
		Token:   strings.TrimSpace(os.Getenv("GITEA_STATUS_TOKEN")),
	}
}

func serveWebhook(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimSpace(os.Getenv("GITEA_WEBHOOK_SECRET"))
	if secret == "" {
		slog.Error("refusing every delivery", "error", errNoSecret)
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	var status StatusPoster
	if g := gitea(); g.Token != "" {
		status = g
	}
	h := &Webhook{
		Secret:  []byte(secret),
		Repo:    os.Getenv("GITEA_REPO"),
		Authors: strings.Fields(strings.ReplaceAll(os.Getenv("ALLOWED_AUTHORS"), ",", " ")),
		Topics: map[string]string{
			ModePreview: os.Getenv("TOPIC_PREVIEW"),
			ModeDeploy:  os.Getenv("TOPIC_DEPLOY"),
			ModeGated:   os.Getenv("TOPIC_GATED"),
		},
		Pub:    &PubSub{},
		Status: status,
	}
	h.ServeHTTP(w, r)
}

func serveBuildStatus(w http.ResponseWriter, r *http.Request) {
	(&BuildStatus{Gitea: gitea()}).ServeHTTP(w, r)
}
