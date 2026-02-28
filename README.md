# ffmpeg-remote

Transparent drop-in replacement for the `ffmpeg` binary that offloads transcoding to a remote server over gRPC. Everything streams in real-time — input is piped to the server as it's read, output is written back as ffmpeg produces it. Nothing touches disk on the server.

## Quick Start

### 1. Start the server

Create a `.env` file:

```env
TOKEN=change-me
MAX_JOBS=4
```

`docker-compose.yml`:

```yaml
services:
  ffmpeg-remote:
    image: ghcr.io/palmarhealer/ffmpeg-go:latest
    restart: unless-stopped
    ports:
      - "50051:50051"
    env_file:
      - .env
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu, video]
```

```bash
docker compose up -d
```

> **Prerequisite:** [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html) must be installed on the host.

---

### 2. Install the client

```bash
# Back up your existing ffmpeg
sudo mv /usr/local/bin/ffmpeg /usr/local/bin/ffmpeg.real

# Download the latest client and install it as ffmpeg
sudo curl -fsSL https://github.com/PalmarHealer/ffmpeg-go/releases/latest/download/ffmpeg-remote-client-linux-amd64 \
  -o /usr/local/bin/ffmpeg && sudo chmod +x /usr/local/bin/ffmpeg
```

### 3. Configure the client

Create `~/.config/ffmpeg-remote/config.json`:

```json
{
  "server_url": "your-server:50051",
  "token": "change-me",
  "fallback_bin": "ffmpeg.real",
  "fallback": "always"
}
```

### 4. Use it

```bash
# Exactly the same as before — transparently runs on the remote GPU
ffmpeg -i input.mkv -vf scale=1280:720 -f mp4 output.mp4
```

If the server is unreachable, the client falls back to `ffmpeg.real` automatically.

---

## Config

The client loads and merges config files in this order (highest priority first):

| Priority | Path |
|----------|------|
| 1 | `./ffmpeg-remote.json` |
| 2 | `$FFMPEG_REMOTE_CONFIG` |
| 3 | `~/.config/ffmpeg-remote/config.json` |
| 4 | `/etc/ffmpeg-remote/config.json` |

| Key | Default | Description |
|-----|---------|-------------|
| `server_url` | — | `host:port` of the gRPC server |
| `token` | — | Auth token (must match server `TOKEN`) |
| `fallback_bin` | `ffmpeg.real` | Local binary to run if remote fails |
| `fallback` | `always` | `always` fall back on error, or `never` |

Server is configured via environment variables (or a `.env` file in the working directory):

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `50051` | Listening port |
| `TOKEN` | — | Required auth token |
| `MAX_JOBS` | `0` | Max concurrent jobs (`0` = unlimited) |
