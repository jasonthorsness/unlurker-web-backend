#!/usr/bin/env bash
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "Usage: $0 <host> [user] [service]"
  exit 1
fi

HOST=$1
USER=${2:-ec2-user}
SERVICE=${3:-unls}

ssh "$USER@$HOST" "sudo systemctl stop $SERVICE"
scp "bin/unls" "$USER@$HOST:unls"
ssh "$USER@$HOST" "sudo systemctl restart $SERVICE"

echo "✅ Deployed unls to $HOST and restarted $SERVICE"
