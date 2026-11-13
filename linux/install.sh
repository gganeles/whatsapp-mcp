#!/bin/sh
# Installs the WhatsApp MCP server natively (no Docker) on Linux and runs it as a service.
#
# Run it from the repository checkout, as root:
#
#   git clone https://github.com/gganeles/whatsapp-mcp && cd whatsapp-mcp
#   sudo sh linux/install.sh
#
# On a small machine, skip the build: --download fetches the binary that GitHub builds
# from main, and --binary uses one you compiled elsewhere (see the README):
#
#   sudo sh linux/install.sh --download
#   sudo sh linux/install.sh --binary ./whatsapp-mcp-linux-amd64
#
# Works with apt (Debian, Ubuntu), dnf/yum (Oracle Linux, RHEL, Rocky, Alma, Fedora,
# Amazon Linux), zypper (openSUSE, SLES), pacman (Arch) and apk (Alpine), on systemd or
# OpenRC. It downloads its own Go toolchain, so the distro's Go version doesn't matter.
#
# It is safe to run again: to update, `git pull` and rerun it. Saved settings, accounts,
# passwords and signed-in clients are all kept, so nobody has to sign in again.
#
# To run without prompts, set any of these in the environment first:
#   WHATSAPP_MCP_PUBLIC_URL   the public https:// address of the server
#   CLOUDFLARE_TUNNEL_TOKEN   a Cloudflare tunnel token (leave unset to skip the tunnel)
#   WHATSAPP_MCP_HTTP         listen address (default 127.0.0.1:8080)
#   WHATSAPP_MCP_READ_ONLY    true to disable every tool that sends messages
#   WHATSAPP_MCP_NONINTERACTIVE=1
#   WHATSAPP_MCP_BINARY       same as --binary
#   WHATSAPP_MCP_DOWNLOAD=1   same as --download
set -eu

RELEASE_URL=${WHATSAPP_MCP_RELEASE_URL:-https://github.com/gganeles/whatsapp-mcp/releases/latest/download}
PREBUILT=${WHATSAPP_MCP_BINARY:-}
DOWNLOAD=${WHATSAPP_MCP_DOWNLOAD:-}
[ "$DOWNLOAD" = 1 ] || DOWNLOAD=""
while [ $# -gt 0 ]; do
	case $1 in
	--binary) [ $# -ge 2 ] || { echo "--binary needs a path" >&2; exit 2; }; PREBUILT=$2; shift 2 ;;
	--binary=*) PREBUILT=${1#--binary=}; shift ;;
	--download) DOWNLOAD=1; shift ;;
	-h | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
	*) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
	esac
done

SVC_USER=whatsapp-mcp
BIN=/usr/local/bin/whatsapp-mcp
LIBEXEC=/usr/local/libexec/whatsapp-mcp
GO_ROOT=/usr/local/lib/whatsapp-mcp/go
CONFIG_DIR=/etc/whatsapp-mcp
CONFIG=$CONFIG_DIR/env
TUNNEL_CONFIG=$CONFIG_DIR/tunnel.env
DATA_DIR=/var/lib/whatsapp-mcp
ADMIN=/usr/local/sbin/whatsapp-mcp-admin

# sudo on Oracle Linux, RHEL and others resets PATH to a secure_path without /usr/local.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH
export PATH

say() { printf '\n==> %s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "this installer is for Linux."
[ "$(id -u)" -eq 0 ] || die "run it as root, e.g. sudo sh $0"
REPO=$(cd "$(dirname "$0")/.." && pwd)
[ -f "$REPO/go.mod" ] || die "run it from the whatsapp-mcp repository checkout."
[ -z "$PREBUILT" ] || [ -z "$DOWNLOAD" ] || die "use either --binary or --download, not both"
if [ -n "$PREBUILT" ]; then
	[ -f "$PREBUILT" ] || die "no such file: $PREBUILT"
	PREBUILT=$(cd "$(dirname "$PREBUILT")" && pwd)/$(basename "$PREBUILT")
fi

# Prompts read from the terminal, so they work even when stdin is a pipe.
INTERACTIVE=1
if [ "${WHATSAPP_MCP_NONINTERACTIVE:-}" = 1 ] || ! { : </dev/tty; } 2>/dev/null; then
	INTERACTIVE=0
fi

# ---------------------------------------------------------------- distro and arch

case "$(uname -m)" in
x86_64 | amd64) GOARCH=amd64 CF_ARCH=amd64 ;;
aarch64 | arm64) GOARCH=arm64 CF_ARCH=arm64 ;;
armv7* | armv6* | armhf) GOARCH=armv6l CF_ARCH=arm ;;
i386 | i686) GOARCH=386 CF_ARCH=386 ;;
ppc64le) GOARCH=ppc64le CF_ARCH="" ;;
s390x) GOARCH=s390x CF_ARCH="" ;;
riscv64) GOARCH=riscv64 CF_ARCH="" ;;
*) die "unsupported CPU architecture: $(uname -m)" ;;
esac

if command -v apt-get >/dev/null 2>&1; then PM=apt
elif command -v dnf >/dev/null 2>&1; then PM=dnf
elif command -v yum >/dev/null 2>&1; then PM=yum
elif command -v zypper >/dev/null 2>&1; then PM=zypper
elif command -v pacman >/dev/null 2>&1; then PM=pacman
elif command -v apk >/dev/null 2>&1; then PM=apk
else PM=none
fi

if [ -d /run/systemd/system ]; then INIT=systemd
elif command -v openrc-run >/dev/null 2>&1 || [ -x /sbin/openrc-run ]; then INIT=openrc
else INIT=none
fi

pkg_install() {
	case $PM in
	apt) DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" ;;
	dnf) dnf install -y -q "$@" ;;
	yum) yum install -y -q "$@" ;;
	zypper) zypper --non-interactive --quiet install "$@" ;;
	pacman) pacman -Sy --needed --noconfirm "$@" ;;
	apk) apk add --no-cache -q "$@" ;;
	none) die "install these packages and run me again: $*" ;;
	esac
}

say "Installing packages ($PM, $INIT, $GOARCH)"
need=""
command -v curl >/dev/null 2>&1 || need="$need curl"
if [ -z "$PREBUILT" ] && [ -z "$DOWNLOAD" ]; then
	command -v tar >/dev/null 2>&1 || need="$need tar"
	command -v gzip >/dev/null 2>&1 || need="$need gzip"
fi
[ -e /etc/ssl/certs/ca-certificates.crt ] || [ -e /etc/pki/tls/certs/ca-bundle.crt ] ||
	[ -e /etc/ssl/ca-bundle.pem ] || need="$need ca-certificates"
if [ "$PM" = apk ]; then
	# Alpine's busybox lacks runuser; su-exec drops privileges for whatsapp-mcp-admin.
	command -v su-exec >/dev/null 2>&1 || need="$need su-exec"
fi
if [ -n "$need" ]; then
	# shellcheck disable=SC2086
	pkg_install $need
else
	echo "nothing to install"
fi

# ---------------------------------------------------------------- build (or prebuilt binary)

if [ -n "$DOWNLOAD" ]; then
	asset=whatsapp-mcp-linux-$(echo "$GOARCH" | sed 's/armv6l/arm/')
	say "Downloading $asset from $RELEASE_URL"
	PREBUILT=$BIN.download
	trap 'rm -f "$PREBUILT"' EXIT
	curl -fSL --progress-bar -o "$PREBUILT" "$RELEASE_URL/$asset" ||
		die "couldn't download $asset; there may be no build for this CPU yet. Run without --download to build here."
	sums=$(curl -fsSL "$RELEASE_URL/SHA256SUMS") || die "couldn't download SHA256SUMS"
	want=$(printf '%s\n' "$sums" | awk -v f="$asset" '$2 == f { print $1 }')
	have=$(sha256sum "$PREBUILT" | awk '{ print $1 }')
	[ -n "$want" ] && [ "$want" = "$have" ] || die "the downloaded $asset doesn't match its SHA256SUMS entry"
	echo "checksum ok"
fi

if [ -n "$PREBUILT" ]; then
	say "Installing the prebuilt binary $PREBUILT"
	# Copy it next to its final place and run it there, which catches a binary built for
	# the wrong CPU before it replaces anything (the source may be on a noexec mount).
	install -m 755 "$PREBUILT" "$BIN.new"
	if ! "$BIN.new" help >/dev/null 2>&1; then
		rm -f "$BIN.new"
		die "$PREBUILT doesn't run here; build it with GOOS=linux GOARCH=$(echo "$GOARCH" | sed 's/armv6l/arm/') (see the README)"
	fi
	mv -f "$BIN.new" "$BIN"
else
	# Go toolchain: the system's if new enough, otherwise our own copy.
	# version_ge A B: true if version A >= B (both like 1.26.0).
	version_ge() {
		[ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
	}

	GO_NEED=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' "$REPO/go.mod" | head -n1)
	[ -n "$GO_NEED" ] || die "can't read the Go version from go.mod"
	GO=""
	for candidate in "$GO_ROOT/bin/go" "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go; do
		[ -n "$candidate" ] && [ -x "$candidate" ] || continue
		have=$("$candidate" env GOVERSION 2>/dev/null | sed 's/^go//')
		if [ -n "$have" ] && version_ge "$have" "$GO_NEED"; then
			GO=$candidate
			break
		fi
	done

	if [ -z "$GO" ]; then
		say "Downloading Go (the server needs Go $GO_NEED or newer)"
		latest=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1)
		case $latest in go1.*) ;; *) die "couldn't look up the latest Go version" ;; esac
		tmp=$(mktemp -d)
		trap 'rm -rf "$tmp"' EXIT
		curl -fSL --progress-bar -o "$tmp/go.tgz" "https://go.dev/dl/$latest.linux-$GOARCH.tar.gz"
		rm -rf "$GO_ROOT"
		mkdir -p "$(dirname "$GO_ROOT")"
		tar -C "$tmp" -xzf "$tmp/go.tgz"
		mv "$tmp/go" "$GO_ROOT"
		GO=$GO_ROOT/bin/go
		echo "installed $latest in $GO_ROOT"
	fi

	say "Building whatsapp-mcp with $("$GO" env GOVERSION) (a few minutes the first time)"
	out=$(mktemp -d)
	# Pure Go (SQLite included), so no C compiler is needed. -buildvcs=false because the
	# checkout may belong to another user, which git refuses to read as root.
	(cd "$REPO" && CGO_ENABLED=0 GOTOOLCHAIN=local "$GO" build -trimpath -buildvcs=false \
		-ldflags='-s -w' -o "$out/whatsapp-mcp" .)
	install -m 755 "$out/whatsapp-mcp" "$BIN"
	rm -rf "$out"
fi
mkdir -p "$LIBEXEC"
install -m 755 "$REPO/linux/whatsapp-mcp-run" "$LIBEXEC/run"
install -m 755 "$REPO/linux/whatsapp-mcp-admin" "$ADMIN"
# Also put it where `sudo whatsapp-mcp-admin` finds it: sudo's secure_path usually lacks
# /usr/local/sbin.
if [ ! -e /usr/sbin/whatsapp-mcp-admin ] || [ -L /usr/sbin/whatsapp-mcp-admin ]; then
	ln -sf "$ADMIN" /usr/sbin/whatsapp-mcp-admin
fi
echo "installed $BIN"

# ---------------------------------------------------------------- service user

say "Setting up the $SVC_USER user and $DATA_DIR"
nologin=/bin/false
for s in /usr/sbin/nologin /sbin/nologin; do [ -x "$s" ] && nologin=$s && break; done
if ! id "$SVC_USER" >/dev/null 2>&1; then
	if command -v useradd >/dev/null 2>&1; then
		useradd --system --user-group --home-dir "$DATA_DIR" --no-create-home \
			--shell "$nologin" --comment "WhatsApp MCP server" "$SVC_USER"
	else
		addgroup -S "$SVC_USER"
		adduser -S -D -H -h "$DATA_DIR" -s "$nologin" -G "$SVC_USER" -g "WhatsApp MCP server" "$SVC_USER"
	fi
fi
mkdir -p "$DATA_DIR"
chown "$SVC_USER:$SVC_USER" "$DATA_DIR"
chmod 700 "$DATA_DIR"

# ---------------------------------------------------------------- settings

# Keep anything already saved, then let the environment override it.
saved_url="" saved_http="" saved_ro="" saved_token=""
if [ -f "$CONFIG" ]; then
	saved_url=$(sed -n "s/^WHATSAPP_MCP_PUBLIC_URL='\(.*\)'$/\1/p" "$CONFIG")
	saved_http=$(sed -n "s/^WHATSAPP_MCP_HTTP='\(.*\)'$/\1/p" "$CONFIG")
	saved_ro=$(sed -n "s/^WHATSAPP_MCP_READ_ONLY='\(.*\)'$/\1/p" "$CONFIG")
fi
[ -f "$TUNNEL_CONFIG" ] && saved_token=$(sed -n "s/^TUNNEL_TOKEN='\(.*\)'$/\1/p" "$TUNNEL_CONFIG")
PUBLIC_URL=${WHATSAPP_MCP_PUBLIC_URL:-$saved_url}
LISTEN=${WHATSAPP_MCP_HTTP:-${saved_http:-127.0.0.1:8080}}
READ_ONLY=${WHATSAPP_MCP_READ_ONLY:-${saved_ro:-false}}
TOKEN=${CLOUDFLARE_TUNNEL_TOKEN-$saved_token}

# ask VAR "prompt" [secret]: sets VAR from the terminal; Enter keeps its current value.
ask() {
	eval "current=\${$1}"
	hint=""
	[ -n "$current" ] && hint=" [Enter keeps the current value]"
	printf '%s%s: ' "$2" "$hint" >/dev/tty
	if [ -n "${3:-}" ]; then
		stty -echo </dev/tty
		IFS= read -r answer </dev/tty || answer=""
		stty echo </dev/tty
		echo >/dev/tty
	else
		IFS= read -r answer </dev/tty || answer=""
	fi
	[ -n "$answer" ] && eval "$1=\$answer"
	return 0
}

if [ "$INTERACTIVE" = 1 ]; then
	say "Settings"
	cat <<'TXT'
The server needs a public HTTPS address for Claude to reach it. The easiest way is a
Cloudflare Tunnel (Zero Trust > Networks > Tunnels > Create a tunnel, type Cloudflared):
copy the token from its install command and add a public hostname that points at
http://localhost:8080. You can also leave the token empty and put your own HTTPS
reverse proxy (Caddy, nginx, Tailscale Funnel) in front of the listen address.
TXT
	ask PUBLIC_URL "Public URL (e.g. https://whatsapp.example.com)"
	if [ -n "$TOKEN" ]; then
		ask TOKEN "Cloudflare tunnel token (type - to stop using the tunnel)" secret
		[ "$TOKEN" = - ] && TOKEN=""
	else
		ask TOKEN "Cloudflare tunnel token (Enter to skip the tunnel)" secret
	fi
fi

PUBLIC_URL=${PUBLIC_URL%/}
case $PUBLIC_URL in
https://*) ;;
"") die "a public URL is required; set WHATSAPP_MCP_PUBLIC_URL or run interactively." ;;
*) die "the public URL must start with https://" ;;
esac
for value in "$PUBLIC_URL" "$LISTEN" "$READ_ONLY" "$TOKEN"; do
	case $value in *"'"* | *'\'*) die "settings can't contain quotes or backslashes" ;; esac
done

umask 077
mkdir -p "$CONFIG_DIR"
cat >"$CONFIG" <<CONF
# Settings for the whatsapp-mcp service. Restart the service after editing.
WHATSAPP_MCP_PUBLIC_URL='$PUBLIC_URL'
WHATSAPP_MCP_HTTP='$LISTEN'
WHATSAPP_MCP_READ_ONLY='$READ_ONLY'
CONF
if [ -n "$TOKEN" ]; then
	cat >"$TUNNEL_CONFIG" <<CONF
# Cloudflare tunnel token for the whatsapp-mcp-tunnel service.
TUNNEL_TOKEN='$TOKEN'
CONF
else
	rm -f "$TUNNEL_CONFIG"
fi
# The service user reads its settings; nobody else can.
chown -R "root:$SVC_USER" "$CONFIG_DIR"
chmod 750 "$CONFIG_DIR"
chmod 640 "$CONFIG"
[ -f "$TUNNEL_CONFIG" ] && chmod 640 "$TUNNEL_CONFIG"
umask 022

# ---------------------------------------------------------------- cloudflared

if [ -n "$TOKEN" ] && [ ! -x /usr/local/bin/cloudflared ]; then
	if existing=$(command -v cloudflared 2>/dev/null); then
		ln -sf "$existing" /usr/local/bin/cloudflared
	else
		[ -n "$CF_ARCH" ] || die "Cloudflare doesn't publish cloudflared for $(uname -m); use your own reverse proxy instead."
		say "Downloading cloudflared"
		curl -fSL --progress-bar -o /usr/local/bin/cloudflared.new \
			"https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$CF_ARCH"
		chmod 755 /usr/local/bin/cloudflared.new
		mv /usr/local/bin/cloudflared.new /usr/local/bin/cloudflared
	fi
fi

# SELinux (Oracle Linux, RHEL, Fedora): give the new files their default labels.
if command -v restorecon >/dev/null 2>&1; then
	restorecon -R "$BIN" "$LIBEXEC" "$ADMIN" "$CONFIG_DIR" "$DATA_DIR" \
		/usr/local/bin/cloudflared 2>/dev/null || true
fi

# ---------------------------------------------------------------- accounts

if [ "$INTERACTIVE" = 1 ]; then
	say "Accounts"
	"$ADMIN" user list || true
	# Accounts, passwords and sign-ins live in $DATA_DIR/auth.db, which reinstalling never
	# touches, so an update needs no account step at all.
	if "$ADMIN" user list 2>/dev/null | grep -q '^+'; then
		echo "These accounts, their passwords and their signed-in clients are kept as they are."
		printf 'Add another account? Enter its WhatsApp number, or press Enter to skip: ' >/dev/tty
	else
		printf 'Add an account now? Enter its WhatsApp number (e.g. +14155551234), or press Enter to skip: ' >/dev/tty
	fi
	IFS= read -r phone </dev/tty || phone=""
	if [ -n "$phone" ]; then
		# A failure here (say, a number that already has an account) mustn't stop the
		# update half way, before the new binary is running.
		"$ADMIN" user add "$phone" </dev/tty || echo "No account added; carrying on." >&2
	fi
fi

# ---------------------------------------------------------------- service

say "Starting the service ($INIT)"
case $INIT in
systemd)
	install -m 644 "$REPO/linux/whatsapp-mcp.service" /etc/systemd/system/whatsapp-mcp.service
	install -m 644 "$REPO/linux/whatsapp-mcp-tunnel.service" /etc/systemd/system/whatsapp-mcp-tunnel.service
	command -v restorecon >/dev/null 2>&1 && restorecon /etc/systemd/system/whatsapp-mcp*.service 2>/dev/null || true
	systemctl daemon-reload
	systemctl enable whatsapp-mcp.service >/dev/null 2>&1
	systemctl restart whatsapp-mcp.service
	if [ -n "$TOKEN" ]; then
		systemctl enable whatsapp-mcp-tunnel.service >/dev/null 2>&1
		systemctl restart whatsapp-mcp-tunnel.service
	else
		systemctl disable --now whatsapp-mcp-tunnel.service >/dev/null 2>&1 || true
	fi
	MANAGE="systemctl status|restart|stop whatsapp-mcp whatsapp-mcp-tunnel
  Logs:     journalctl -u whatsapp-mcp -u whatsapp-mcp-tunnel -f"
	;;
openrc)
	install -m 755 "$REPO/linux/whatsapp-mcp.openrc" /etc/init.d/whatsapp-mcp
	install -m 755 "$REPO/linux/whatsapp-mcp-tunnel.openrc" /etc/init.d/whatsapp-mcp-tunnel
	rc-update add whatsapp-mcp default >/dev/null
	rc-service whatsapp-mcp restart
	if [ -n "$TOKEN" ]; then
		rc-update add whatsapp-mcp-tunnel default >/dev/null
		rc-service whatsapp-mcp-tunnel restart
	else
		rc-service whatsapp-mcp-tunnel stop >/dev/null 2>&1 || true
		rc-update del whatsapp-mcp-tunnel default >/dev/null 2>&1 || true
	fi
	MANAGE="rc-service whatsapp-mcp status|restart|stop (and whatsapp-mcp-tunnel)
  Logs:     tail -f /var/log/whatsapp-mcp/*.log"
	;;
none)
	MANAGE="No systemd or OpenRC found, so nothing starts automatically. Run the server with:
    runuser -u $SVC_USER -- $LIBEXEC/run server
  and the tunnel with:
    runuser -u $SVC_USER -- $LIBEXEC/run tunnel
  under whatever supervisor you use."
	;;
esac

if [ "$INIT" != none ]; then
	healthy=0
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		if "$ADMIN" healthcheck >/dev/null 2>&1; then healthy=1; break; fi
		sleep 1
	done
	if [ "$healthy" = 1 ]; then
		echo "server is up on $LISTEN"
	else
		echo "The server didn't answer its health check yet; check the logs (below)." >&2
	fi
fi

update_note="git pull, then run this installer again."
if [ -n "$DOWNLOAD" ]; then
	update_note="git pull, then run this installer again with --download."
elif [ -n "$PREBUILT" ]; then
	update_note="git pull, build a new binary, then run this installer again with --binary."
fi

tunnel_note="In the Cloudflare dashboard, point the tunnel's public hostname at http://localhost:${LISTEN##*:}."
[ -n "$TOKEN" ] || tunnel_note="Point your HTTPS reverse proxy for $PUBLIC_URL at http://$LISTEN."

cat <<DONE

Installed. Existing accounts, passwords and connector sign-ins were kept; nobody needs to
sign in again.

  1. $tunnel_note
  2. New users only: in claude.ai, Settings > Connectors > Add custom connector >
     $PUBLIC_URL/mcp, sign in with your number and password, then ask Claude to
     "link my WhatsApp".

  Accounts: sudo whatsapp-mcp-admin user add|passwd|check|remove|list
  Service:  $MANAGE
  Settings: $CONFIG (restart the service after editing)
  Data:     $DATA_DIR

To update: $update_note
DONE
