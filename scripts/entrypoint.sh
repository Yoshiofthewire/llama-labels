#!/bin/sh
set -eu

mkdir -p /kypost/config /kypost/logs /kypost/state

# Runs synchronously (as root, before the chown below) so admin.env exists
# before any service starts — a hard guarantee that supervisord's
# priority-based program ordering could only approximate.
/bin/sh /opt/kypost/scripts/bootstrap.sh

chown -R kypost:kypost /kypost

exec supervisord -c /etc/supervisord.conf