# Docker deployment

## Images and tags

Pull `vaalacat/openvaala` from Docker Hub or `ghcr.io/vaalacat/openvaala` from GHCR. Both registries carry the same Linux image for amd64 and arm64. Use a version tag such as `v0.1.0` to pin a release, `latest` for the most recent release, or `alpha` for builds from `main`.

## Data, ports and first start

Choose one Compose example below and save it as `compose.yaml` in its own directory. Each example sets an explicit startup command and mounts a writable named volume at `/data` for configuration and state. World listens on port 8791 and the full runtime listens on 8790. The examples publish only to the host's loopback address.

Start with an empty volume by running this command in the Compose directory. Reuse the volume on upgrades to keep your settings and data.

```bash
docker compose up -d
```

## Public relay

```yaml
services:
  openvaala:
    image: vaalacat/openvaala
    command: ["world", "serve", "--relay-mode", "public"]
    ports: ["127.0.0.1:8791:8791"]
    volumes: ["data:/data"]
    healthcheck:
      test: ["CMD", "wget", "-qO", "/dev/null", "http://127.0.0.1:8791/world/v1/healthz"]
volumes:
  data:
```

Open `http://127.0.0.1:8791/web/v1/` for the web client. Relay WebSocket connections use the same port.

## Domain control plane

Replace `world.example.com` with the domain you manage.

```yaml
services:
  openvaala:
    image: vaalacat/openvaala
    command: ["world", "serve"]
    environment:
      - OPENVAALA_WORLD_SERVER_ENABLED=true
      - OPENVAALA_WORLD_SERVER_DOMAIN=world.example.com
      - OPENVAALA_WORLD_SERVER_ADMIN_TOKEN
    ports: ["127.0.0.1:8791:8791"]
    volumes: ["data:/data"]
    healthcheck:
      test: ["CMD", "wget", "-qO", "/dev/null", "http://127.0.0.1:8791/world/v1/healthz"]
volumes:
  data:
```

On first start, the control plane creates `/data/config.yaml` to save its generated admin token. To preload domain rules, place your configuration in the volume before startup. Read `world.server.admin_token` with `docker compose exec openvaala cat /data/config.yaml` and use it at `/web/v1/world/admin`. To supply your own token instead, export `OPENVAALA_WORLD_SERVER_ADMIN_TOKEN` on each deployment. The domain database lives at `/data/worldserver/world.db`.

## Full runtime

Place the project you want the agent to work on in `./project`, or change the host path in the bind mount. The local executor starts in `/workspace`.

```yaml
services:
  openvaala:
    image: vaalacat/openvaala
    command: ["serve"]
    working_dir: /workspace
    ports: ["127.0.0.1:8790:8790"]
    volumes: ["data:/data", "./project:/workspace"]
    healthcheck:
      test: ["CMD", "wget", "-qO", "/dev/null", "http://127.0.0.1:8790/api/v1/healthz"]
volumes:
  data:
```

Read the runtime token with `docker compose exec openvaala cat /data/runtime-server.yaml`. Open `http://127.0.0.1:8790/web/v1/#token=<token>` with that token. Keep the volume when recreating the container to retain the token and conversations.

## HTTPS with Caddy

Point your domain at the host and run Caddy on the host. For the relay or control plane, use this Caddyfile:

```text
world.example.com {
    reverse_proxy 127.0.0.1:8791
}
```

For the full runtime, use its domain and port 8790. Caddy handles WebSocket upgrades so clients can connect over `wss://`. When accessed remotely, the browser needs HTTPS for the Web Crypto APIs used by World.

## Extend the image

Install additional tools in a derived image and use it in your Compose file:

```dockerfile
FROM vaalacat/openvaala
RUN apk add --no-cache python3
```

## Multi-instance public relay

Run one public relay container per instance. Give every instance its own DNS name and TLS certificate, and route that name to its container. Put the same list in each container’s `/data/config.yaml` and restart it to load the configuration.

```yaml
world:
  relay_mode: public
  relay_instances:
    - https://relay-a.example.com
    - https://relay-b.example.com
    - https://relay-c.example.com
```

Nodes configure one entry in `world.relays`, such as `https://relay.example.com`. The entry can point to any instance or to a load balancer across them. Both the attaching node or control plane and the dialing client must run a version that supports relay instances. When scaling, add or remove one instance at a time. Keep a retiring instance online until the updated list has been served and attachments have refreshed (normally within 10 minutes).
