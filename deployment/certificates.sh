#!/bin/sh
set -eu
: "${MAIL_HOSTNAME:?MAIL_HOSTNAME is required}"
: "${ACME_EMAIL:?ACME_EMAIL is required}"
# Fixed certificate name avoids interpreting a hostname as a filesystem path.
certbot certonly --standalone --non-interactive --agree-tos \
  --email "$ACME_EMAIL" --domain "$MAIL_HOSTNAME" --cert-name unfoldmail-connect \
  --keep-until-expiring
/bin/sh /deploy/publish-certificates.sh
trap 'exit 0' TERM INT
while true; do
  sleep 43200 &
  wait $!
  if certbot renew --non-interactive --cert-name unfoldmail-connect; then
    /bin/sh /deploy/publish-certificates.sh
  else
    echo 'Certificate renewal failed; inspect certificate service logs.' >&2
  fi
done
