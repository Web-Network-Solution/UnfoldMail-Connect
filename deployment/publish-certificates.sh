#!/bin/sh
set -eu
umask 077
lineage=/etc/letsencrypt/live/unfoldmail-connect
# The gateway runs as distroless UID 65532; never make its private key public.
for name in fullchain.pem privkey.pem; do
  cp "$lineage/$name" "/certs/$name.next"
  chown 65532:65532 "/certs/$name.next"
  chmod 600 "/certs/$name.next"
  mv -f "/certs/$name.next" "/certs/$name"
done
