#!/bin/bash
# lockdown-tenant-sg.sh — restrict the tenant EC2 security group to Cloudflare IPs only
#
# Phase 35.1 security hardening. Workspace EC2 instances currently allow
# inbound from 0.0.0.0/0 on port 8080. Locking to Cloudflare's IP ranges
# means only requests coming through Cloudflare (Worker or Tunnel) reach
# the instance — direct IP access is blocked.
#
# IMPORTANT: if you've fully migrated to Cloudflare Tunnel (issue #933),
# you should run --close-ingress instead. Tunnel is outbound-only from
# the EC2 side, so no public ingress is needed at all.
#
# Usage:
#   bash scripts/lockdown-tenant-sg.sh --sg-id sg-xxxxx                 # lock to CF IPs
#   bash scripts/lockdown-tenant-sg.sh --sg-id sg-xxxxx --close-ingress # remove all public ingress
#   bash scripts/lockdown-tenant-sg.sh --sg-id sg-xxxxx --dry-run       # preview changes

set -euo pipefail

SG_ID=""
PORT=8080
CLOSE_INGRESS=false
DRY_RUN=false

while [ $# -gt 0 ]; do
  case "$1" in
    --sg-id) SG_ID="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --close-ingress) CLOSE_INGRESS=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    -h|--help)
      head -25 "$0" | tail -20 | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$SG_ID" ]; then
  echo "error: --sg-id is required" >&2
  echo "usage: $0 --sg-id sg-xxxxx [--port 8080] [--close-ingress] [--dry-run]" >&2
  exit 1
fi

run() {
  if [ "$DRY_RUN" = true ]; then
    echo "DRY RUN: $*"
  else
    "$@"
  fi
}

echo "=== Current ingress on $SG_ID (port $PORT) ==="
aws ec2 describe-security-groups --group-ids "$SG_ID" \
  --query "SecurityGroups[0].IpPermissions[?FromPort==\`$PORT\`]" --output table

echo ""
echo "=== Revoking existing 0.0.0.0/0 ingress on port $PORT ==="
run aws ec2 revoke-security-group-ingress \
  --group-id "$SG_ID" \
  --protocol tcp --port "$PORT" \
  --cidr 0.0.0.0/0 2>/dev/null || echo "  (no 0.0.0.0/0 rule — already locked)"

if [ "$CLOSE_INGRESS" = true ]; then
  echo ""
  echo "=== Close mode: no ingress added. EC2 reachable only via Cloudflare Tunnel. ==="
  exit 0
fi

echo ""
echo "=== Fetching Cloudflare IP ranges ==="
CF_IPS=$(curl -fsSL https://www.cloudflare.com/ips-v4)
IP_COUNT=$(echo "$CF_IPS" | wc -l | tr -d ' ')
echo "Got $IP_COUNT Cloudflare IPv4 ranges"

echo ""
echo "=== Adding Cloudflare ingress rules on port $PORT ==="
for ip in $CF_IPS; do
  run aws ec2 authorize-security-group-ingress \
    --group-id "$SG_ID" \
    --protocol tcp --port "$PORT" \
    --cidr "$ip" \
    --tag-specifications "ResourceType=security-group-rule,Tags=[{Key=Purpose,Value=cloudflare-only}]" \
    2>/dev/null || echo "  (rule for $ip already exists)"
done

echo ""
echo "=== Final ingress on $SG_ID ==="
if [ "$DRY_RUN" = false ]; then
  aws ec2 describe-security-groups --group-ids "$SG_ID" \
    --query "SecurityGroups[0].IpPermissions[?FromPort==\`$PORT\`].IpRanges[].CidrIp" \
    --output table
fi

echo ""
echo "=== Done ==="
echo "Tenant EC2 is now reachable only via Cloudflare. Direct IP access blocked."
echo ""
echo "To revert (re-open to 0.0.0.0/0):"
echo "  aws ec2 authorize-security-group-ingress --group-id $SG_ID --protocol tcp --port $PORT --cidr 0.0.0.0/0"
