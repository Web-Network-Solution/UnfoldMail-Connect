# UnfoldMail Connect

**Connect your mail app to UnfoldMail.**

UnfoldMail Connect is the optional IMAP, SMTP submission, and POP3 extension for
[UnfoldMail](https://github.com/Web-Network-Solution/UnfoldMail). It is free,
MIT-licensed software from [Web Network Solutions](https://webnetworksolution.com).
You pay your hosting provider for the server and network usage.

The gateway connects Outlook and other compatible mail apps to an existing
UnfoldMail installation. Mailboxes, device passwords, storage, and delivery stay
in UnfoldMail. This is not a standalone mail server or a connector for arbitrary
third-party SMTP servers. Gmail support means clients that allow an external
IMAP account; it does not mean signing into gmail.com with an UnfoldMail account.

Source: [Web-Network-Solution/UnfoldMail-Connect](https://github.com/Web-Network-Solution/UnfoldMail-Connect).

## Deploy on DigitalOcean

[![Create DigitalOcean Droplet](deployment/digitalocean/button.svg)](https://cloud.digitalocean.com/droplets/new?appId=241490599&image=docker-20-04)

Creates a Docker-ready server. Then follow the [guided Connect setup](deployment/digitalocean/README.md).
This is not yet a dedicated UnfoldMail Connect Marketplace image.

## How it works

Mail app -> TLS IMAP/SMTP -> UnfoldMail Connect -> HTTPS -> UnfoldMail Worker

| Feature | Public port | Container port |
| --- | --- | --- |
| IMAP over TLS | 993 | 1993 |
| Authenticated SMTP over TLS | 465 | 1465 |
| Optional POP3 over TLS | 995 | 1995 |
| Certificate HTTP validation | 80 | 80, certificate service |

POP3 is disabled by default. The gateway has no separate application database or
mail queue to provision. Cloudflare Email Service remains responsible for sending
mail externally; this gateway does not deliver directly to recipient MX servers.
Port 25 is not required by this deployment.

## Deployment status

Included: Go source and tests, a non-root Docker image definition, Docker Compose,
initial Let's Encrypt certificate issuance, a renewal loop, and a CI workflow.

The portable deployment is currently **one command after server, DNS, and environment
setup**. A provider button that creates the server itself is not published yet.
Do not advertise AWS, DigitalOcean, or Railway one-click buttons until their
provider templates are published and tested.

| Target | Current path | Remaining work for a provider button |
| --- | --- | --- |
| DigitalOcean Droplet / other Linux VPS | [Docker Droplet button and guided setup](deployment/digitalocean/README.md) | Provisioning template or Marketplace image, setup form, and end-to-end test |
| AWS EC2 | Docker Compose on a dedicated instance | CloudFormation template, stable IP, networking, secure secret delivery, and hosted launch template |
| Coolify | Compose deployment with the certificate arrangement adapted to the host | Reusable service template and certificate integration; existing proxies normally occupy port 80 |
| Railway | Not a drop-in target for this Compose stack | Separate protocol services/proxies, custom public port settings in UnfoldMail, and a supported certificate lifecycle |

Railway assigns the external TCP port and currently supports one TCP proxy per
service. UnfoldMail currently displays standard protocol ports. These differences
must be addressed before promising a compatible Railway template.
See [Railway TCP proxy](https://docs.railway.com/networking/tcp-proxy) and
[proxy limits](https://docs.railway.com/cli/tcp-proxy).
AWS provides [CloudFormation quick-create links](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/cfn-console-create-stacks-quick-create-links.html).
DigitalOcean offers [Marketplace Droplet images](https://docs.digitalocean.com/products/marketplace/).

## Deploy on a dedicated Linux VPS

### 1. Prepare the server and DNS

- Install Docker Engine and the Docker Compose v2 plugin.
- Use a dedicated server with free TCP ports 80, 993, and 465. Enable 995 only
  if using POP3. Allow these ports in the provider firewall and host firewall.
- Allow outbound HTTPS for the UnfoldMail API, certificate authority, and image downloads.
- Create a DNS-only A record such as `protocol.example.com` pointing to this server.
  Add an AAAA record only when IPv6 reaches this same server correctly.
- If using Cloudflare DNS, leave this hostname **DNS only**. The ordinary HTTP
  proxy is not the mail protocol endpoint.
- The supplied resource limits retain the original gateway profile: 4 CPUs and
  a 6 GiB container memory cap, with additional headroom needed for the host and
  certificate service. These are configuration limits, not a guaranteed user capacity.

The bundled certificate service requires public port 80 for HTTP-01 validation
and renewal. Do not run this stack unchanged on a host where Coolify, Traefik,
Nginx, or another service already publishes port 80. Use a dedicated host or
integrate certificate issuance with that host's existing proxy first.

### 2. Configure the extension

Clone the repository on the server:

```sh
git clone https://github.com/Web-Network-Solution/UnfoldMail-Connect.git
cd UnfoldMail-Connect
cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

Save the generated value privately. Edit `.env`:

| Variable | Value |
| --- | --- |
| `WORKER_URL` | UnfoldMail HTTPS origin, e.g. `https://mail.example.com`, without a trailing slash |
| `MAIL_HOSTNAME` | Public mail-app hostname, e.g. `protocol.example.com` |
| `ACME_EMAIL` | Administrator contact email for the certificate account |
| `GATEWAY_SECRET` | Generated shared secret, identical to the Worker's `PROTOCOL_GATEWAY_SECRET` |
| `ENABLE_POP3` | Keep `false` unless POP3 is needed |

Never commit `.env`, private keys, or device passwords. Do not reuse the
UnfoldMail installation/bootstrap secret as the gateway secret.

### 3. Connect the Worker

In Cloudflare, open the deployed UnfoldMail Worker -> Settings -> Variables and
Secrets. Add **PROTOCOL_GATEWAY_SECRET** as a secret, using exactly the gateway's
`GATEWAY_SECRET` value, and deploy the change.

In UnfoldMail, open **Admin -> Mail app access**. Set the gateway hostname and
enable IMAP and SMTP. Leave POP3 disabled unless required. Ensure the employee's
mail-app access is enabled. The existing UnfoldMail installation must include
its protocol database migrations and `/api/protocol-gateway` endpoints.

### 4. Start

```sh
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 certificates connect
```

The certificate service accepts the Let's Encrypt subscriber agreement as part
of issuance, obtains a certificate, and exports it for the non-root gateway.
The gateway starts after certificate files exist. Certificate state and exported
keys persist in named Docker volumes. The certificate service checks renewal
approximately every 12 hours; the gateway reloads certificates during subsequent
TLS handshakes after its one-minute reload interval. Monitor renewal logs and
certificate expiration. See [Certbot documentation](https://eff-certbot.readthedocs.io/en/stable/using.html).

This flow requires working DNS and domain validation. It does not request your
Cloudflare API token. The gateway health check tests its local HTTP health endpoint;
it does not prove upstream authentication, sending, or external delivery.

### 5. Set up a mail app

In UnfoldMail **Settings -> Mail apps**, create a separate device password for
each device. Use the full email address as the username for both protocols.

| Setting | Value |
| --- | --- |
| Incoming type | IMAP |
| Incoming server | Your `MAIL_HOSTNAME` |
| Incoming port / security | 993 / SSL-TLS |
| Outgoing server | Your `MAIL_HOSTNAME` |
| Outgoing port / security | 465 / SSL-TLS |
| SMTP authentication | Required; full email address and device password |
| Password | Generated device password, not the normal webmail password |

This deployment uses implicit TLS, not STARTTLS on port 587. Current SMTP limits
are 5 MiB per complete message (including encoding overhead) and 50 recipients.
POP3 can delete server copies; prefer IMAP for synchronized folders.

### 6. Verify before inviting employees

From a separate machine:

```sh
openssl s_client -connect protocol.example.com:993 -servername protocol.example.com -verify_hostname protocol.example.com -verify_return_error </dev/null
openssl s_client -connect protocol.example.com:465 -servername protocol.example.com -verify_hostname protocol.example.com -verify_return_error </dev/null
```

Use your hostname in these commands. Confirm certificate validation succeeds.
Then test device-password login, inbox synchronization, an outgoing message to
an external mailbox, and a reply. Confirm actual receipt and backend sending logs;
a message in Sent alone does not establish successful delivery.

## Operations and updates

- Keep the health and metrics port 8080 private; it is not published by Compose.
- Keep certificate volumes and `.env` across updates. Do not use `docker compose down -v`
  unless you intentionally want to remove certificate state.
- Deploy reviewed versions with `docker compose up -d --build`. Back up configuration
  and certificate state securely before changing infrastructure.
- Preserve the shared secret in both applications when moving servers; rotate both
  together if compromised. Device access is managed in UnfoldMail.
- Review memory and connection limits against actual workload. This repository
  does not certify support for a particular team size.
- Monitor certificate renewal, upstream failures, and gateway availability.

## Development

Use the Go version required by `go.mod` or a compatible newer version.

```sh
go test ./...
go vet ./...
go build -o bin/unfoldmail-connect .
```

CI additionally runs race tests, Compose validation, shell syntax checks, and a
Docker build. The internal Go module remains `mail-protocol` for continuity;
the distributed product and container binary are named UnfoldMail Connect.

## Before a public one-click release

1. Run the repository CI workflow successfully and publish a tested release tag.
2. Publish versioned container images for the CPU architectures supported by the templates.
3. Build and test the selected provider template, including networking, cost disclosure,
   certificate renewal, and secure handling of the shared secret.
4. Test the Docker/ACME flow with a real fresh server and domain. Local Go tests do
   not prove certificate issuance or Docker networking.
5. Verify compatibility against a tagged UnfoldMail version, including delivery and
   device-password revocation. Add this version pairing to release notes.
6. Complete a dependency/security review and realistic concurrency testing before
   making production-readiness or team-size guarantees.

## License and ownership

Copyright (c) 2026 Web Network Solutions. Released under the [MIT license](LICENSE).
Dependencies retain their own licenses. This project is provided without a hosting
or support SLA. For private security reporting, contact
[Web Network Solutions](https://webnetworksolution.com) before sending sensitive details.
