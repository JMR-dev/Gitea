package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// PubSub publishes through the Pub/Sub REST API as the function's runtime
// identity. One request per delivery does not need the client library's
// batching, and this keeps the function's dependencies to two modules.
type PubSub struct {
	once sync.Once
	ts   oauth2.TokenSource
	err  error
}

func (p *PubSub) Publish(r *http.Request, m Message) error {
	if m.Topic == "" {
		return fmt.Errorf("no topic configured for mode %q", m.Attributes["mode"])
	}
	p.once.Do(func() {
		p.ts, p.err = google.DefaultTokenSource(r.Context(), "https://www.googleapis.com/auth/pubsub")
	})
	if p.err != nil {
		return p.err
	}
	tok, err := p.ts.Token()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"messages": []map[string]any{{
		// Pub/Sub refuses a message with neither data nor attributes; the
		// attributes are what the trigger reads.
		"data":       []byte(m.Attributes["mode"]),
		"attributes": m.Attributes,
	}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"https://pubsub.googleapis.com/v1/"+m.Topic+":publish", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	tok.SetAuthHeader(req)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("publish to %s: HTTP %d: %s", m.Topic, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
