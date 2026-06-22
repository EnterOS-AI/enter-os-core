#!/usr/bin/env bash
# ensure-ecr-lifecycle.sh — idempotently apply the canonical ECR image
# lifecycle policy to a prod ECR repository, called from the publish
# pipelines right after they push an image.
#
# Why this exists: the prod ECR repos under
# <AWS_ACCOUNT_ID>.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/* had their
# lifecycle policies set out-of-band (no IaC managed them), so the prod
# ECR storage bill (~$56/mo, account <AWS_ACCOUNT_ID>) kept growing —
# platform-tenant alone accumulated 70+ images / 12GB+. Every untagged
# layer and every superseded :sha-<...> tag lingered forever.
#
# The durable fix: the publish workflows ALREADY authenticate to prod ECR
# and push images, so they already hold the right creds + region. This
# script just adds a `put-lifecycle-policy` call after each push. ECR's
# own lifecycle engine then expires old images on its schedule — no
# deletes happen here, this only DECLARES the policy. Re-applying the
# same policy on every build keeps it in lockstep with this file (IaC),
# so an out-of-band edit is corrected on the next publish.
#
# SSOT: the canonical policy JSON below is the single source of truth.
# It is intentionally duplicated byte-for-byte into the equivalent script
# in each repo whose publish workflow pushes to prod ECR (a workflow can
# only call a script in its own checkout); keep them identical. The policy
# was validated on the operator account before rollout.
#
# Policy:
#   rule 1 — expire untagged images 1 day after push (build cache churn,
#            orphaned layers from re-pushed tags)
#   rule 2 — keep only the last 10 tagged images for the sha-/v/latest/
#            staging/main tag families (per-prefix retention; ECR keeps
#            the N most-recent by push time and expires older)
#
# Fail-soft by design: a publish MUST NOT fail because policy application
# errored (e.g. transient ECR API blip, IAM gap). On any error this logs
# a warning and exits 0. The policy is reapplied on the next publish.
#
# Usage:
#   scripts/ops/ensure-ecr-lifecycle.sh <repository-name>
#     e.g. scripts/ops/ensure-ecr-lifecycle.sh molecule-ai/platform-tenant
#
# Env (all optional — sane defaults match the publish workflows):
#   AWS_REGION / AWS_DEFAULT_REGION — ECR region (default: us-east-2)
#   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY — provided by the publish step
#
# Exit codes:
#   0 — policy applied, already current, or fail-soft no-op (always 0)

set -uo pipefail

REPO="${1:-}"
REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-2}}"

if [ -z "${REPO}" ]; then
  echo "::warning::ensure-ecr-lifecycle: no repository name given; skipping" >&2
  exit 0
fi

# --- Canonical lifecycle policy (SSOT) -------------------------------------
# Keep this JSON identical across every repo's copy of this script.
read -r -d '' LIFECYCLE_POLICY <<'JSON' || true
{"rules":[
 {"rulePriority":1,"description":"Expire untagged after 1 day","selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":1},"action":{"type":"expire"}},
 {"rulePriority":2,"description":"Keep last 10 tagged","selection":{"tagStatus":"tagged","tagPrefixList":["sha-","v","latest","staging","main"],"countType":"imageCountMoreThan","countNumber":10},"action":{"type":"expire"}}
]}
JSON

if ! command -v aws >/dev/null 2>&1; then
  echo "::warning::ensure-ecr-lifecycle: aws CLI not found; skipping policy for ${REPO}" >&2
  exit 0
fi

echo "::notice::ensure-ecr-lifecycle: applying canonical lifecycle policy to ${REPO} (region ${REGION})"

if aws ecr put-lifecycle-policy \
      --repository-name "${REPO}" \
      --region "${REGION}" \
      --lifecycle-policy-text "${LIFECYCLE_POLICY}" >/dev/null 2>/tmp/ecr-lifecycle-err.$$; then
  echo "::notice::ensure-ecr-lifecycle: policy applied to ${REPO}"
else
  echo "::warning::ensure-ecr-lifecycle: put-lifecycle-policy failed for ${REPO} (non-fatal — policy reapplies next publish)" >&2
  sed 's/^/::warning::ensure-ecr-lifecycle:   /' /tmp/ecr-lifecycle-err.$$ >&2 2>/dev/null || true
fi
rm -f /tmp/ecr-lifecycle-err.$$ 2>/dev/null || true

# Always succeed — never break a publish on lifecycle-policy errors.
exit 0
