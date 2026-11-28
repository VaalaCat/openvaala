# Docker 部署

## 镜像与标签

从 Docker Hub 拉取 `vaalacat/openvaala`，或从 GHCR 拉取 `ghcr.io/vaalacat/openvaala`。两个仓库提供相同的 Linux 镜像，支持 amd64 和 arm64。要固定版本，用形如 `v0.1.0` 的发行标签。`latest` 跟随最近一次发版，`alpha` 跟随 `main`。

## 数据、端口与首次启动

从下面选一种部署方式，在独立目录中将示例保存为 `compose.yaml`。每个示例都写明启动命令，并将可写命名卷挂载到 `/data`，用于保存配置和数据。World 监听 8791，完整运行时监听 8790。示例将端口绑定到宿主机的回环地址。

在 Compose 目录执行下面的命令即可用空卷启动。升级时复用原卷，即可保留配置和数据。

```bash
docker compose up -d
```

## 公开 relay

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

打开 `http://127.0.0.1:8791/web/v1/` 即可使用 Web 客户端。relay 的 WebSocket 连接也走这个端口。

## 域控制面

将 `world.example.com` 换成你管理的域名。

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

控制面首次启动会创建 `/data/config.yaml` 保存自动生成的管理 Token，要预置域规则可以先把配置文件放进卷。用 `docker compose exec openvaala cat /data/config.yaml` 查看 `world.server.admin_token`，再到 `/web/v1/world/admin` 使用。要指定自己的 Token，每次部署时导出 `OPENVAALA_WORLD_SERVER_ADMIN_TOKEN`。域数据库保存在 `/data/worldserver/world.db`。

## 完整运行时

将 Agent 要操作的项目放在 `./project`，也可以改成已有项目的宿主机路径。本机 Executor 从容器里的 `/workspace` 开始执行命令。

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

执行 `docker compose exec openvaala cat /data/runtime-server.yaml` 查看运行时 Token，代入 `http://127.0.0.1:8790/web/v1/#token=<token>` 后打开。重建容器时复用原卷即可保留 Token 和对话。

## 用 Caddy 提供 HTTPS

将域名解析到宿主机，在宿主机运行 Caddy。公开 relay 和域控制面可使用以下 Caddyfile：

```text
world.example.com {
    reverse_proxy 127.0.0.1:8791
}
```

完整运行时换成自己的域名和 8790 端口。Caddy 自动处理 WebSocket 升级，客户端通过 `wss://` 连接。远程访问时，浏览器需要 HTTPS 才能使用 World 所需的 Web Crypto 接口。

## 扩展镜像

可以在派生镜像里安装项目需要的工具，然后将 Compose 里的镜像名换成自己的：

```dockerfile
FROM vaalacat/openvaala
RUN apk add --no-cache python3
```

## 多实例公开 relay

每个实例运行一个公开 relay 容器，分配独立域名和 TLS 证书，并将域名指向对应容器。在每个容器的 `/data/config.yaml` 中填写同一份列表，重启容器加载配置。

```yaml
world:
  relay_mode: public
  relay_instances:
    - https://relay-a.example.com
    - https://relay-b.example.com
    - https://relay-c.example.com
```

节点只需在 `world.relays` 中配置一个入口，例如 `https://relay.example.com`。入口可指向任一实例，也可以在全部实例间做负载均衡。挂靠的节点或控制面和拨号客户端都需要使用支持多实例的版本。扩缩容时一次只增删一个实例。下线前先发布新列表，等挂靠方刷新后再停止旧实例，正常刷新周期为 10 分钟。
