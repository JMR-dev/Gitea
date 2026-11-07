package iam

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v9/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"gitea-infra/pkg/config"
	"gitea-infra/pkg/secrets"
)

// What the mock hands back for an account's email and name. "mock", not the
// stack's project, so a value the code built itself would not match.
func mockEmail(accountID string) string { return accountID + "@mock.iam.gserviceaccount.com" }
func mockName(accountID string) string {
	return "projects/mock/serviceAccounts/" + mockEmail(accountID)
}

var (
	vmMember    = "serviceAccount:" + mockEmail("gitea-vm")
	caddyMember = "serviceAccount:" + mockEmail("gitea-caddy-dns")
)

const (
	secretIAMMember = "gcp:secretmanager/secretIamMember:SecretIamMember"
	accessor        = "roles/secretmanager.secretAccessor"
	versionAdder    = "roles/secretmanager.secretVersionAdder"
)

// registered is one resource as the program registered it.
type registered struct {
	typ    string
	inputs resource.PropertyMap
}

// recorder is a Pulumi mock that keeps what the program registers. Unlike
// package telemetry's, it keys by resource name: this program registers many
// resources of one type (IAMMember above all), and keyed by type all but the
// last would be lost.
type recorder struct {
	mu   sync.Mutex
	got  map[string]registered
	dups []string
}

func (r *recorder) NewResource(a pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.got[a.Name]; seen {
		r.dups = append(r.dups, a.Name)
	}
	r.got[a.Name] = registered{typ: a.TypeToken, inputs: a.Inputs}
	state := a.Inputs.Copy()
	if a.TypeToken == "gcp:serviceaccount/account:Account" {
		id := str(a.Inputs, "accountId")
		state["email"] = resource.NewStringProperty(mockEmail(id))
		state["name"] = resource.NewStringProperty(mockName(id))
	}
	return a.Name + "-id", state, nil
}

func (r *recorder) Call(a pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return a.Args, nil
}

func str(m resource.PropertyMap, key string) string {
	v, ok := m[resource.PropertyKey(key)]
	if !ok || !v.IsString() {
		return ""
	}
	return v.StringValue()
}

// everything runs New and every Grant in this package, as infra/main.go does,
// with the secrets main.go grants. It returns what was registered and the
// accounts New returned.
func everything(t *testing.T) (map[string]registered, *Accounts) {
	t.Helper()
	r := &recorder{got: map[string]registered{}}
	var accounts *Accounts
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cfg := &config.Config{Project: "p"}
		a, err := New(ctx, cfg, nil)
		if err != nil {
			return err
		}
		accounts = a
		repo, err := artifactregistry.NewRepository(ctx, "test-repo", &artifactregistry.RepositoryArgs{
			RepositoryId: pulumi.String("test-repo"),
			Format:       pulumi.String("DOCKER"),
		})
		if err != nil {
			return err
		}
		if err := GrantRegistry(ctx, cfg, a, repo); err != nil {
			return err
		}
		if err := GrantSecrets(ctx, cfg, a, secrets.Names); err != nil {
			return err
		}
		for _, name := range []string{secrets.ACMEEmail, secrets.RunnerTokenName} {
			if err := GrantSecretRead(ctx, cfg, a, name); err != nil {
				return err
			}
		}
		var buckets []*storage.Bucket
		for _, name := range []string{"test-config", "test-backup", "test-source"} {
			b, err := storage.NewBucket(ctx, name, &storage.BucketArgs{Location: pulumi.String("US-EAST1")})
			if err != nil {
				return err
			}
			buckets = append(buckets, b)
		}
		if err := GrantBuckets(ctx, cfg, a, buckets[0], buckets[1]); err != nil {
			return err
		}
		return GrantBuildSource(ctx, a, buckets[2])
	}, pulumi.WithMocks("p", "s", r))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dups) > 0 {
		t.Fatalf("resource names registered twice, the recorder lost one of each: %v", r.dups)
	}
	return r.got, accounts
}

// --- gitea-vm@ and Gitea's secrets -------------------------------------------

// gitea-vm@ reads each Gitea secret, one binding per secret: app.ini needs
// all four.
func TestGrantSecretsLetsTheVMReadEachSecret(t *testing.T) {
	got, _ := everything(t)
	for _, name := range secrets.Names {
		g, ok := got["vm-read-"+name]
		if !ok {
			t.Errorf("no vm-read-%s: gitea-vm@ can't read %s", name, name)
			continue
		}
		if g.typ != secretIAMMember || str(g.inputs, "secretId") != name ||
			str(g.inputs, "role") != accessor || str(g.inputs, "member") != vmMember {
			t.Errorf("vm-read-%s: %s %v, want a %s of %s on %s for %s",
				name, g.typ, g.inputs, secretIAMMember, accessor, name, vmMember)
		}
	}
}

// No account can add a version to any secret: the next deploy would install
// whatever is newest, and a new SECRET_KEY leaves everything Gitea encrypted
// with the old one unreadable. scripts/bootstrap.sh makes them.
func TestGrantSecretsGivesTheVMNoVersionAdder(t *testing.T) {
	got, _ := everything(t)
	if _, ok := got["vm-read-"+secrets.Names[0]]; !ok {
		t.Fatalf("GrantSecrets registered no read grant either: %v", got)
	}
	for name, g := range got {
		if str(g.inputs, "role") == versionAdder {
			t.Errorf("%s (%s) grants %s %s on %s",
				name, g.typ, str(g.inputs, "member"), versionAdder, str(g.inputs, "secretId"))
		}
		if strings.HasPrefix(name, "vm-add-") {
			t.Errorf("%s is registered: gitea-vm@ needs no grant beyond reading the secret", name)
		}
	}
}

// --- the Caddy DNS account ---------------------------------------------------

func TestCaddyDNSAccount(t *testing.T) {
	got, accounts := everything(t)
	if accounts.CaddyDNS == nil {
		t.Fatal("Accounts.CaddyDNS is nil")
	}
	sa, ok := got["gitea-caddy-dns-sa"]
	if !ok || sa.typ != "gcp:serviceaccount/account:Account" {
		t.Fatalf("no service account gitea-caddy-dns-sa: %+v", sa)
	}
	if id := str(sa.inputs, "accountId"); id != "gitea-caddy-dns" {
		t.Errorf("account id %q, want gitea-caddy-dns", id)
	}
	for _, key := range []string{"displayName", "description"} {
		if !strings.Contains(str(sa.inputs, key), "DNS-01") {
			t.Errorf("%s %q should say it is Caddy's ACME DNS-01 identity", key, str(sa.inputs, key))
		}
	}
}

// The account's IAM policy is who may act as it. Exactly one member: gitea-vm@,
// whose host timer mints Caddy's short-lived tokens. Anyone else here could
// write DNS records.
func TestCaddyDNSAccountsOnlyMemberIsTheVMsTokenCreator(t *testing.T) {
	got, _ := everything(t)
	var onIt []string
	for name, r := range got {
		if strings.HasPrefix(r.typ, "gcp:serviceaccount/iAM") &&
			str(r.inputs, "serviceAccountId") == mockName("gitea-caddy-dns") {
			onIt = append(onIt, name)
		}
	}
	if len(onIt) != 1 {
		t.Fatalf("IAM resources on gitea-caddy-dns@: %v, want exactly one", onIt)
	}
	m := got[onIt[0]]
	if m.typ != "gcp:serviceaccount/iAMMember:IAMMember" ||
		str(m.inputs, "role") != "roles/iam.serviceAccountTokenCreator" ||
		str(m.inputs, "member") != vmMember {
		t.Errorf("%s: %s %v, want gitea-vm@ as roles/iam.serviceAccountTokenCreator",
			onIt[0], m.typ, m.inputs)
	}
}

// A key is a long-lived secret that works from anywhere. Caddy's tokens are
// minted on the VM and expire within the hour; no account here has a key.
func TestNoServiceAccountKeys(t *testing.T) {
	got, _ := everything(t)
	for name, r := range got {
		if r.typ == "gcp:serviceaccount/key:Key" {
			t.Errorf("service account key %s registered", name)
		}
	}
}

// Its DNS access is granted in package dns, and that is all it has. Nothing in
// this package grants it anything: above all no secret, which is what keeping
// it apart from gitea-vm@ is for.
func TestCaddyDNSAccountGetsNothingHere(t *testing.T) {
	got, _ := everything(t)
	var secretGrants int
	for name, r := range got {
		if str(r.inputs, "member") == caddyMember {
			t.Errorf("%s (%s) grants gitea-caddy-dns@ %s", name, r.typ, str(r.inputs, "role"))
		}
		if ms := r.inputs["members"]; ms.IsArray() {
			for _, v := range ms.ArrayValue() {
				if v.IsString() && v.StringValue() == caddyMember {
					t.Errorf("%s (%s) lists gitea-caddy-dns@ as a member", name, r.typ)
				}
			}
		}
		if strings.HasPrefix(r.typ, "gcp:secretmanager/") {
			secretGrants++
			if str(r.inputs, "member") != vmMember {
				t.Errorf("secret grant %s goes to %s, want gitea-vm@ only", name, str(r.inputs, "member"))
			}
		}
	}
	// Proof the loop above saw the secret grants it is checking: gitea-vm@'s
	// read grant on each Gitea secret, the ACME email and the runner token.
	if want := len(secrets.Names) + 2; secretGrants != want {
		t.Errorf("%d secret grants, want %d", secretGrants, want)
	}
}

// gitea-vm@ keeps every project role it had. The Caddy account is an addition
// here, not a replacement.
func TestVMAccountUnchanged(t *testing.T) {
	got, _ := everything(t)
	if id := str(got["gitea-vm-sa"].inputs, "accountId"); id != "gitea-vm" {
		t.Fatalf("gitea-vm-sa account id %q", id)
	}
	var roles []string
	for _, r := range got {
		if r.typ == "gcp:projects/iAMMember:IAMMember" && str(r.inputs, "member") == vmMember {
			roles = append(roles, str(r.inputs, "role"))
		}
	}
	slices.Sort(roles)
	if want := []string{"roles/logging.logWriter", "roles/monitoring.metricWriter"}; !slices.Equal(roles, want) {
		t.Errorf("gitea-vm@'s project roles from this package: %v, want %v", roles, want)
	}
}
