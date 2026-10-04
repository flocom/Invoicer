# Invoicer

Self-hosted invoicing in a single, hardened Docker container.

- **Multiple companies**, each with its own clients, numbering, branding, bank details, **Resend** account (e-mails) and **Stripe** account (card payments)
- **Invoices** with multi-rate VAT, PDF generation, public payment page, SEPA QR code (EUR + IBAN)
- **Currencies**: EUR, USD, CAD, CHF, GBP
- **Client language**: each client is EN or FR; invoices, PDFs, e-mails and the payment page follow it. The interface is in English by default (French available per user)
- **Payments**: Stripe Checkout (detected automatically by webhook, or by polling when the server is not publicly reachable) and bank transfers (recorded manually, partial payments supported)
- **Recurring invoices** for subscriptions (weekly / monthly / yearly, every N periods, end date or count, automatic sending)
- **Automatic reminders** before/on/after the due date, on a per-company schedule
- **Automatic, signed updates** from GitHub releases, with backup and rollback
- **Zero required configuration**: the public domain is detected from your browser; the database, encryption key and certificates are created on first start

## Quick start

```bash
docker run -d --name invoicer --restart unless-stopped \
  -p 8080:8080 -v invoicer-data:/data \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
  ghcr.io/flocom/invoicer:latest
```

or, with the provided [docker-compose.yml](docker-compose.yml):

```bash
docker compose up -d
```

Then open the site and get the **one-time setup token** from the logs:

```bash
docker logs invoicer
```

The **first account created becomes the owner** and has ultimate control over the instance. The setup token makes sure nobody else can claim a freshly started server before you.

### HTTPS

Pick one:

1. **Reverse proxy** (recommended if you already have one): Caddy, Traefik, Nginx, Cloudflare Tunnel… pointing at port `8080`. The proxy must forward the `Host` and `X-Forwarded-Proto` headers (Caddy and Traefik do it by default).
2. **Built-in Let's Encrypt**: set `TLS=auto` in `.env` and map ports `80:8080` and `443:8443`. Point your DNS at the server and open `https://your-domain`; the certificate is issued automatically for the domain you use.

Stripe webhooks are configured automatically when the instance is reachable over public HTTPS. Otherwise (local network, testing) payments are checked every 10 minutes.

## Configuration

Everything is optional — an empty `.env` works. See [.env.example](.env.example).

| Variable | Default | Purpose |
| --- | --- | --- |
| `TLS` | *(empty)* | `auto` enables built-in Let's Encrypt certificates |
| `ACME_EMAIL` | *(empty)* | Contact address for Let's Encrypt |
| `DOMAIN` | *(auto)* | Restrict built-in certificates to this host name |
| `AUTO_UPDATE` | `true` | `false` only notifies (critical/mandatory releases are still installed) |
| `TRUST_PROXY` | *(auto)* | `true`/`false` to force trusting `X-Forwarded-*` headers (auto = private networks only) |
| `INVOICER_MASTER_KEY` | *(file)* | 32-byte base64 key to keep the encryption key out of the data volume |
| `PORT` | `8080` | Plain HTTP port inside the container |

Per-company settings (Resend, Stripe, bank details, numbering, reminders…) are managed in the web interface. Secrets are encrypted at rest.

## Setting up a company

1. **Settings → General**: legal name, address, VAT and registration numbers, logo, brand colour.
2. **Settings → Invoicing**: currency, document language, VAT rate, numbering prefix (`INV-2026-0001`, sequential per year), payment terms, legal footer, bank details, reminder schedule (e.g. `-3,0,7,15,30` days relative to the due date).
3. **Settings → E-mail**: a [Resend](https://resend.com) API key and a sender on a verified domain. Send a test.
4. **Settings → Payments**: a Stripe secret key, or better a *restricted* key with write access to *Checkout Sessions* and *Webhook Endpoints*. The webhook is created for you.
5. **Settings → Access**: choose which members can see the company.

Issued invoices are immutable (numbering without gaps, buyer/seller details frozen at issue time); void and duplicate an invoice to correct it.

## Security

- Single static Go binary on a distroless, non-root image (~20 MB), read-only root filesystem, no shell
- Argon2id password hashing, progressive account lockout, rate limiting, optional TOTP 2FA with recovery codes
- Session tokens stored hashed, `HttpOnly`/`Secure`/`SameSite` cookies, session rotation on login
- CSRF tokens on every form plus `Origin` checks; strict Content-Security-Policy without inline scripts or styles; HSTS, `X-Frame-Options`, `nosniff`…
- API keys and 2FA secrets encrypted with AES-256-GCM, bound to their record
- Uploaded logos are decoded and re-encoded; public invoice links use 192-bit random tokens
- Stripe webhooks verified (HMAC-SHA256, timestamp tolerance) and re-fetched from the Stripe API
- Audit log of sensitive actions; the e-mail links use the recorded public URL so a forged `Host` header cannot poison them

## Updates

Every release publishes Linux binaries and a `manifest.json` signed with Ed25519. The public key is compiled into Invoicer, so an update is only installed if:

- the manifest signature is valid,
- its version is newer than the running one,
- the binary matches the signed SHA-256.

The instance checks every 6 hours (and shortly after start), takes a database backup, swaps binaries and restarts in about a second. A version that fails to start three times is skipped automatically. Releases can be flagged *critical* or set a *minimum version* to force the update everywhere.

Force it yourself:

- **System → Check now / Install now** in the interface (owner), or
- `docker exec invoicer /app/invoicer update`

Pulling a newer image (`docker compose pull && docker compose up -d`) works too.

## Backups

The whole state lives in the `/data` volume: `invoicer.db` (SQLite), `master.key` (encryption key — **back it up**, without it stored API keys cannot be decrypted), certificates and installed updates. A consistent backup is written every day to `/data/backups` (14 kept) and before each update.

```bash
docker run --rm -v invoicer-data:/data -v "$PWD":/out alpine tar czf /out/invoicer-backup.tgz -C /data .
```

## Command line

```bash
docker exec invoicer /app/invoicer version
docker exec invoicer /app/invoicer update                       # install the latest signed release
docker exec invoicer /app/invoicer reset-password you@example.com  # one-time reset link (e.g. lost owner password)
```

## Development

```bash
INVOICER_DATA=./.devdata PORT=8090 go run ./cmd/invoicer
go test ./...
```

### Releasing

Releases are built and signed by [.github/workflows/release.yml](.github/workflows/release.yml) when a `v*.*.*` tag is pushed. The workflow needs the repository secret `RELEASE_SIGNING_KEY` (the private key matching [internal/updater/key.go](internal/updater/key.go)). To rotate keys: `go run ./cmd/release keygen`, put the new public key in `key.go`, publish that release while the secret still holds the old key, then replace the secret with the new private key for the following releases.

The Docker image is pushed to `ghcr.io/flocom/invoicer`. After the first release, make the package public in *GitHub → Packages → invoicer → Package settings*.

---

### En bref (français)

Outil de facturation auto-hébergé, tout-en-un dans un conteneur Docker durci. Multi-entreprises (chacune avec son compte Resend et son compte Stripe), factures PDF en anglais ou en français selon le client, 5 devises, paiements par carte (Stripe) ou virement, factures récurrentes, relances automatiques, mises à jour automatiques signées depuis GitHub. Aucune variable d'environnement obligatoire : lancez le conteneur, récupérez le jeton d'installation avec `docker logs invoicer`, et le premier compte créé devient propriétaire.
