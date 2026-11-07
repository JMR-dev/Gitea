package alerting

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"

	"gitea-cicd/pkg/config"
)

// What the mock hands back as the channel's name and the check's ID, as
// Google would after creating them.
const (
	testChannel = "projects/p/notificationChannels/1234567890"
	testCheckID = "gitea-https-a1b2c3d4"
)

// recorder is a Pulumi mock that keeps what the program registers, by
// resource name: the alert policies all share one type.
type recorder struct {
	mu  sync.Mutex
	got map[string]resource.PropertyMap
}

func (r *recorder) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[a.Name] = a.Inputs
	state := a.Inputs.Copy()
	switch a.TypeToken {
	case "gcp:monitoring/uptimeCheckConfig:UptimeCheckConfig":
		state["uptimeCheckId"] = resource.NewStringProperty(testCheckID)
	case "gcp:monitoring/notificationChannel:NotificationChannel":
		state["name"] = resource.NewStringProperty(testChannel)
	}
	return a.Name + "-id", state, nil
}

func (r *recorder) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

func testConfig(days int) *config.Config {
	return &config.Config{
		Project: "p", GiteaURL: "https://gitea.example.test",
		OperatorEmail: "op@example.com", CertAlertDays: days,
	}
}

// run registers the alerting resources against the mock and returns them,
// by name, with the check ID New returned (what main.go exports).
func run(t *testing.T, cfg *config.Config) (map[string]resource.PropertyMap, string) {
	t.Helper()
	r := &recorder{got: map[string]resource.PropertyMap{}}
	var checkID string
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		id, err := New(ctx, cfg, nil)
		if err != nil {
			return err
		}
		res, err := internals.UnsafeAwaitOutput(ctx.Context(), id)
		if err != nil {
			return err
		}
		checkID, _ = res.Value.(string)
		return nil
	}, pulumi.WithMocks("p", "s", r))
	if err != nil {
		t.Fatal(err)
	}
	return r.got, checkID
}

func resourceNamed(t *testing.T, got map[string]resource.PropertyMap, name string) resource.PropertyMap {
	t.Helper()
	m, ok := got[name]
	if !ok {
		t.Fatalf("no resource %q; created: %v", name, keys(got))
	}
	return m
}

func keys(m map[string]resource.PropertyMap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func str(m resource.PropertyMap, key string) string {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsString() {
		return ""
	}
	return v.StringValue()
}

func num(m resource.PropertyMap, key string) float64 {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsNumber() {
		return -1
	}
	return v.NumberValue()
}

func boolean(m resource.PropertyMap, key string) bool {
	v, ok := m[resource.PropertyKey(key)]
	return ok && v.IsBool() && v.BoolValue()
}

func obj(m resource.PropertyMap, key string) resource.PropertyMap {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsObject() {
		return resource.PropertyMap{}
	}
	return v.ObjectValue()
}

func arr(m resource.PropertyMap, key string) []resource.PropertyValue {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsArray() {
		return nil
	}
	return v.ArrayValue()
}

func strs(m resource.PropertyMap, key string) []string {
	var out []string
	for _, v := range arr(m, key) {
		out = append(out, v.StringValue())
	}
	return out
}

// The one condition of a policy.
func condition(t *testing.T, policy resource.PropertyMap) resource.PropertyMap {
	t.Helper()
	conds := arr(policy, "conditions")
	if len(conds) != 1 || !conds[0].IsObject() {
		t.Fatalf("want exactly one condition, got %v", policy["conditions"])
	}
	return conds[0].ObjectValue()
}

// The three log-match alerts are as they were: this change only adds.
func TestLogAlertsUnchanged(t *testing.T) {
	got, _ := run(t, testConfig(18))
	for name, want := range map[string]struct{ severity, filter string }{
		"rollback-failed": {"CRITICAL",
			`resource.type="build" AND (textPayload:"GITEA_RELEASE_ROLLBACK_FAILED" OR textPayload:"GITEA_INFRA_ROLLBACK_FAILED")`},
		"rolled-back": {"WARNING",
			`resource.type="build" AND (textPayload:"GITEA_RELEASE_ROLLBACK path=" OR textPayload:"GITEA_INFRA_ROLLBACK sha=")`},
		"pipeline-failed": {"WARNING",
			`resource.type="cloud_run_revision" AND resource.labels.service_name="build-status" AND jsonPayload.msg="GITEA_PIPELINE_FAILED"`},
	} {
		p := resourceNamed(t, got, name)
		if s := str(p, "severity"); s != want.severity {
			t.Errorf("%s: severity %q, want %q", name, s, want.severity)
		}
		if f := str(obj(condition(t, p), "conditionMatchedLog"), "filter"); f != want.filter {
			t.Errorf("%s: filter\n got %s\nwant %s", name, f, want.filter)
		}
		strategy := obj(p, "alertStrategy")
		if period := str(obj(strategy, "notificationRateLimit"), "period"); period != "300s" {
			t.Errorf("%s: rate limit %q, want 300s", name, period)
		}
		if c := strs(p, "notificationChannels"); !slices.Equal(c, []string{testChannel}) {
			t.Errorf("%s: channels %v", name, c)
		}
	}
}

func TestUptimeCheckProbesGiteaOverValidatedTLS(t *testing.T) {
	got, checkID := run(t, testConfig(18))
	c := resourceNamed(t, got, "gitea-https")

	if p := str(c, "project"); p != "p" {
		t.Errorf("project %q", p)
	}
	mr := obj(c, "monitoredResource")
	labels := obj(mr, "labels")
	if str(mr, "type") != "uptime_url" || len(labels) != 2 ||
		str(labels, "host") != "gitea.example.test" || str(labels, "project_id") != "p" {
		t.Errorf("monitoredResource: %v", mr)
	}
	h := obj(c, "httpCheck")
	if !boolean(h, "useSsl") || !boolean(h, "validateSsl") || num(h, "port") != 443 ||
		str(h, "path") != "/api/healthz" || str(h, "requestMethod") != "GET" {
		t.Errorf("httpCheck: %v", h)
	}
	if str(c, "period") != "900s" || str(c, "timeout") != "10s" || str(c, "checkerType") != "STATIC_IP_CHECKERS" {
		t.Errorf("period %q, timeout %q, checkerType %q", str(c, "period"), str(c, "timeout"), str(c, "checkerType"))
	}
	if c.HasValue("selectedRegions") {
		t.Errorf("selectedRegions set (%v); unset is every region", c["selectedRegions"])
	}
	if checkID != testCheckID {
		t.Errorf("New returned check ID %q, want %q (main.go exports it as certCheckId)", checkID, testCheckID)
	}
}

func TestCertPolicy(t *testing.T) {
	got, _ := run(t, testConfig(18))
	p := resourceNamed(t, got, "cert-not-renewing")

	if d := str(p, "displayName"); d != "Gitea: TLS certificate not renewing (or HTTPS unreachable)" {
		t.Errorf("displayName %q", d)
	}
	if str(p, "severity") != "CRITICAL" || str(p, "combiner") != "OR" || str(p, "project") != "p" {
		t.Errorf("severity %q, combiner %q, project %q", str(p, "severity"), str(p, "combiner"), str(p, "project"))
	}

	th := obj(condition(t, p), "conditionThreshold")
	wantFilter := `metric.type="monitoring.googleapis.com/uptime_check/time_until_ssl_cert_expires" AND ` +
		`resource.type="uptime_url" AND metric.labels.check_id="` + testCheckID + `"`
	if f := str(th, "filter"); f != wantFilter {
		t.Errorf("filter\n got %s\nwant %s", f, wantFilter)
	}
	aggs := arr(th, "aggregations")
	if len(aggs) != 1 || !aggs[0].IsObject() {
		t.Fatalf("aggregations: %v", th["aggregations"])
	}
	a := aggs[0].ObjectValue()
	if str(a, "alignmentPeriod") != "1800s" || str(a, "perSeriesAligner") != "ALIGN_NEXT_OLDER" ||
		str(a, "crossSeriesReducer") != "REDUCE_MIN" ||
		!slices.Equal(strs(a, "groupByFields"), []string{"metric.label.check_id"}) {
		t.Errorf("aggregation: %v", a)
	}
	if str(th, "comparison") != "COMPARISON_LT" || num(th, "thresholdValue") != 18 ||
		str(th, "duration") != "3600s" || num(obj(th, "trigger"), "count") != 1 {
		t.Errorf("comparison %q, threshold %v, duration %q, trigger %v",
			str(th, "comparison"), num(th, "thresholdValue"), str(th, "duration"), th["trigger"])
	}
	// No reading for an hour is a dead VM, a refused handshake or a deleted
	// check: that fires too.
	if m := str(th, "evaluationMissingData"); m != "EVALUATION_MISSING_DATA_ACTIVE" {
		t.Errorf("evaluationMissingData %q", m)
	}

	if c := strs(p, "notificationChannels"); !slices.Equal(c, []string{testChannel}) {
		t.Errorf("channels %v", c)
	}
	s := obj(p, "alertStrategy")
	if s.HasValue("notificationRateLimit") {
		t.Errorf("notificationRateLimit set (%v): it is for log-match alerts only", s["notificationRateLimit"])
	}
	if ac := str(s, "autoClose"); ac != "604800s" {
		t.Errorf("autoClose %q", ac)
	}
	ncs := arr(s, "notificationChannelStrategies")
	if len(ncs) != 1 || !ncs[0].IsObject() {
		t.Fatalf("notificationChannelStrategies: %v", s["notificationChannelStrategies"])
	}
	nc := ncs[0].ObjectValue()
	if !slices.Equal(strs(nc, "notificationChannelNames"), []string{testChannel}) || str(nc, "renotifyInterval") != "86400s" {
		t.Errorf("notificationChannelStrategy: %v", nc)
	}

	doc := obj(p, "documentation")
	content := str(doc, "content")
	if str(doc, "mimeType") != "text/markdown" || len([]rune(content)) > 8192 {
		t.Errorf("documentation: mimeType %q, %d characters (8192 at most)", str(doc, "mimeType"), len([]rune(content)))
	}
	// Unreachable needs action in minutes, so it comes first.
	unreachable, renewing := strings.Index(content, "HTTPS unreachable from outside"), strings.Index(content, "Not renewing")
	if unreachable < 0 || renewing < 0 || unreachable > renewing {
		t.Errorf("documentation must explain unreachable (at %d) before not renewing (at %d):\n%s", unreachable, renewing, content)
	}
	for _, want := range []string{
		// What it is not: with Caddy serving TLS, Gitea down may send nothing.
		"not an outage alert",
		"Gitea itself is down",
		// Right on any VM.
		`sudo journalctl CONTAINER_NAME=caddy | grep -E '"logger":"tls\.(obtain|renew)"'`,
		"Caddy cannot get a certificate",
		// Right once the change that brings them has landed.
		"systemctl status gitea-caddy-dns-token.timer gitea-caddy-dns-token.service",
		"journalctl -u gitea-caddy-dns-token",
		"journalctl -t gitea-caddy-dns-check",
		"sudo gitea-caddy-dns-token --check",
		"make acme-staging-check",
		"gitea-cicd:certAlertDays",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("documentation lacks %q", want)
		}
	}
	// The minter, its hourly check and make acme-staging-check come with
	// changes of their own. A line that names one says so, so the e-mail is
	// right before they land and after.
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "gitea-caddy-dns-") && !strings.Contains(line, "once the Caddy DNS-token change has deployed") {
			t.Errorf("names the DNS-token change's tools without saying they come with it:\n%s", line)
		}
		if strings.Contains(line, "acme-staging-check") && !strings.Contains(line, "once its PR has merged") {
			t.Errorf("names make acme-staging-check without saying it comes with its own PR:\n%s", line)
		}
	}
}

func TestCertPolicyFollowsConfig(t *testing.T) {
	got, _ := run(t, testConfig(120))
	th := obj(condition(t, resourceNamed(t, got, "cert-not-renewing")), "conditionThreshold")
	if v := num(th, "thresholdValue"); v != 120 {
		t.Errorf("threshold %v, want 120 (gitea-cicd:certAlertDays)", v)
	}
}

// Caddy (certmagic) renews a certificate when a third of its lifetime is
// left, and retries until it expires. Let's Encrypt issues 64-day
// certificates from 2027-02-10, so renewal starts at about 21.3 days left;
// the default alerts with Caddy's retries at least retryDays old and leaves
// the operator fixDays to fix it.
//
// Before 2028-02-16, when Let's Encrypt moves to 45-day certificates (renewal
// from 15 days left), set config.CertLifetimeDays to 45: this then fails
// until config.DefaultCertAlertDays drops to about 10.
func TestDefaultThresholdLeavesCaddyItsRetries(t *testing.T) {
	const (
		lifetimeDays = config.CertLifetimeDays // 64
		renewRatio   = 1.0 / 3
		retryDays    = 3
		fixDays      = 10
	)
	d := float64(config.DefaultCertAlertDays)
	if ceiling := lifetimeDays*renewRatio - retryDays; d < fixDays || d > ceiling {
		t.Errorf("default %v days: want from %v (time to fix) to %.2f (a %d-day certificate's renewal point, less %d days of retries)",
			d, fixDays, ceiling, lifetimeDays, retryDays)
	}
}
