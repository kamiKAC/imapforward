<div align="center">

# <img src="https://sinedied.github.io/imapforward/logo.svg" alt="imapforward logo" height="32px"> imapforward

[![Build Status](https://img.shields.io/github/actions/workflow/status/sinedied/imapforward/ci.yml?style=flat-square)](https://github.com/sinedied/imapforward/actions)
[![Docker](https://img.shields.io/badge/ghcr.io-imapforward-blue?style=flat-square)](https://github.com/sinedied/imapforward/pkgs/container/imapforward)
[![License](https://img.shields.io/github/license/sinedied/imapforward?style=flat-square)](LICENSE)

Simple real-time IMAP email forwarder for syncing multiple email accounts into one.<br>
Built to replace deprecated [gmailify](https://support.google.com/mail/answer/7644837) tool for Gmail.

[Features](#features) · [Installation](#installation) · [Configuration](#configuration) · [Docker](#docker)

</div>

## Features

- **Real-time sync** — Uses IMAP IDLE for instant email forwarding, with polling fallback for servers that don't support it
- **Three forwarding methods** — IMAP APPEND (preserves all headers), SMTP (enables spam filtering), or Gmail API (preserves headers + spam filtering, more complex setup)
- **Multiple sources** — Forward from multiple email accounts to a single Gmail
- **Original headers preserved** — Reply and Reply-All work with original senders and recipients
- **Selective folders** — Choose which folders to sync per source, with concurrent monitoring
- **Auto cleanup** — Optionally delete messages after successful forwarding
- **Production-grade** — Auto reconnect with exponential backoff, health check endpoint
- **Minimal footprint** — Static Go binary, no runtime dependencies
- **Docker ready** — Scratch-capable Alpine image with built-in health checks

## Installation

### Binary

Download the binary for your platform from [GitHub Releases](https://github.com/sinedied/imapforward/releases) (Linux, macOS, Windows), or build from source:

```bash
go install github.com/sinedied/imapforward@latest
```

### Docker

```bash
docker pull ghcr.io/sinedied/imapforward:latest
```

## Configuration

Create a `config.json` file. You can use the [online configuration generator](https://sinedied.github.io/imapforward/#config) to build it interactively.

```json
{
  "target": {
    "host": "imap.gmail.com",
    "port": 993,
    "secure": true,
    "auth": {
      "user": "your-email@gmail.com",
      "pass": "your-app-password"
    }
  },
  "sources": [
    {
      "name": "Work Email",
      "host": "imap.work.com",
      "port": 993,
      "secure": true,
      "auth": {
        "user": "user@work.com",
        "pass": "password"
      },
      "folders": ["INBOX"],
      "deleteAfterForward": false
    }
  ]
}
```

### Configuration Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `target.host` | string | imap/smtp | — | Target server hostname (IMAP or SMTP) |
| `target.port` | number | imap/smtp | — | Target server port |
| `target.secure` | boolean | no | port-based | Use TLS (defaults to `true` for ports 465/993) |
| `target.auth.user` | string | yes | — | Target email address |
| `target.auth.pass` | string | imap/smtp | — | Password or app password |
| `target.folder` | string | no | `"INBOX"` | Default target mailbox folder (IMAP method only). Auto-created if it doesn't exist |
| `forwardMethod` | string | no | `"imap"` | Forwarding method: `"imap"`, `"smtp"`, or `"gmail-api"` |
| `gmailApi.clientId` | string | gmail-api | — | Google OAuth2 client ID |
| `gmailApi.clientSecret` | string | gmail-api | — | Google OAuth2 client secret |
| `gmailApi.refreshToken` | string | gmail-api | — | Google OAuth2 refresh token |
| `sources[].name` | string | yes | — | Display name for the source |
| `sources[].host` | string | yes | — | IMAP server hostname |
| `sources[].port` | number | yes | — | IMAP server port |
| `sources[].secure` | boolean | no | port-based | Use TLS (defaults to `true` for ports 465/993) |
| `sources[].auth.user` | string | yes | — | IMAP username |
| `sources[].auth.pass` | string | yes | — | IMAP password |
| `sources[].folders` | string[] | no | `["INBOX"]` | Folders to monitor |
| `sources[].deleteAfterForward` | boolean | no | `false` | Delete messages after forwarding |
| `sources[].targetFolder` | string | no | — | Target mailbox for this source. Falls back to `target.folder`. Auto-created if it doesn't exist |
| `sources[].localTracking` | boolean | no | `false` | Remember forwarded messages in a local state file instead of the `$Forwarded` IMAP flag. Cannot be combined with `deleteAfterForward`. See [Tracking forwarded messages](#tracking-forwarded-messages) |
| `sources[].trackingId` | string | with `localTracking` | — | Stable, unique identifier of this source in the state file. Ignored unless `localTracking` is `true` |
| `statePath` | string | no | `state.db` next to the config file | State file location (used only when some source has `localTracking`). Relative paths are resolved against the config file's directory |
| `healthCheck.port` | number | no | `8080` | HTTP health check server port |

> [!TIP]
> For Gmail, you need to have 2FA enabled and use an [App Password](https://support.google.com/accounts/answer/185833) instead of your regular password.

### Forwarding Methods

imapforward supports three forwarding methods:

#### IMAP APPEND (default)

```json
{ "forwardMethod": "imap" }
```

Appends the raw RFC822 message directly to the target mailbox via IMAP. This preserves **all** original headers exactly — From, To, CC, Reply-To, Date, Message-ID, etc. Replies and Reply-All preserve the original sender and recipients. However, since messages bypass Gmail's intake pipeline, **spam filtering is not applied**.

#### SMTP Forward

```json
{
  "target": {
    "host": "smtp.gmail.com",
    "port": 587,
    "auth": { "user": "you@gmail.com", "pass": "your-app-password" }
  },
  "forwardMethod": "smtp"
}
```

Forwards messages via SMTP. Gmail's spam filters process the message normally. A `Reply-To` header is automatically added (if not present) pointing to the original `From` address, so Reply and Reply-All work with the original sender and recipients. Gmail may rewrite the `From` header to match the authenticated sender.

#### Gmail API

The best of both worlds: preserves **all original headers** (From, To, CC, etc.) **and** runs Gmail's spam/phishing filters. Uses the [Gmail API `messages.import`](https://developers.google.com/gmail/api/reference/rest/v1/users.messages/import) endpoint. Free within [Google's default quota limits](https://developers.google.com/gmail/api/reference/quota) (more than enough for email forwarding). Requires a one-time OAuth2 setup, which is more involved than the other methods.

```json
{
  "target": {
    "auth": { "user": "you@gmail.com" }
  },
  "forwardMethod": "gmail-api",
  "gmailApi": {
    "clientId": "your-client-id.apps.googleusercontent.com",
    "clientSecret": "your-client-secret",
    "refreshToken": "your-refresh-token"
  }
}
```

<details>
<summary><strong>Gmail API Setup Guide</strong></summary>

##### 1. Create a Google Cloud Project

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Create a new project (or select an existing one)
3. Enable the **Gmail API**: APIs & Services → Library → search "Gmail API" → Enable

##### 2. Create OAuth2 Credentials

1. Go to APIs & Services → Credentials
2. Click **Create Credentials** → **OAuth client ID**
3. If prompted, configure the **OAuth consent screen**:
   - User type: **External** (or Internal for Workspace)
   - App name: `imapforward`
   - Scopes: add `https://www.googleapis.com/auth/gmail.insert` and `https://www.googleapis.com/auth/gmail.labels`
   - Test users: add your Gmail address
4. Application type: **Desktop app**
5. Note the **Client ID** and **Client Secret**

##### 3. Obtain a Refresh Token

Run the built-in authorization helper:

```bash
imapforward -auth \
  -auth-client-id "YOUR_CLIENT_ID" \
  -auth-client-secret "YOUR_CLIENT_SECRET"
```

This opens your browser for Google consent. After authorizing, the tool prints the `gmailApi` config block to paste into your `config.json`.

##### 4. Configure

Add the output to your `config.json`. No `target.host`, `target.port`, or `target.auth.pass` are needed — only `target.auth.user` (your Gmail address).

</details>

### Tracking forwarded messages

By default, imapforward marks every forwarded message on the source server with the `$Forwarded` IMAP flag and skips flagged messages afterwards. Some servers accept this flag but forget it when the connection closes (for example `poczta.onet.pl`). On such a server every restart forwards all unread messages again.

For those sources, enable local tracking:

```json
{
  "statePath": "/data/state.db",
  "sources": [
    {
      "name": "Onet",
      "host": "imap.poczta.onet.pl",
      "port": 993,
      "auth": { "user": "user@poczta.onet.pl", "pass": "password" },
      "localTracking": true,
      "trackingId": "onet"
    }
  ]
}
```

- The UID of each forwarded message is stored in a small embedded database (`statePath`) right after it is sent. The source mailbox is not modified.
- `trackingId` is required and must be unique across sources with `localTracking`. It identifies the source in the state file, so do not change it after the first run.
- `localTracking` cannot be combined with `deleteAfterForward`.
- On the first run the state file is empty, so **all unread messages in the monitored folders are forwarded**. Later runs forward only new ones. Stopping and restarting is safe at any point.
- Messages you read in your mailbox before they are forwarded are skipped, as with the flag method.
- If the server changes a folder's `UIDVALIDITY`, the remembered UIDs for that folder are discarded (a warning is logged) and unread messages are forwarded again.
- If the process stops after a message is sent but before it is recorded, that single message may be forwarded again.
- Switching an existing source to `localTracking` (or changing its `trackingId`) starts from an empty state.

With Docker, keep the state file on a volume (see [Docker](#docker)).

## Usage

### CLI

```bash
# Run with default config.json in current directory
imapforward

# Set log level
imapforward -log-level debug

# Custom config path
imapforward -config /path/to/config.json
```

### CLI Options

| Option | Description |
|--------|-------------|
| `-config <path>` | Config file path (default: `config.json`) |
| `-log-level <level>` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `-auth` | Run OAuth2 flow to obtain a Gmail API refresh token |
| `-auth-client-id <id>` | Google OAuth2 client ID (required with `-auth`) |
| `-auth-client-secret <secret>` | Google OAuth2 client secret (required with `-auth`) |
| `-version` | Show version |
| `-help` | Show help |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `IMAPFORWARD_CONFIG` | Override config file path |
| `LOG_LEVEL` | Override log level |

## Docker

### Run with Docker

```bash
docker run -d \
  --name imapforward \
  -v $(pwd)/config.json:/app/config.json:ro \
  -v imapforward-data:/data \
  ghcr.io/sinedied/imapforward:latest
```

The `/data` volume is only needed for sources with `localTracking`; set `"statePath": "/data/state.db"` in your config. The container runs as uid 1001, so a bind mount instead of a named volume must be writable by that user.

### Docker Compose

```yaml
services:
  imapforward:
    image: ghcr.io/sinedied/imapforward:latest
    restart: unless-stopped
    volumes:
      - ./config.json:/app/config.json:ro
      - imapforward-data:/data

volumes:
  imapforward-data:
```

### Health Check

The health check server is always enabled (default port `8080`). You can customize the port in your config:

```json
{
  "healthCheck": {
    "port": 9090
  }
}
```

The HTTP health endpoint is available at:

```bash
curl http://localhost:8080/health
```

Response:

```json
{
  "status": "ok",
  "sources": [
    {
      "name": "Work Email",
      "connected": true,
      "lastSync": "2026-02-25T10:30:00.000Z",
      "error": null
    }
  ]
}
```

Status values: `ok` (all connected), `degraded` (some connected), `error` (none connected).

## How It Works

1. Connects to each configured IMAP source account (one connection per folder)
2. Scans for unseen messages that haven't been forwarded yet (`NOT SEEN`, minus those already flagged or recorded locally)
3. Forwards each message to the target using the configured method:
   - **IMAP**: Appends raw RFC822 to the target mailbox, preserving all original headers
   - **SMTP**: Sends via SMTP with Reply-To injection for reply/reply-all support
   - **Gmail API**: Imports via Gmail API with spam filtering and full header preservation
4. Remembers forwarded messages: by default with a `$Forwarded` IMAP flag on the source, or in a local state file for sources with `localTracking`
5. Enters IMAP IDLE mode to watch for new messages in real-time, or falls back to polling when the server doesn't support IDLE
6. Automatically reconnects with exponential backoff on connection loss
