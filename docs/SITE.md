# Public project site

`https://relay.boniluan.com` serves the English institutional page in `site/`.
It is static HTML/CSS/JavaScript with no build step, external asset dependency,
analytics, API calls, or credentials. Delivery scenarios are illustrative, not
live traffic. Backend features and planned work are distinguished explicitly.

## Deployment boundary

The existing Boniluan edge owns ports 80/443 and TLS. Its repository at
`/home/luan/projects/boniluan` contains the canonical `nginx/relay.conf`, copied
into the edge image by its Dockerfile. Its `compose.yaml` mounts `../relay/site`
read-only at `/usr/share/nginx/relay`. Keep these sibling checkout paths on this
VPS; a missing site directory causes 404 responses. Only the site directory is
mounted, never the Relay repository, environment, database, or signing keyring.

The Relay virtual host serves `/`, `/styles.css`, `/flow.js`, and `/mark.svg`.
Unknown paths (including `/api/`, `/metrics`, and `/.env`) return 404. Only GET
and HEAD are accepted over HTTPS. CSP restricts content to local static assets
and disallows framing and forms. HTTP redirects to the fixed HTTPS hostname,
except for the ACME challenge webroot. HTML and assets require revalidation.

No Relay process, port, network, or database is added by this deployment. The API
remains loopback-only when explicitly started. Kubernetes is not involved.

## Local preview

```bash
python3 -m http.server 18889 --bind 127.0.0.1 --directory site
```

Open `http://127.0.0.1:18889`. Stop the temporary server with Ctrl-C. Check desktop
and narrow mobile layouts, keyboard navigation, all three scenario buttons,
reduced-motion preference, and links. This preview does not reproduce edge headers.

## TLS and updates

DNS is proxied through Cloudflare. The origin has a separate Let's Encrypt
certificate named `relay.boniluan.com`, initially expiring December 12, 2026.
The existing Certbot loop renews all due certificates every 12 hours; the edge
reloads every six hours. Other domains retain their existing certificate.
No keys or ACME account data belong in Git.

To test renewal of only this certificate:

```bash
docker exec boniluan-certbot certbot renew --cert-name relay.boniluan.com --dry-run
```

Static edits are visible through the read-only bind without restarting Nginx.
For routing changes, first validate Compose and a candidate image, then recreate
only the edge web service. This briefly interrupts connections to shared sites;
never restart the whole stack or remove volumes.

```bash
docker compose -p boniluan -f ../boniluan/compose.yaml config --quiet
docker compose -p boniluan -f ../boniluan/compose.yaml build web
docker run --rm --volumes-from boniluan-home \
  --mount type=bind,source=/home/luan/projects/relay/site,target=/usr/share/nginx/relay,readonly \
  --entrypoint nginx boniluan-web:latest -t
docker compose -p boniluan -f ../boniluan/compose.yaml up -d --no-deps --no-build web
```

Check public HTTPS and the origin separately (Cloudflare has its own edge TLS):

```bash
curl --fail https://relay.boniluan.com/
curl --fail --resolve relay.boniluan.com:443:127.0.0.1 https://relay.boniluan.com/
curl -I http://relay.boniluan.com/
curl -I https://relay.boniluan.com/api/
```

Also check Boniluan, FinPulse, Sítio, Vigil, and Vigil's private-route authentication
before and after an edge deployment. Do not disable TLS verification to make a
check pass.

## Rollback

The initial rollout preserves `boniluan-web:before-relay-site-20260913` locally.
To roll back that rollout, retag this image as `boniluan-web:latest`, then run
`up -d --no-deps --no-build --force-recreate web` using the command's project/file
arguments above. Restore the corresponding Boniluan source change separately
before future builds. Keep the certificate volumes and all other services intact.
For later releases, save the current image under a new explicit rollback tag
before building. Static-only changes can be reverted independently in Relay.

## Initial rollout verification (2026-09-13)

- Compose configuration, candidate `nginx -t`, and final edge health passed.
- Chromium checked the public page at 1440, 390, and 320 pixels, all three scenario
  buttons, reduced-motion mode, JavaScript errors, and horizontal overflow.
  Axe WCAG 2 A/AA and 2.1 AA checks reported no violations. Desktop and mobile
  screenshots were visually inspected. Keyboard navigation, reduced-motion
  behavior, and content without JavaScript passed; automated checks are not a full
  accessibility audit.
- Public assets returned 200; HTTP redirected to HTTPS; origin TLS verified
  without bypassing certificate checks. Unknown/private paths returned 404 and
  POST returned 405.
- Boniluan, FinPulse, Sítio, and Vigil root pages retained 200 responses. Vigil's
  private route retained 401 and its metrics route retained 404.
- The certificate-scoped Certbot dry-run renewal succeeded against ACME staging.
- Browser tooling and its libraries/fonts were kept under `/tmp/relay-site-check`;
  no host packages or Relay dependencies were installed. Go code was unchanged.
