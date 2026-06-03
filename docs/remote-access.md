# Remote access

usher has no relay or cloud component. You expose it by running a tunnel on the
same machine that points at usher's local port: usher stays bound to
`127.0.0.1:7777`, and the tunnel handles reaching it from elsewhere. Two
well-supported tunnels are covered below — **Tailscale** (private, your devices
only) and **Cloudflare Tunnel** (a public URL, optionally gated by Cloudflare
Access).

> **Set a password.** usher's bind gate only forces a password when you bind it
> to a *non-loopback* address directly. When a tunnel fronts a loopback usher
> (every setup below), the gate doesn't trip — so set one yourself unless you're
> certain the tunnel is the only way in and no one else shares it.

## Set a password

```
./usher set-password                                        # prompts twice on the terminal
echo -n 'hunter2' | ./usher set-password --password-stdin   # for scripting
```

Plaintext is only read from stdin (never a flag or env var); an empty password
is rejected. Once set, every request goes through a login page.

## Option A — Tailscale

Tailscale puts your machines on a private network (a *tailnet*) that only your
own devices can join. Best when every device is yours.

1. Install Tailscale on the usher machine and on each device you'll use, and log
   both into the same tailnet:

   ```
   tailscale up
   ```

2. Let Tailscale serve usher over HTTPS inside the tailnet. usher keeps running
   on loopback — no port is opened to the public internet:

   ```
   ./usher serve                  # still on 127.0.0.1:7777
   tailscale serve --bg 7777      # proxy https://<machine>.<tailnet>.ts.net → localhost:7777
   ```

   `tailscale serve status` prints the URL; `tailscale serve reset` undoes it.
   (Flag spelling drifts between Tailscale versions — check `tailscale serve --help`.)

3. Open `https://<machine>.<tailnet>.ts.net` from any device on the tailnet, and
   add it to your phone's home screen to install the PWA.

## Option B — Cloudflare Tunnel

Cloudflare Tunnel reaches usher through an outbound-only connection from your
machine — no inbound port. usher stays on loopback and `cloudflared` connects to
it locally. Install
[`cloudflared`](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/)
first.

There are two ways to run it — **pick one**.

### 1. Named tunnel + Cloudflare Access (recommended)

The named (logged-in) tunnel is worth the extra setup because you can put
**Cloudflare Access** in front of it. Access is Cloudflare's Zero Trust access
product (ZTNA) that makes visitors authenticate before they ever reach usher
(it has a free tier). Tunnel and Access are separate products — both part of
Cloudflare One, managed in the Zero Trust dashboard — but you apply the Access
policy to the tunnel's hostname, so they work together. Requires a Cloudflare
account with a domain managed by Cloudflare. Run these in order:

```
cloudflared tunnel login
cloudflared tunnel create usher
cloudflared tunnel route dns usher usher.example.com
```

Create `~/.cloudflared/config.yml`:

```yaml
tunnel: usher
credentials-file: /home/you/.cloudflared/<TUNNEL-UUID>.json
ingress:
  - hostname: usher.example.com
    service: http://localhost:7777
  - service: http_status:404
```

Run it alongside `usher serve`:

```
cloudflared tunnel run usher
```

Finally, in the Cloudflare Zero Trust dashboard, add a self-hosted **Access**
application for `usher.example.com` with a policy that only admits you (the exact
menu path shifts between dashboard revisions). Keep a usher password set as a
second layer behind Access.

### 2. Ephemeral tunnel (quick test only)

No account or domain, but it gives a public, unauthenticated URL — so it leans
entirely on your usher password. One command, alongside `usher serve`:

```
cloudflared tunnel --url http://localhost:7777   # prints a random trycloudflare.com URL
```

## How auth works

- The password is stored as an **argon2id** hash in `<data-dir>/auth.json`
  (mode 0600).
- A separate 32-byte HMAC secret is generated on first start at
  `<data-dir>/secret` and reused across restarts, so cookies survive a restart.
- The login cookie (HttpOnly, SameSite=Lax) is `base64url(HMAC(secret,
  password_hash))` — there is **no server-side session table**. Rotating the password (via `set-password`)
  changes the hash, which invalidates every cookie ever issued; that's the only
  way to forcibly sign out other devices.
- `/login` rate-limits per client IP: the first 5 attempts are free, then
  exponential backoff from the 6th failure (1s → 2s → 4s … capped at 60s); a
  successful login resets the counter.

The permission hook channel is a separate Unix domain socket (mode 0600) and
never traverses the web port, regardless of whether a password is set.

## Threat model

usher's auth defends against:

- Other devices on your LAN or tailnet (the original motivation).
- A compromised tailnet peer trying to talk to your usher.
- Accidental `--addr 0.0.0.0` exposure (the bind gate refuses to start without
  a password).
- A neighboring container that shares your host's network namespace but not its
  filesystem — it can't reach the hook socket or read `auth.json` / `secret`.

It does **not** defend against code running as your own OS user. Such code can
read `auth.json` + `secret` and forge a cookie, read your session jsonl
directly, or just run `claude --resume <id>` itself — bypassing usher entirely.
The OS user account is the trust boundary; if you need more isolation, use a
dedicated UID, container, or sandbox. This is the same posture as code-server,
Jupyter, and most other single-user local web tools.
