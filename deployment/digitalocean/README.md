# DigitalOcean Droplet deployment

[![Create Docker Droplet](button.svg)](https://cloud.digitalocean.com/droplets/new?appId=241490599&image=docker-20-04)

This button opens DigitalOcean's **Docker Droplet** creation page. It does not
create a preconfigured UnfoldMail Connect Marketplace image. Finish the short
setup below after creating the server. DigitalOcean charges for the Droplet.

1. Choose a region close to your users, an SSH key, and a server with at least
   4 vCPUs / 8 GiB RAM for the current gateway limits and host overhead.
   Review the displayed price before creating it. This is not a user-capacity guarantee.
2. Create a DNS-only A record for `protocol.example.com` pointing to its public IPv4.
   Add IPv6 only if it is configured and reachable. Allow inbound TCP 80, 993, 465
   in any DigitalOcean Cloud Firewall; restrict SSH 22 to your administration IP.
   Leave port 8080 private. Outbound HTTPS must work.
3. On your computer, generate a shared secret (`openssl rand -hex 32`). Add it to
   the UnfoldMail Worker as the secret `PROTOCOL_GATEWAY_SECRET`. Keep it for setup.
4. Connect using SSH or the DigitalOcean Droplet Console. Run:

   ```sh
   git clone https://github.com/Web-Network-Solution/UnfoldMail-Connect.git
   cd UnfoldMail-Connect
   sudo bash deployment/digitalocean/setup.sh
   ```

5. Enter the webmail origin, protocol hostname, certificate email, and shared secret.
   Secret input is hidden. Setup creates a root-readable `.env`, opens the required
   ports in UFW if active, and starts Docker Compose. It never disables the firewall
   or replaces an existing `.env`. The provider Cloud Firewall must be configured
   separately as in step 2.
6. In UnfoldMail Admin -> Mail app access, enable IMAP/SMTP and enter the hostname.
   Create a device password in Settings -> Mail apps, then test inbox synchronization
   and sending to an external recipient. See the [main README](../../README.md).

A dedicated Droplet avoids port 80 conflicts with another proxy. Let's Encrypt
needs DNS to resolve to this server before startup. Certificate state persists in
Docker volumes and renewals are checked automatically. Monitor renewal failures.
Do not put gateway secrets in Droplet startup scripts: user data is available
through instance metadata. This setup collects the secret after server creation.

## What is still needed for a true one-click Marketplace app

A dedicated UnfoldMail Connect Marketplace image must be built, tested, and
submitted through DigitalOcean's vendor process. It needs a secure first-run
configuration flow and release maintenance. The Docker Droplet button above is
an immediately usable guided deployment route, not that Marketplace listing.

References: [official Docker image](https://marketplace.digitalocean.com/apps/docker),
[Marketplace vendor information](https://docs.digitalocean.com/products/marketplace/),
[Droplet user data](https://docs.digitalocean.com/products/droplets/how-to/provide-user-data/).
