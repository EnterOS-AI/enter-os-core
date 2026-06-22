#!/usr/bin/env bash

# EC2 leak check for staging E2E harnesses.
#
# Modes:
#   E2E_AWS_LEAK_CHECK=off       skip
#   E2E_AWS_LEAK_CHECK=auto      check only when aws + credentials exist
#   E2E_AWS_LEAK_CHECK=required  fail if aws + credentials are unavailable
#
# Optional:
#   E2E_AWS_LEAK_CHECK_SECS      poll budget, default 90
#   E2E_AWS_LEAK_CHECK_INTERVAL  poll interval, default 10
#   E2E_AWS_TERMINATE_LEAKS=1    terminate matching leaked instances

e2e_aws_leak_mode() {
  echo "${E2E_AWS_LEAK_CHECK:-auto}"
}

e2e_aws_region() {
  echo "${E2E_AWS_REGION:-${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-2}}}"
}

e2e_aws_creds_available() {
  command -v aws >/dev/null 2>&1 || return 1
  [ -n "${AWS_ACCESS_KEY_ID:-}" ] || return 1
  [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || return 1
}

e2e_ec2_instances_for_slug() {
  local slug="$1"
  local region
  region=$(e2e_aws_region)

  # shellcheck disable=SC2016
  aws ec2 describe-instances \
    --region "$region" \
    --filters "Name=tag:Name,Values=*$slug*" \
              "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].[InstanceId,State.Name,Tags[?Key==`Name`].Value|[0]]' \
    --output text
}

e2e_terminate_instances() {
  local ids="$1"
  local region
  region=$(e2e_aws_region)

  [ -n "$ids" ] || return 0
  # shellcheck disable=SC2086
  aws ec2 terminate-instances --region "$region" --instance-ids $ids >/dev/null
}

e2e_verify_no_ec2_leaks_for_slug() {
  local slug="$1"
  local mode
  local max_secs
  local interval
  local elapsed=0
  local rows=""
  local ids=""

  mode=$(e2e_aws_leak_mode)
  case "$mode" in
    off)
      echo "[aws-leak-check] skipped: E2E_AWS_LEAK_CHECK=off" >&2
      return 0
      ;;
    auto|required) ;;
    *)
      echo "[aws-leak-check] invalid E2E_AWS_LEAK_CHECK=$mode (expected off|auto|required)" >&2
      return 2
      ;;
  esac

  if ! e2e_aws_creds_available; then
    if [ "$mode" = "required" ]; then
      echo "[aws-leak-check] required but aws CLI or AWS credentials are unavailable" >&2
      return 2
    fi
    echo "[aws-leak-check] skipped: aws CLI or AWS credentials unavailable" >&2
    return 0
  fi

  max_secs="${E2E_AWS_LEAK_CHECK_SECS:-90}"
  interval="${E2E_AWS_LEAK_CHECK_INTERVAL:-10}"

  while true; do
    rows=$(e2e_ec2_instances_for_slug "$slug" 2>&1) || {
      echo "[aws-leak-check] aws ec2 describe-instances failed for slug=$slug" >&2
      echo "$rows" >&2
      return 2
    }

    if [ -z "$rows" ] || [ "$rows" = "None" ]; then
      echo "[aws-leak-check] no live EC2 instances for slug=$slug" >&2
      return 0
    fi

    if [ "$elapsed" -ge "$max_secs" ]; then
      echo "[aws-leak-check] leaked EC2 instance(s) for slug=$slug after ${elapsed}s:" >&2
      echo "$rows" >&2
      if [ "${E2E_AWS_TERMINATE_LEAKS:-0}" = "1" ]; then
        ids=$(echo "$rows" | awk 'NF {print $1}' | sort -u | tr '\n' ' ')
        echo "[aws-leak-check] terminating leaked EC2 instance(s): $ids" >&2
        e2e_terminate_instances "$ids" || {
          echo "[aws-leak-check] terminate-instances failed for: $ids" >&2
          return 4
        }
      fi
      return 4
    fi

    sleep "$interval"
    elapsed=$((elapsed + interval))
  done
}
