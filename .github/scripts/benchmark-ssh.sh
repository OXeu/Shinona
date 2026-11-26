#!/usr/bin/env bash
set -euo pipefail

: "${TMPDIR:?Set TMPDIR to the workspace .tmp directory}"
fixture=$(mktemp -d "$TMPDIR/shinona-bench-ssh.XXXXXXXX")
image="${SHINONA_BENCH_SSH_IMAGE:-shinona-bench-ssh:${fixture##*.}}"
cleanup() {
  if [[ -s "$fixture/sshd.pid" ]]; then
    sudo -n kill "$(cat "$fixture/sshd.pid")" 2>/dev/null || true
  fi
  if [[ -z "${SHINONA_BENCH_SSH_IMAGE:-}" ]]; then
    docker image rm "$image" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$fixture"
}
trap cleanup EXIT

if [[ -z "${SHINONA_BENCH_SSH_IMAGE:-}" ]]; then
  docker build --tag "$image" - <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash git util-linux \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
fi

port=$(python3 - <<'PY'
import socket
with socket.socket() as server:
    server.bind(("127.0.0.1", 0))
    print(server.getsockname()[1])
PY
)
user=$(id -un)
ssh-keygen -q -t ed25519 -N '' -f "$fixture/host"
ssh-keygen -q -t ed25519 -N '' -f "$fixture/client"

cat > "$fixture/sshd_config" <<CONFIG
Port $port
ListenAddress 127.0.0.1
HostKey "$fixture/host"
PidFile "$fixture/sshd.pid"
AuthorizedKeysFile "$fixture/client.pub"
AllowUsers $user
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM yes
DisableForwarding yes
PermitUserRC no
CONFIG
sudo -n "$(command -v sshd)" -E "$fixture/sshd.log" -f "$fixture/sshd_config" || {
  sudo -n cat "$fixture/sshd.log"
  exit 1
}

export SHINONA_BENCH_SSH_ADDRESS="127.0.0.1:$port"
export SHINONA_BENCH_SSH_USER="$user"
export SHINONA_BENCH_SSH_KEY_PATH="$fixture/client"
export SHINONA_BENCH_SSH_HOST_FINGERPRINT
SHINONA_BENCH_SSH_HOST_FINGERPRINT=$(ssh-keygen -lf "$fixture/host.pub" | cut -d ' ' -f 2)
SHINONA_BENCH_SSH_IMAGE="$image" "$@"
