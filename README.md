# whatsapp-mcp

An [MCP](https://modelcontextprotocol.io) server that links to your WhatsApp account as a companion
device (the same way WhatsApp Web does), keeps a local SQLite copy of your chats, and lets an MCP
client such as Claude read, search, and send messages. Built on
[whatsmeow](https://github.com/tulir/whatsmeow).

Everything stays on your machine: the session keys and messages live in `~/.whatsapp-mcp`.

## Install

Requires Go 1.26+ and a C compiler (for SQLite via cgo).

```sh
go install github.com/gganeles/whatsapp-mcp@latest
# or, from a checkout:
go build -o whatsapp-mcp .
```

## Link your account

```sh
whatsapp-mcp login --phone +14155551234
```

This prints an 8-character pairing code. On your phone open **WhatsApp > Settings > Linked devices >
Link a device > Link with phone number instead** and type it in. The code is valid for about
2.5 minutes. If you'd rather scan a QR code, run `whatsapp-mcp login --qr`.

After linking, the command stays connected while your phone sends chat history, and exits once
the sync goes quiet. Keep the phone online for this. You can also link from inside an MCP client
with the `pair_device` tool (pairing code by default, `use_qr: true` for a QR image).

## Use it from an MCP client

Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/path/to/whatsapp-mcp",
      "args": ["serve"]
    }
  }
}
```

Claude Code:

```sh
claude mcp add whatsapp -- /path/to/whatsapp-mcp serve
```

Only one process can use a WhatsApp session at a time. If you want several clients to share it,
run a single server over HTTP and point them all at it:

```sh
whatsapp-mcp serve --http 127.0.0.1:8765     # MCP endpoint: http://127.0.0.1:8765/mcp
claude mcp add --transport http whatsapp http://127.0.0.1:8765/mcp
```

The HTTP endpoint has no authentication, so keep it bound to localhost.

## Tools

| Tool | What it does |
| --- | --- |
| `get_status` | Linked/connected state, sync progress, pending pairing code |
| `pair_device` | Link the account (pairing code, or QR with `use_qr`) |
| `list_chats` | Chats by recent activity, with last message; filter by name, group/direct |
| `get_chat` | Chat details; group topic and participants |
| `list_messages` | Messages in a chat, with time range and sender filters |
| `search_messages` | Full-text search across all chats or one chat |
| `get_message_context` | Messages around a given message (e.g. a search hit) |
| `search_contacts` | Find a contact or group by name or number |
| `send_message` | Send text, optionally as a quote-reply |
| `send_file` | Send an image, video, audio, voice note, or document from a local path |
| `send_reaction` | React to a message with an emoji |
| `download_media` | Save a message's attachment to disk (images are also returned inline) |
| `request_history` | Ask the phone for older messages in a chat |

Wherever a chat is expected you can pass a JID (`14155551234@s.whatsapp.net`, `123...@g.us`), a phone
number, or a chat name that matches exactly one chat.

## Configuration

| Setting | Default |
| --- | --- |
| `--data DIR` / `WHATSAPP_MCP_DATA` | `~/.whatsapp-mcp` |
| `--phone` / `WHATSAPP_PHONE` | Number used for pairing codes |
| `--log-level` | `INFO` (logs go to stderr) |

Files in the data directory: `whatsapp.db` (session keys, contacts; treat it like a password),
`messages.db` (your messages), and `media/` (downloaded attachments).

## Unlink

```sh
whatsapp-mcp logout
```

This removes the linked device from your account. Delete the data directory to remove stored messages.

## Notes

- This uses WhatsApp's multi-device web protocol through an unofficial library. Automated or bulk
  messaging can get an account banned, so use sending tools for normal, personal-scale messaging.
- WhatsApp decides how much history a newly linked device receives. `request_history` can pull more
  for a specific chat while the phone is online.
- Media from old messages may no longer be downloadable once WhatsApp's servers expire it.
