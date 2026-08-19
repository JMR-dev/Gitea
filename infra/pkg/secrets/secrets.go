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
