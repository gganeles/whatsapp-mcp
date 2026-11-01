#!/data/data/com.termux/files/usr/bin/bash
# Installs the WhatsApp MCP server on Termux and sets it to start when the phone boots.
# Run it from the repository checkout:  bash termux/install.sh
set -euo pipefail

if [ -z "${PREFIX:-}" ] || [[ "$PREFIX" != *com.termux* ]]; then
	echo "This script is for Termux on Android." >&2
	exit 1
fi
REPO="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG_DIR="$HOME/.config/whatsapp-mcp"
CONFIG="$CONFIG_DIR/env"

echo "==> Installing packages"
pkg install -y golang cloudflared procps >/dev/null

echo "==> Building whatsapp-mcp (this takes a few minutes the first time)"
(cd "$REPO" && go build -trimpath -o "$PREFIX/bin/whatsapp-mcp" .)
install -m 700 "$REPO/termux/whatsapp-mcp-service" "$PREFIX/bin/whatsapp-mcp-service"

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
echo
echo "==> Settings"
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

echo
echo "==> Accounts"
whatsapp-mcp user list
read -r -p "Add an account now? Enter its WhatsApp number (e.g. +972501234567), or press Enter to skip: " phone
if [ -n "$phone" ]; then
	whatsapp-mcp user add "$phone"
fi

echo
echo "==> Start on boot"
mkdir -p "$HOME/.termux/boot"
install -m 700 "$REPO/termux/boot-whatsapp-mcp" "$HOME/.termux/boot/whatsapp-mcp"

whatsapp-mcp-service restart >/dev/null
sleep 3
whatsapp-mcp-service status

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
