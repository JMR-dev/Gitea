// Package secrets names Gitea's signing secrets. It deliberately creates
// nothing.
//
// The secrets are created and populated by scripts/bootstrap.sh, outside
// Pulumi, for two reasons:
//
//  1. Ordering. INTERNAL_TOKEN must be a valid Gitea-issued JWT, not a random
//     string, so `gitea generate secret` has to produce it. If Pulumi owned the
//     containers, the values could only be added after `pulumi up` -- but the VM
//     boots during that same `pulumi up` and wants to render app.ini.
//
//  2. Blast radius. `pulumi destroy` should not be able to delete the keys that
//     every existing session, OAuth token, and LFS URL is signed with. Same
//     reasoning as the pre-existing DNS zone and the backup bucket.
//
// Pulumi still grants the VM access to them by id -- an IAM binding does not
// require owning the resource.
package secrets

// Names are the Secret Manager secret ids. vm/bootstrap.sh maps each one to its
// `gitea generate secret` argument by suffix, so renaming these means updating
// fetch_or_create_secret too.
var Names = []string{
	"gitea-secret-key",
	"gitea-internal-token",
	"gitea-oauth2-jwt-secret",
	"gitea-lfs-jwt-secret",
}

// ACMEEmail holds the contact address Caddy registers with Let's Encrypt. It
// is a secret not because it signs anything but to keep it out of this public
// repository and out of instance metadata. Unlike Names it is supplied by the
// operator, never generated, so the VM gets read access only.
const ACMEEmail = "gitea-acme-email"

// RunnerTokenName is the Secret Manager id of the Gitea Actions runner
// registration token. Like Names, it is created by scripts/bootstrap.sh and
// never read by Pulumi; unlike Names, it is deliberately NOT in that list.
//
// vm/bootstrap.sh treats Names as all-or-nothing -- render_all skips rendering
// app.ini entirely if any one of them is missing, and fetch_or_create_secret
// maps each to a `gitea generate secret` key by suffix. This token is neither:
// it is a random string from `openssl rand`, it is optional (only needed when
// gitea:runnersEnabled is set), and a missing one must not take the rest of the
// config down with it.
//
// The same value reaches both ends: the VM passes it to Gitea as
// GITEA_RUNNER_REGISTRATION_TOKEN_FILE, and the runner pods mount it through the
// GKE Secret Manager CSI add-on.
const RunnerTokenName = "gitea-runner-registration-token"
