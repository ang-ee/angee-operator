#!/bin/sh
# Runs inside the source container (no pkill there): SIGTERM every pg_dump process.
for p in /proc/[0-9]*; do
  if [ "$(cat "$p/comm" 2>/dev/null)" = pg_dump ]; then
    kill -TERM "${p#/proc/}" && echo "killed pg_dump pid ${p#/proc/}"
  fi
done
