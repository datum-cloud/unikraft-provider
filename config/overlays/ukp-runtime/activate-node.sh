#!/bin/sh
# Obtain this node's ukp license certificate before the runtime starts; see
# docs/architecture/node-licensing.md.
#
# The token is not passed here: the launcher sources /etc/ukp.conf with `set -a`,
# which pulls in the credentials overlay and exports NODE_ACTIVATION_TOKEN.
#
# Fail closed — a node that cannot obtain a license must not serve guests — but
# retry first, so a control-plane blip does not tear the runtime down.
set -u

agent=/usr/lib/ukp/agent/launcher/agent
secrets=/etc/ukp-secrets/ukp.secrets.conf
attempts=5
sleep_between=10

# Nothing to do while the cached certificate is valid; the agent owns renewal.
not_after="$("$agent" license status 2>/dev/null | sed -n 's/^not_after: "\(.*\)"$/\1/p')"
if [ -n "$not_after" ]; then
  expires_at="$(date -u -d "$not_after" +%s 2>/dev/null || echo 0)"
  if [ "$expires_at" -gt "$(date -u +%s)" ]; then
    echo "activate-node: licensed through $not_after"
    exit 0
  fi
  echo "activate-node: cached license expired at $not_after; re-activating"
fi

# No secret is a configuration error, not a reason to run unlicensed.
if ! grep -q '^NODE_ACTIVATION_TOKEN=' "$secrets" 2>/dev/null; then
  echo "activate-node: FATAL: no NODE_ACTIVATION_TOKEN in $secrets" >&2
  exit 1
fi

attempt=1
while :; do
  if "$agent" license activate; then
    "$agent" license status
    exit 0
  fi
  if [ "$attempt" -ge "$attempts" ]; then
    echo "activate-node: FATAL: activation failed after $attempts attempts" >&2
    exit 1
  fi
  echo "activate-node: activation attempt $attempt failed; retrying in ${sleep_between}s" >&2
  attempt=$((attempt + 1))
  sleep "$sleep_between"
done
