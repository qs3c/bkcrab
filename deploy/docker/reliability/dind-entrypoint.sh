#!/bin/sh
# The daemon API belongs to the outer Compose network (eth0). Inner sandboxes
# must not create their own privileged/unlimited containers through docker0.
set -eu
if [ -n "${DOCKER_IPTABLES_LEGACY:-}" ]; then
    export PATH="/usr/local/sbin/.iptables-legacy:$PATH"
else
    # Keep this wrapper and the upstream entrypoint on the same backend.
    export DOCKER_IPTABLES_LEGACY=
fi
for firewall in iptables ip6tables; do
    "$firewall" -w -C INPUT ! -i eth0 -p tcp --dport 2375 -j REJECT 2>/dev/null ||
        "$firewall" -w -I INPUT 1 ! -i eth0 -p tcp --dport 2375 -j REJECT
done
exec /usr/local/bin/dockerd-entrypoint.sh "$@"
