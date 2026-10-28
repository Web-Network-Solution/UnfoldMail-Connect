#!/usr/bin/env bash
# Run from a fresh dedicated Docker Droplet; secrets are read interactively.
set -euo pipefail
if [[ ${EUID} -ne 0 ]]; then echo 'Run this setup with sudo bash.' >&2; exit 1; fi
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
command -v docker >/dev/null || { echo 'Docker is required. Use the Docker Droplet image.' >&2; exit 1; }
docker compose version >/dev/null
command -v python3 >/dev/null || { echo 'Install python3 before setup.' >&2; exit 1; }
if [[ -e .env ]]; then
  echo 'An .env already exists; keeping it unchanged. Review it and run docker compose up -d --build.' >&2
  exit 1
fi
if [[ ! -t 0 ]]; then echo 'Run interactively in your SSH session or Droplet Console.' >&2; exit 1; fi
printf '%s\n' 'UnfoldMail Connect: dedicated DigitalOcean Docker Droplet setup' \
  'Prepare DNS first. Ports 80, 993 and 465 must reach this server.' \
  'Set PROTOCOL_GATEWAY_SECRET on your UnfoldMail Worker before continuing.'
read -r -p 'UnfoldMail HTTPS origin (no trailing slash): ' WORKER_URL
read -r -p 'Protocol hostname (for example protocol.example.com): ' MAIL_HOSTNAME
read -r -p 'Certificate contact email: ' ACME_EMAIL
read -r -s -p 'Shared gateway secret (32+ characters): ' GATEWAY_SECRET
printf '\n'
export WORKER_URL MAIL_HOSTNAME ACME_EMAIL GATEWAY_SECRET
# Validate before writing a dotenv file. Restrict secrets to unambiguous dotenv characters.
python3 - <<'PY'
import os,re,sys
from urllib.parse import urlsplit
u=os.environ['WORKER_URL']; parsed=urlsplit(u)
host=os.environ['MAIL_HOSTNAME']; email=os.environ['ACME_EMAIL']; key=os.environ['GATEWAY_SECRET']
def hostname(s):
 return len(s)<=253 and '.' in s and all(re.fullmatch(r'[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?',label) for label in s.split('.'))
if not re.fullmatch(r'https://[A-Za-z0-9.-]+',u) or not hostname(parsed.hostname or ''):
 sys.exit('Use an HTTPS origin without a path, port, trailing slash, or credentials.')
if not hostname(host):sys.exit('Enter a valid public protocol hostname.')
if not re.fullmatch(r'[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+',email):sys.exit('Invalid certificate email.')
if not re.fullmatch(r'[A-Za-z0-9_-]{32,128}',key):sys.exit('Use a 32-128 character hex or URL-safe gateway secret.')
PY
read -r -p "Agree to the Let's Encrypt subscriber agreement (https://letsencrypt.org/repository/) and start? [yes/no]: " agreement
if [[ "$agreement" != yes ]]; then echo 'Setup cancelled; no configuration written.'; exit 1; fi
umask 077
python3 - <<'PY'
import os
with open('.env','x') as f:
 for name in ['WORKER_URL','MAIL_HOSTNAME','ACME_EMAIL','GATEWAY_SECRET']:
  f.write(name+'='+os.environ[name]+'\n')
 f.write('ENABLE_POP3=false\n')
os.chmod('.env',0o600)
PY
unset GATEWAY_SECRET
# Keep SSH and existing rules intact; add protocol ports only if UFW is active.
if command -v ufw >/dev/null && ufw status | grep -q '^Status: active'; then
  ufw allow 80/tcp
  ufw allow 993/tcp
  ufw allow 465/tcp
fi
docker compose config --quiet
docker compose up -d --build
docker compose ps
printf '%s\n' 'Setup submitted. Check certificate and gateway status:' \
  '  docker compose logs --tail=100 certificates connect' \
  'Then enable IMAP/SMTP and the hostname in UnfoldMail Admin > Mail app access.' \
  'Test login and actual external delivery before inviting users.'
