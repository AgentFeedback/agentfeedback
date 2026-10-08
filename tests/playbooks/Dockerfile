# The clean Linux machine of the playbook gate (scripts/playbooks.py): systemd
# as PID 1 so the stack playbook's user unit runs as written, a `local` user
# who runs the client playbook with no server, a lingering `stack` user who
# owns the server, and a `client` user who joins it with the client playbook.
# Only what the playbooks expect of a stock machine is installed; the gate
# runs on the host and executes each block with docker exec.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus-user-session curl ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/bash local \
 && useradd -m -s /bin/bash stack \
 && useradd -m -s /bin/bash client \
 && mkdir -p /var/lib/systemd/linger \
 && touch /var/lib/systemd/linger/stack
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
