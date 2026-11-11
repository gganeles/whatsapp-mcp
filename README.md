# whatsapp-mcp

An [MCP](https://modelcontextprotocol.io) server that links to your WhatsApp account as a companion
device (the same way WhatsApp Web does), keeps a searchable local copy of your chats, and lets Claude
read, search, and send messages. Built on [whatsmeow](https://github.com/tulir/whatsmeow).

It runs in two ways:

- **As a remote connector** (recommended): one always-on server that Claude uses from the web,
  desktop, and mobile apps and from routines. It can host several people's WhatsApp accounts, each
  fully isolated, and signs clients in with OAuth.
- **As a local server** over stdio, for Claude Code or Claude Desktop on the same computer.

Session keys and messages stay on the machine that runs the server.

## Accounts

Every account is one WhatsApp phone number with its own password. Each account has its own
WhatsApp session, message database, and files folder under `DATA/accounts/<number>/`, and a
signed-in client can only ever reach the account it signed in as:

- The sign-in page asks for the phone number and that account's password, and every OAuth code and
  token is bound to that one account.
- Each request is served from the account its access token belongs to, never from the MCP session,
  so reusing someone else's session ID gets you your own data.
- An account can only be linked to its own WhatsApp number. Linking a different phone is detected
  and immediately unlinked.
- File tools are sandboxed to the account's own files folder.

## Remote connector setup (Docker + Cloudflare Tunnel)

You need a machine that stays on (a home server, a Raspberry Pi, a small VPS, or a Mac that doesn't
sleep) with Docker, and a domain on Cloudflare. The tunnel gives you HTTPS without opening any ports.

1. **Create a tunnel.** In the Cloudflare dashboard go to Zero Trust > Networks > Tunnels > Create a
   tunnel (type Cloudflared). Copy the token from the install command. Add a public hostname such as
   `whatsapp.example.com` with service `http://whatsapp-mcp:8080`.

2. **Configure it and create an account.**

   ```sh
   git clone https://github.com/gganeles/whatsapp-mcp && cd whatsapp-mcp
   cp .env.example .env        # fill in WHATSAPP_MCP_PUBLIC_URL and CLOUDFLARE_TUNNEL_TOKEN
   docker compose run --rm whatsapp-mcp user add +14155551234
   ```

   Type a password (at least 12 characters) or press Enter to generate one. Repeat for each person
   who should have an account, and give them their number's password. Accounts can be added and
   removed while the server runs.

3. **Start it.**

   ```sh
   docker compose up -d
   ```

4. **Add the connector.** In claude.ai go to Settings > Connectors > Add custom connector and enter
   `https://whatsapp.example.com/mcp`. A sign-in page from your server opens; enter your WhatsApp
   number and your account's password, and allow. The connector then works in the web, desktop, and
   mobile apps. Each person adds the same URL to their own Claude and signs in with their own number.

5. **Link WhatsApp.** Ask Claude to "link my WhatsApp" (or call `pair_device`). It shows an
   8-character code for your account's number. On your phone open WhatsApp > Settings > Linked devices > Link a
   device > **Link with phone number instead**, and enter it. History then syncs in the background;
   `get_status` shows progress.

To use it from Claude Code too: `claude mcp add --transport http whatsapp https://whatsapp.example.com/mcp`.

To update, `git pull` and run `docker compose up -d --build`. Accounts, passwords, connector
sign-ins and the WhatsApp link all live in the `whatsapp-data` volume, which a rebuild keeps, so
nobody has to sign in again (see [Updating keeps everyone signed in](#updating-keeps-everyone-signed-in)).

### On a Linux server without Docker

`linux/install.sh` installs the server natively on any Linux: Oracle Linux, RHEL, Rocky, Alma,
Fedora, Amazon Linux, Debian, Ubuntu, openSUSE, Arch, or Alpine, on x86-64 or ARM (such as Oracle
Cloud's Ampere machines). First create the Cloudflare tunnel as in step 1 above, but point its public
hostname at `http://localhost:8080`. Then:

```sh
git clone https://github.com/gganeles/whatsapp-mcp && cd whatsapp-mcp
sudo sh linux/install.sh
```

The installer installs `curl` and `tar` with the system's package manager if they're missing,
downloads its own Go toolchain (the distro's Go is usually too old), builds the server, and asks for
your public URL and tunnel token. It then offers to create an account and starts the server and
tunnel as systemd (or OpenRC) services that start on boot and restart if they exit. The server runs
as its own `whatsapp-mcp` user, listens only on `127.0.0.1:8080`, and keeps its data in
`/var/lib/whatsapp-mcp`. Leave the token empty to use your own HTTPS reverse proxy instead of a
tunnel.

Manage accounts with `sudo whatsapp-mcp-admin user add|passwd|remove|list`, and the services with
`systemctl status|restart whatsapp-mcp whatsapp-mcp-tunnel` (logs: `journalctl -u whatsapp-mcp -f`).
Settings live in `/etc/whatsapp-mcp/env`. To update, `git pull` and run the installer again; it keeps
your settings, accounts and passwords, and everyone stays signed in. Press Enter at its account
prompt: there's no need to add existing accounts again.

#### Using a prebuilt binary

Building takes a while on a small VM. Every push to `main` builds Linux binaries for amd64, arm64
and 32-bit ARM and publishes them on the repository's
[latest release](https://github.com/gganeles/whatsapp-mcp/releases/tag/latest), so you can skip the
build entirely:

```sh
sudo sh linux/install.sh --download
```

The installer picks the binary for the server's CPU, checks it against the release's `SHA256SUMS`,
and does the rest as usual. To update, `git pull` and run it again with `--download`.

To compile it yourself instead: the server is pure Go with no C dependencies, so you can
compile it on any faster computer (Linux, macOS, or Windows with Go 1.26+) for the server's CPU and
copy it over. Use `amd64` for Intel/AMD servers and `arm64` for ARM ones such as Oracle Cloud's
Ampere A1 (`uname -m` on the server prints `x86_64` or `aarch64`):

```sh
# on your computer, in the repository
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o whatsapp-mcp-linux-amd64 .
scp whatsapp-mcp-linux-amd64 you@server:

# on the server, in the repository
sudo sh linux/install.sh --binary ~/whatsapp-mcp-linux-amd64
```

(In Windows PowerShell set the variables with `$env:CGO_ENABLED=0; $env:GOOS="linux"; $env:GOARCH="amd64"` first.)
The installer then skips downloading Go and building, checks the binary runs on this machine, and
does the rest as usual. Build the binary from the same version of the repository as the server's
checkout.

### On an Android phone (Termux)

You can host the server on the phone itself with [Termux](https://termux.dev), no Docker needed.
First create the Cloudflare tunnel as in step 1 above, but point its public hostname at
`http://localhost:8080`. Then, in Termux:

```sh
pkg install git
git clone https://github.com/gganeles/whatsapp-mcp && cd whatsapp-mcp
bash termux/install.sh
```

The installer builds the server, asks for your public URL and tunnel token, offers to create an
account, and installs a [Termux:Boot](https://wiki.termux.com/wiki/Termux:Boot) script so the server
and tunnel start when the phone boots. They restart automatically if either one exits. Install the
Termux:Boot app (from the same store as Termux) and open it once, and set Termux's battery usage to
Unrestricted so Android doesn't stop it.

Manage it with `whatsapp-mcp-service start|stop|restart|status|logs`. Settings live in
`~/.config/whatsapp-mcp/env`. To update, `git pull` and run the installer again.

### Other ways to host

Any HTTPS reverse proxy works: Tailscale Funnel, Caddy, nginx. Run the binary or container with
`--http 127.0.0.1:8080 --public-url https://your.host` and point the proxy at it. If the proxy sends a
`Host` header other than your public hostname, add it with `--allowed-hosts`.

## Local setup (stdio)

Requires Go 1.26+. No C compiler is needed.

```sh
go install github.com/gganeles/whatsapp-mcp@latest
whatsapp-mcp login --phone +14155551234     # add --qr to scan a QR code instead
claude mcp add whatsapp -- whatsapp-mcp serve --phone +14155551234
```

For Claude Desktop, add it to `claude_desktop_config.json`:

```json
{ "mcpServers": { "whatsapp": { "command": "/path/to/whatsapp-mcp", "args": ["serve"] } } }
```

Local mode needs no password because only your own computer can reach it. Only one process can use a
WhatsApp session at a time, so don't run the local and remote setups against the same data
directory at once.

## Managing accounts

| Command | What it does |
| --- | --- |
| `user add +NUMBER` | Create an account and set its password |
| `user passwd +NUMBER` | Change the password and sign out that account's clients |
| `user remove +NUMBER [--delete-data]` | Remove the account, sign out its clients, and unlink its WhatsApp |
| `user list` | Show accounts, signed-in clients, and whether each is linked |
| `revoke-tokens [--phone +NUMBER]` | Sign out one account's clients, or everyone's |

In Docker, prefix them with `docker compose run --rm whatsapp-mcp`.

## Tools

| Tool | What it does |
| --- | --- |
| `get_status` | Linked/connected state, sync progress, pending pairing code |
| `pair_device` | Link the account with a pairing code (or a QR image with `use_qr`) |
| `list_chats` | Chats by recent activity, with last message; filter by name, group/direct |
| `get_chat` | Chat details; group topic and participants |
| `list_messages` | Messages in a chat, with time range and sender filters |
| `search_messages` | Full-text search across all chats or within one |
| `get_message_context` | Messages around a given message, such as a search hit |
| `search_contacts` | Find a contact or group by name or number |
| `send_message` | Send text, optionally as a quote-reply |
| `send_file` | Send an image, video, audio, voice note, or document |
| `send_reaction` | React to a message with an emoji |
| `download_media` | Get a message's attachment (images inline, documents as a file) |
| `request_history` | Ask the phone for older messages in a chat |

Wherever a chat is expected you can pass a JID (`14155551234@s.whatsapp.net`, `...@g.us`), a phone
number, or a chat name that matches exactly one chat.

## Security

- **OAuth 2.1 with PKCE.** Clients register dynamically and must be approved on a consent page with
  an account's phone number and password (bcrypt). Only SHA-256 hashes of codes and tokens are
  stored. Access tokens last an hour; refresh tokens rotate on every use and expire after 90 days
  of disuse.
- **Redirect allowlist.** Only Claude's callback URLs and loopback addresses (for local apps) can
  register, so a stranger can't set up a client that sends codes to their own site. Add more with
  `--allow-redirect`.
- **Brute-force protection.** Five failed sign-ins lock that account for 15 minutes, 30 failures
  across all accounts lock sign-in entirely, and registrations are rate limited. Wrong numbers and
  wrong passwords get the same answer in the same time, so the page doesn't reveal who has an account.
- **Hardened HTTP.** Unknown `Host` headers are rejected, request sizes and header timeouts are
  capped, and pages send a strict CSP, `frame-ancestors 'none'`, and HSTS.
- **File sandbox.** `send_file` only reads from the account's files folder or takes the file's bytes
  directly; it can't reach session databases, other accounts, or anything else on the server.
- **Prompt-injection guidance.** The server tells the model that message contents are untrusted and
  that it must not send anything because a message asks it to.
- **Read-only mode.** `--read-only` (or `WHATSAPP_MCP_READ_ONLY=true`) removes every sending tool.
- **Private data.** Databases are created owner-only (0600). The container runs as non-root on a
  read-only filesystem with all capabilities dropped.


## Configuration

Every flag can also be set by environment variable.

| Flag | Variable | Default |
| --- | --- | --- |
| `--data` | `WHATSAPP_MCP_DATA` | `~/.whatsapp-mcp` (`/data` in Docker) |
| `--phone` | `WHATSAPP_PHONE` | account for `login`, `logout`, and stdio `serve` |
| `--http` | `WHATSAPP_MCP_HTTP` | unset (stdio) |
| `--public-url` | `WHATSAPP_MCP_PUBLIC_URL` | required with `--http` |
| `--read-only` | `WHATSAPP_MCP_READ_ONLY` | `false` |
| `--allow-redirect` | `WHATSAPP_MCP_ALLOW_REDIRECTS` | Claude's callbacks and loopback |
| `--allowed-hosts` | `WHATSAPP_MCP_ALLOWED_HOSTS` | the public URL's host |
| `--log-level` | `WHATSAPP_MCP_LOG_LEVEL` | `info` |
Data directory layout: `auth.db` holds accounts (hashed passwords), OAuth clients and hashed tokens.
Each `accounts/<number>/` holds `whatsapp.db` (session keys; treat it like a password),
`messages.db` (messages with a full-text index) and `files/`.

## Updating keeps everyone signed in

The server has no signing key or secret that changes between builds. Everything that keeps a
connector signed in is a row in `auth.db` in the data directory: the hashed passwords, the
registered OAuth clients and the hashed access and refresh tokens. A new binary or image reads the
same rows, so a client signed in before the update keeps working after it, and its refresh token
keeps renewing for 90 days of inactivity. Updating needs no `user add` or `user passwd`.

Everyone has to sign in again only when one of these happens:

- **`user passwd`**, `user remove` or `revoke-tokens`. These sign the account's clients out on
  purpose. Don't use them as part of an update. `user add` for a number that already has an account
  is refused and changes nothing.
- **The data directory is lost.** With Docker, `docker compose down -v`, `docker volume rm` or
  `docker volume prune` deletes the `whatsapp-data` volume; plain `docker compose down` and
  `up --build` keep it. A `docker run` without `-v whatsapp-data:/data` starts from an empty
  `/data` each time. On Linux, the installer never touches `/var/lib/whatsapp-mcp`; only deleting it,
  or rebuilding the VM without its disk, loses it. Back up the data directory to keep accounts across
  a move to a new machine.
- **The public URL changes.** Tokens are issued for `WHATSAPP_MCP_PUBLIC_URL`/mcp, and the connector
  in Claude points at that address, so a new hostname means a new connector.
- **`user add` without `whatsapp-mcp-admin`** on an installed Linux server writes to
  `~/.whatsapp-mcp`, which the service never reads. Use `sudo whatsapp-mcp-admin user …`.

## Unlink

`user remove +NUMBER --delete-data` unlinks an account's WhatsApp and deletes its data. Locally,
`whatsapp-mcp logout` unlinks the device. You can also remove the device from the phone under
Linked devices.

If the device is removed while the server runs (from the phone, or by WhatsApp itself), the server
drops the dead session and the tools say so; `get_status` shows `device_removed` with WhatsApp's
reason. Call `pair_device` to link again, with no restart needed. Stored chats and messages are
kept. Contacts are part of the WhatsApp session, so `search_contacts` is empty until WhatsApp syncs
them again after re-linking.

## Notes

- This uses WhatsApp's multi-device web protocol through an unofficial library. Automated or bulk
  messaging can get an account banned, so keep sending to normal, personal use.
- WhatsApp decides how much history a newly linked device receives. `request_history` pulls more for
  a chat while your phone is online.
- WhatsApp unlinks companion devices if the phone stays offline for about two weeks.
- Media from old messages may no longer be downloadable once WhatsApp's servers expire it.
