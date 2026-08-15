#!/usr/bin/env bash
# Shared helpers for waldo end-to-end tests against real agents.
set -uo pipefail

WALDO_E2E_DIR="${WALDO_E2E_DIR:-/tmp/waldo-e2e}"
SSH_PORT="${WALDO_E2E_SSH_PORT:-22222}"
CONTAINER="${WALDO_E2E_CONTAINER:-waldo-sshd}"
IMAGE="waldo-test-sshd"

pass=0; fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n'  "$1"; printf '        %s\n' "${2:-}"; fail=$((fail+1)); }
info() { printf '\033[1m%s\033[0m\n' "$1"; }

# assert_contains HAYSTACK NEEDLE LABEL
assert_contains() {
  if [[ "$1" == *"$2"* ]]; then ok "$3"; else bad "$3" "expected to contain: $2"; fi
}
assert_not_contains() {
  if [[ "$1" != *"$2"* ]]; then ok "$3"; else bad "$3" "should NOT contain: $2"; fi
}

# clean_agent_env runs a coding agent without the state it inherits when waldo's
# own tests are themselves run from inside a coding agent. A nested Claude Code
# otherwise hangs on the parent's session variables, and an inherited
# ANTHROPIC_API_KEY silently overrides the OAuth login and fails auth.
clean_agent_env() {
  env -u CLAUDECODE -u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_SESSION_ID \
      -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_EXECPATH -u CLAUDE_CODE_MESSAGING_SOCKET \
      -u CLAUDE_PID -u CLAUDE_EFFORT -u AI_AGENT -u ANTHROPIC_API_KEY \
      "$@" </dev/null
}

setup_target() {
  mkdir -p "$WALDO_E2E_DIR"
  cd "$WALDO_E2E_DIR" || exit 1

  if [[ ! -f test_key ]]; then
    ssh-keygen -q -t ed25519 -N '' -f test_key -C waldo-e2e
  fi
  cat > Dockerfile <<'DOCKER'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server ca-certificates \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh
COPY test_key.pub /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys && \
    sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
RUN mkdir -p /srv/app && echo "hello from remote" > /srv/app/README.md
EXPOSE 22
CMD ["/usr/sbin/sshd","-D","-e"]
DOCKER
  docker build -q -t "$IMAGE" . >/dev/null 2>&1 || return 1
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  docker run -d --name "$CONTAINER" -p "${SSH_PORT}:22" "$IMAGE" >/dev/null 2>&1 || return 1

  cat > ssh_config <<CONF
Host waldo-e2e
  HostName 127.0.0.1
  Port ${SSH_PORT}
  User root
  IdentityFile ${WALDO_E2E_DIR}/test_key
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
CONF
  export WALDO_SSH_CONFIG="$WALDO_E2E_DIR/ssh_config"
  export WALDO_HOME="$WALDO_E2E_DIR/home"
  rm -rf "$WALDO_HOME"

  for _ in $(seq 30); do
    ssh -F ssh_config waldo-e2e true 2>/dev/null && return 0
    sleep 0.5
  done
  return 1
}

teardown_target() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }

summary() {
  echo
  if (( fail == 0 )); then
    printf '\033[32m%d passed, 0 failed\033[0m\n' "$pass"; return 0
  fi
  printf '\033[31m%d passed, %d FAILED\033[0m\n' "$pass" "$fail"; return 1
}
