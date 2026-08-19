#!/usr/bin/env bash
#
# One-time project bootstrap, run from a workstation BEFORE the first
# `pulumi up`.
#
# Everything here exists because Pulumi cannot create it:
#
#   * the GCS bucket that holds Pulumi's own state
#   * the passphrase that encrypts that state
#   * the service account that RUNS Pulumi in Cloud Build
#   * Gitea's signing secrets -- INTERNAL_TOKEN must be a valid Gitea-issued
#     JWT, so `gitea generate secret` has to produce it, and the values must
#     exist before the VM first boots and tries to render app.ini
#
# Idempotent: safe to re-run.
#
# Usage: scripts/bootstrap.sh <project-id> [region]   (default region: us-east1)

set -euo pipefail

PROJECT="${1:-}"
REGION="${2:-us-east1}"

if [[ -z "${PROJECT}" ]]; then
    echo "usage: $0 <project-id> [region]" >&2
    exit 1
fi

STATE_BUCKET="${PROJECT}-pulumi-state"
INFRA_SA="cb-infra"
INFRA_SA_EMAIL="${INFRA_SA}@${PROJECT}.iam.gserviceaccount.com"
GITEA_IMAGE="docker.io/gitea/gitea:latest"

log() { echo "==> $*"; }

command -v gcloud >/dev/null || { echo "gcloud is required" >&2; exit 1; }

# ---------------------------------------------------------------------------
log "enabling APIs"
# ---------------------------------------------------------------------------
gcloud services enable --project="${PROJECT}" \
    compute.googleapis.com \
    dns.googleapis.com \
    artifactregistry.googleapis.com \
    cloudbuild.googleapis.com \
    secretmanager.googleapis.com \
    iap.googleapis.com \
    storage.googleapis.com \
    logging.googleapis.com \
    monitoring.googleapis.com \
    cloudscheduler.googleapis.com \
    iamcredentials.googleapis.com \
    oslogin.googleapis.com

# ---------------------------------------------------------------------------
log "creating Pulumi state bucket gs://${STATE_BUCKET}"
# ---------------------------------------------------------------------------
if ! gcloud storage buckets describe "gs://${STATE_BUCKET}" --project="${PROJECT}" >/dev/null 2>&1; then
    gcloud storage buckets create "gs://${STATE_BUCKET}" \
        --project="${PROJECT}" \
        --location="${REGION}" \
        --uniform-bucket-level-access \
        --public-access-prevention
fi
# Versioning is the undo button for a corrupted or truncated state file.
gcloud storage buckets update "gs://${STATE_BUCKET}" --versioning --project="${PROJECT}"

# ---------------------------------------------------------------------------
log "creating secrets"
# ---------------------------------------------------------------------------
ensure_secret() {
    local name="$1"
    if ! gcloud secrets describe "${name}" --project="${PROJECT}" >/dev/null 2>&1; then
        gcloud secrets create "${name}" --project="${PROJECT}" \
            --replication-policy=automatic --labels=app=gitea
    fi
}

has_version() {
    gcloud secrets versions list "$1" --project="${PROJECT}" \
        --filter='state:ENABLED' --limit=1 --format='value(name)' 2>/dev/null | grep -q .
}

# Pulumi's state encryption passphrase. Generated here so it never lives in a
# shell history or a config file.
ensure_secret pulumi-config-passphrase
if ! has_version pulumi-config-passphrase; then
    log "generating Pulumi state passphrase"
    openssl rand -base64 48 | tr -d '\n' \
        | gcloud secrets versions add pulumi-config-passphrase --project="${PROJECT}" --data-file=-
fi

# The GitHub PAT for the Cloud Build connection. Created empty on purpose --
# a PAT is an interactive artifact and cannot be generated here.
ensure_secret github-pat
if ! has_version github-pat; then
    echo "    NOTE: secret 'github-pat' has no value yet."
    echo "          Create a GitHub PAT with repo + read:user scope and run:"
    echo "            printf %s '<token>' | gcloud secrets versions add github-pat --project=${PROJECT} --data-file=-"
fi

# Gitea's signing secrets. These MUST come from `gitea generate secret`:
# INTERNAL_TOKEN is a JWT, and a random string there produces an instance that
# starts and then fails every internal API call in a confusing way.
declare -A GITEA_SECRETS=(
    [gitea-secret-key]=SECRET_KEY
    [gitea-internal-token]=INTERNAL_TOKEN
    [gitea-oauth2-jwt-secret]=JWT_SECRET
    [gitea-lfs-jwt-secret]=LFS_JWT_SECRET
)

runner=""
for candidate in podman docker; do
    command -v "${candidate}" >/dev/null 2>&1 && { runner="${candidate}"; break; }
done

for name in "${!GITEA_SECRETS[@]}"; do
    ensure_secret "${name}"
    if has_version "${name}"; then
        log "secret ${name} already populated -- leaving it alone"
        continue
    fi
    if [[ -z "${runner}" ]]; then
        echo "    WARNING: no podman/docker available; cannot generate ${name}." >&2
        echo "             Install one and re-run, or the VM will skip rendering app.ini." >&2
        continue
    fi
    log "generating ${name}"
    # The upstream image is used only as a throwaway generator here; the
    # deployed image is our own Debian 13 build.
    "${runner}" run --rm "${GITEA_IMAGE}" gitea generate secret "${GITEA_SECRETS[$name]}" \
        | tr -d '\n' \
        | gcloud secrets versions add "${name}" --project="${PROJECT}" --data-file=-
done

# ---------------------------------------------------------------------------
log "creating the Pulumi runner service account ${INFRA_SA_EMAIL}"
# ---------------------------------------------------------------------------
# This is the chicken-and-egg account: it is the identity that runs `pulumi up`,
# so it cannot be created by `pulumi up`.
if ! gcloud iam service-accounts describe "${INFRA_SA_EMAIL}" --project="${PROJECT}" >/dev/null 2>&1; then
    gcloud iam service-accounts create "${INFRA_SA}" \
        --project="${PROJECT}" \
        --display-name="Cloud Build: infrastructure (runs Pulumi)"
fi

# Broad by necessity -- Pulumi manages IAM, compute, DNS, and secrets bindings.
# Deliberately a different identity from cb-image@, which only pushes images.
INFRA_ROLES=(
    roles/compute.admin
    roles/dns.admin
    roles/artifactregistry.admin
    roles/secretmanager.admin
    roles/iam.serviceAccountAdmin
    roles/iam.serviceAccountUser
    roles/resourcemanager.projectIamAdmin
    roles/serviceusage.serviceUsageAdmin
    roles/cloudscheduler.admin
    roles/cloudbuild.builds.editor
    roles/iap.tunnelResourceAccessor
    roles/compute.osAdminLogin
    roles/logging.logWriter
)
for role in "${INFRA_ROLES[@]}"; do
    gcloud projects add-iam-policy-binding "${PROJECT}" \
        --member="serviceAccount:${INFRA_SA_EMAIL}" \
        --role="${role}" \
        --condition=None \
        --quiet >/dev/null
done

# Pulumi's state lives in the bucket, so the runner needs write access to it --
# scoped to that bucket rather than project-wide storage admin.
gcloud storage buckets add-iam-policy-binding "gs://${STATE_BUCKET}" \
    --project="${PROJECT}" \
    --member="serviceAccount:${INFRA_SA_EMAIL}" \
    --role=roles/storage.admin >/dev/null

cat <<SUMMARY

Bootstrap complete.

  Pulumi backend : gs://${STATE_BUCKET}
  Pulumi runner  : ${INFRA_SA_EMAIL}

Next:
  1. Install the Cloud Build GitHub App on your repository and note the
     installation id, then populate the github-pat secret (see note above).
  2. Confirm DNS delegation:  dig NS <your-domain>
  3. cd infra
     pulumi login gs://${STATE_BUCKET}
     pulumi stack init prod
     pulumi config set gcp:project ${PROJECT}
     pulumi config set gitea:infraBuildServiceAccount ${INFRA_SA_EMAIL}
     # ...plus domain, dnsZone, acmeEmail, githubOwner, githubAppInstallationId
     pulumi up
  4. make build     # or, spelled out:
     gcloud builds submit --config cloudbuild/image.yaml --project ${PROJECT} \\
       --region ${REGION} \\
       --service-account projects/${PROJECT}/serviceAccounts/cb-image@${PROJECT}.iam.gserviceaccount.com

SUMMARY
