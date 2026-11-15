# marketplace

A small, mobile-friendly giveaway site for friends. Post things you're getting
rid of with photos and descriptions, and friends claim them. Everything is free.

- **Per-item claiming**: 🎲 *lottery* (people enter until a deadline, then a
  random draw picks the winner; with no entrants the item falls back to first
  come), ⚡ *first come, first served*, or ✋ *requests, I choose*.
- **Handoff**: *must pick up* / *I can drop off* / *flexible*, plus a free-text
  note ("3rd floor, bring 2 people").
- **Available from**: claim now, hand off later, for things you're still using.
- **Invite-only email magic-link sign-in**: only admins and emails on the
  invite list (Admin → People) can sign in. Others get the same "check your
  email" page but no email, so bots can't sign up or make the app email
  strangers. Removing someone from the list signs them out. Set
  `OPEN_SIGNUP=true` to let anyone in.
- **New-item emails**: batched, sent once you've stopped posting for
  `NEW_ITEMS_DELAY`. Each email has a one-click unsubscribe.
- **Winner emails**, and admin emails on claims, requests and give-backs.
- **Give it back**: a winner can release an item. Lotteries redraw among the
  remaining entrants.
- **Handoffs checklist** (Admin → Handoffs): claimed items grouped by person,
  with a tick box for each handoff.
- **Hidden drafts**: draft several items, then publish them together.

A single Go binary with SQLite (pure Go, no cgo). Templates and assets are
embedded. Photos are resized in the browser before upload, which also fixes
phone rotation, and are stored under `DATA_DIR/photos`.

## Run locally

```sh
ADMIN_EMAILS=you@example.com SITE_TITLE="My Move-Out Giveaway" go run .
```

With no mail transport configured, emails (including sign-in links) are
printed to the log.

## Configuration

| Variable | Default | |
|---|---|---|
| `ADMIN_EMAILS` | required | Comma-separated admins |
| `SITE_TITLE` | `Free Stuff` | Name shown in the header and emails |
| `BASE_URL` | `http://localhost:8080` | Public URL for emailed links; `https://` marks cookies Secure |
| `ADDR` | `:8080` | Listen address |
| `DATA_DIR` | `./data` | SQLite DB + photos |
| `NEW_ITEMS_DELAY` | `10m` | Quiet period before the new-items email |
| `OPEN_SIGNUP` | `false` | Let anyone with an email sign in, ignoring the invite list |
| `MAIL_FROM` | | e.g. `Free Stuff <stuff@mail.example.com>` |
| `RESEND_API_KEY` / `_FILE` | | Send via Resend (takes precedence) |
| `SMTP_HOST`, `SMTP_PORT` (587), `SMTP_USERNAME`, `SMTP_PASSWORD` / `_FILE` | | Send via SMTP |

## NixOS

```nix
inputs.marketplace.url = "git+ssh://git@github.com/<you>/marketplace?ref=release";
# ...
imports = [ inputs.marketplace.nixosModules.default ];
services.marketplace = {
  enable = true;
  port = 8017;
  baseUrl = "https://stuff.example.com";
  siteTitle = "My Move-Out Giveaway";
  adminEmails = [ "you@example.com" ];
  mail.from = "Free Stuff <stuff@mail.example.com>";
  mail.resendApiKeyFile = config.sops.secrets."marketplace/resend_api_key".path;
};
```

State lives in `/var/lib/marketplace`. Rate-limit `POST /login` at the reverse
proxy. The app also applies a 1-minute per-email cooldown.

After changing Go dependencies, update `vendorHash` in `flake.nix`.
