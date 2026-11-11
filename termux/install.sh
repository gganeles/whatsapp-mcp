#!/data/data/com.termux/files/usr/bin/bash
# Installs the WhatsApp MCP server on Termux and sets it to start when the phone boots.
# Run it from the repository checkout:  bash termux/install.sh
set -euo pipefail

START=$SECONDS
step() { printf '\n\033[1;32m==> [%dm%02ds] %s\033[0m\n' $(((SECONDS - START) / 60)) $(((SECONDS - START) % 60)) "$*"; }
info() { printf '    %s\n' "$*"; }
# Show which command failed instead of exiting silently.
trap 'printf "\n\033[1;31mFailed at line %s: %s\033[0m\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

if [ -z "${PREFIX:-}" ] || [[ "$PREFIX" != *com.termux* ]]; then
	echo "This script is for Termux on Android." >&2
	exit 1
fi
REPO="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_DIR="$HOME/.config/whatsapp-mcp"
CONFIG="$CONFIG_DIR/env"

step "Installing packages: golang, cloudflared, procps"
pkg install -y golang cloudflared procps
info "$(go version)"
info "cloudflared $(cloudflared --version 2>&1 | head -n1)"

step "Downloading Go modules (each one is listed as it downloads)"
(cd "$REPO" && go mod download -x 2>&1 | grep --line-buffered -E '^# get .*200 OK|^go: ' | sed -u 's/^# get /    got /' || true)

step "Compiling whatsapp-mcp; each package is listed as it builds, SQLite is the slow one (several minutes)"
(cd "$REPO" && go build -v -trimpath -o "$PREFIX/bin/whatsapp-mcp" . 2>&1 | sed -u 's/^/    /')
info "Built $PREFIX/bin/whatsapp-mcp ($(du -h "$PREFIX/bin/whatsapp-mcp" | cut -f1))"
install -m 700 "$REPO/termux/whatsapp-mcp-service" "$PREFIX/bin/whatsapp-mcp-service"
info "Installed $PREFIX/bin/whatsapp-mcp-service"

# Ask for settings, keeping any already saved.
mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
WHATSAPP_MCP_PUBLIC_URL="" CLOUDFLARE_TUNNEL_TOKEN="" WHATSAPP_MCP_READ_ONLY="false"
# shellcheck source=/dev/null
[ -f "$CONFIG" ] && . "$CONFIG"

ask() { # ask VAR "prompt" [secret]
	local current="${!1}" answer
	if [ -n "$current" ]; then
		read -r -p "$2 [press Enter to keep the current value]: " ${3:+-s} answer
	else
		read -r -p "$2: " ${3:+-s} answer
	fi
	[ -n "${3:-}" ] && echo
	[ -n "$answer" ] && printf -v "$1" '%s' "$answer"
	return 0
}
step "Settings (saved to $CONFIG)"
ask WHATSAPP_MCP_PUBLIC_URL "Public URL from your Cloudflare tunnel (e.g. https://whatsapp.example.com)"
ask CLOUDFLARE_TUNNEL_TOKEN "Cloudflare tunnel token" secret
WHATSAPP_MCP_PUBLIC_URL="${WHATSAPP_MCP_PUBLIC_URL%/}"
case "$WHATSAPP_MCP_PUBLIC_URL" in
https://*) ;;
*) echo "The public URL must start with https://" >&2; exit 1 ;;
esac

umask 077
cat >"$CONFIG" <<CONF
# Settings for whatsapp-mcp-service. Run "whatsapp-mcp-service restart" after editing.
WHATSAPP_MCP_PUBLIC_URL='$WHATSAPP_MCP_PUBLIC_URL'
CLOUDFLARE_TUNNEL_TOKEN='$CLOUDFLARE_TUNNEL_TOKEN'
WHATSAPP_MCP_HTTP='127.0.0.1:8080'
WHATSAPP_MCP_READ_ONLY='$WHATSAPP_MCP_READ_ONLY'
CONF
chmod 600 "$CONFIG"
info "Saved settings to $CONFIG (readable only by you)"

step "Accounts"
whatsapp-mcp user list
if whatsapp-mcp user list 2>/dev/null | grep -q '^+'; then
	echo "These accounts, their passwords and their signed-in clients are kept as they are."
	read -r -p "Add another account? Enter its WhatsApp number, or press Enter to skip: " phone
else
	read -r -p "Add an account now? Enter its WhatsApp number (e.g. +972501234567), or press Enter to skip: " phone
fi
if [ -n "$phone" ]; then
	whatsapp-mcp user add "$phone" || echo "No account added; carrying on." >&2
fi

step "Start on boot"
mkdir -p "$HOME/.termux/boot"
install -m 700 "$REPO/termux/boot-whatsapp-mcp" "$HOME/.termux/boot/whatsapp-mcp"
info "Installed boot script $HOME/.termux/boot/whatsapp-mcp"

step "Starting the server and tunnel"
whatsapp-mcp-service restart
info "Waiting 5s for them to come up..."
sleep 5
whatsapp-mcp-service status
info "Recent server log:"
tail -n 5 "$HOME/.local/state/whatsapp-mcp/server.log" 2>/dev/null | sed 's/^/      /' || true
step "Done in $(((SECONDS - START) / 60))m$(((SECONDS - START) % 60))s"

cat <<DONE

Installed. A few things to do on the phone, once:

  1. Install the Termux:Boot app from the same store you got Termux from (F-Droid or
     GitHub), and open it once so Android lets it run at boot.
  2. Android Settings > Apps > Termux (and Termux:Boot) > Battery > Unrestricted,
     otherwise Android may stop the server when the screen is off.
  3. In the Cloudflare dashboard, point the tunnel's public hostname at
     http://localhost:8080.
  4. In claude.ai: Settings > Connectors > Add custom connector >
     $WHATSAPP_MCP_PUBLIC_URL/mcp, sign in, then ask Claude to "link my WhatsApp".

Manage it with: whatsapp-mcp-service start|stop|restart|status|logs
DONE
