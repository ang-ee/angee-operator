#!/bin/sh
# Run the operator as the OWNER of the mounted stack root. The container's
# checkouts and compose files live on a host bind mount: root-run git refuses
# them ("dubious ownership") and root-run writes would leave root-owned files
# that break the host user's own git. Root here only bootstraps the matching
# user and docker-socket group, then drops privileges.
set -e

root=""
prev=""
for arg in "$@"; do
  [ "$prev" = "--root" ] && root="$arg"
  prev="$arg"
done

if [ "$(id -u)" = "0" ] && [ -n "$root" ] && [ -d "$root" ]; then
  uid=$(stat -c %u "$root")
  gid=$(stat -c %g "$root")
  if [ "$uid" != "0" ]; then
    getent group "$gid" >/dev/null 2>&1 || addgroup -g "$gid" angee
    group_name=$(getent group "$gid" | cut -d: -f1)
    getent passwd "$uid" >/dev/null 2>&1 \
      || adduser -D -H -u "$uid" -G "$group_name" -h /tmp angee
    user_name=$(getent passwd "$uid" | cut -d: -f1)

    sock=/var/run/docker.sock
    if [ -S "$sock" ]; then
      sock_gid=$(stat -c %g "$sock")
      if [ "$sock_gid" != "0" ]; then
        getent group "$sock_gid" >/dev/null 2>&1 || addgroup -g "$sock_gid" dockersock
        addgroup "$user_name" "$(getent group "$sock_gid" | cut -d: -f1)" 2>/dev/null || true
      else
        # Socket owned by root:root — container-local root-group membership
        # is the narrowest way to keep the docker API reachable after the drop.
        addgroup "$user_name" root 2>/dev/null || true
      fi
    fi

    # git wants a writable HOME for its global config lookup.
    export HOME=/tmp
    exec su-exec "$user_name" /usr/local/bin/angee-operator "$@"
  fi
fi

exec /usr/local/bin/angee-operator "$@"
