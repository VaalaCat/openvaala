package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeserver"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"github.com/VaalaCat/openvaala/www"
	"github.com/gin-gonic/gin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// worldCommandFlags 保存本次 CLI 调用选择的节点、控制面及输出参数。
type worldCommandFlags struct {
	serverURL, adminToken                        string
	jsonOutput                                   bool
	name, thread, note, conversation             string
	messageDirection, messageStatus, messageKind string
	messagePeers                                 []string
	messageLimit                                 int
	messageCursorID                              string
	wait                                         bool
	tokenOwner, revokeOwner, aclFile             string
	reusable                                     bool
	tags                                         []string
	ttl                                          time.Duration
}

// newWorldCommand 注册节点、独立控制面及其运维命令。
func newWorldCommand() *cobra.Command {
	// 1. 注册节点命令。join 直接注册身份，其他命令复用 Runtime 连接。
	f := &worldCommandFlags{}
	group := &cobra.Command{Use: "world", Short: "Manage World nodes and messages"}
	group.PersistentFlags().BoolVar(&f.jsonOutput, "json", false, "write machine-readable JSON")
	join := &cobra.Command{Use: "join <invite-code>", Short: "Register this node with a world server", Args: cobra.ExactArgs(1), RunE: buildCLIHandlerWithArguments(f.joinWorld)}
	join.Flags().StringVar(&f.name, "name", "", "node name (default: configured name or hostname)")
	group.AddCommand(join)
	for _, op := range []struct {
		use, short string
		n          int
	}{
		{"status", "Show this node's connection status", 0},
		{"peers", "List visible peers", 0},
		{"send <address> <message>", "Send a message to a peer", 2},
		{"forward <conversation-id> <address>", "Forward the latest assistant reply", 2},
		{"messages", "List World messages", 0},
		{"cancel <message-id>", "Cancel an offline message", 1},
	} {
		cmd := &cobra.Command{Use: op.use, Short: op.short, Args: cobra.ExactArgs(op.n), RunE: buildCLIHandlerWithArguments(f.runNodeCommand)}
		addURLFlag(cmd.Flags())
		switch cmd.Name() {
		case "send":
			cmd.Flags().StringVar(&f.thread, "thread", "", "continue this thread")
			cmd.Flags().BoolVar(&f.wait, "wait", false, "wait for the peer's reply")
		case "forward":
			cmd.Flags().StringVar(&f.note, "note", "", "note to append")
		case "messages":
			cmd.Flags().StringVar(&f.conversation, "conversation", "", "filter by conversation ID")
			cmd.Flags().StringVar(&f.messageDirection, "direction", "", "filter by inbound or outbound direction")
			cmd.Flags().StringSliceVar(&f.messagePeers, "peer", nil, "filter by peer address (repeatable)")
			cmd.Flags().StringVar(&f.messageKind, "kind", "", "filter by message or reply kind")
			cmd.Flags().StringVar(&f.messageStatus, "status", "", "filter by message status")
			cmd.Flags().IntVar(&f.messageLimit, "limit", 50, "maximum messages to return (up to 200)")
			cmd.Flags().StringVar(&f.messageCursorID, "cursor-id", "", "continue from next_cursor.id")
		}
		group.AddCommand(cmd)
	}
	// 2. 注册独立控制面、relay 和管理命令。
	serve := &cobra.Command{Use: "serve", Short: "Serve the configured World control plane or relay without a Runtime", Args: cobra.NoArgs, RunE: BuildCLIHandler(f.runWorldServe)}
	serve.Flags().String("listen", "", "World listen address (host:port)")
	serve.Flags().String("relay-mode", "", "override world.relay_mode for this run (off, domain, public). Defaults to domain with the control plane, off otherwise")
	serve.Flags().StringVar(&f.adminToken, "admin-token", "", "world control server admin token")
	group.AddCommand(serve)
	token := &cobra.Command{Use: "token", Short: "Manage join tokens"}
	node := &cobra.Command{Use: "node", Short: "Manage registered nodes"}
	acl := &cobra.Command{Use: "acl", Short: "Manage access rules"}
	for _, cmd := range []*cobra.Command{token, node, acl} {
		cmd.PersistentFlags().StringVar(&f.serverURL, "server", "", "world control server URL")
		cmd.PersistentFlags().StringVar(&f.adminToken, "admin-token", "", "world control server admin token")
		group.AddCommand(cmd)
	}
	runAdmin := buildCLIHandlerWithArguments(f.runAdminCommand)
	create := &cobra.Command{Use: "create", Short: "Create a join token", Args: cobra.NoArgs, RunE: runAdmin}
	create.Flags().StringVar(&f.tokenOwner, "owner", "", "owner assigned to the joining node")
	_ = create.MarkFlagRequired("owner")
	create.Flags().StringSliceVar(&f.tags, "tags", nil, "comma-separated node tags")
	create.Flags().BoolVar(&f.reusable, "reusable", false, "allow multiple registrations")
	create.Flags().DurationVar(&f.ttl, "ttl", 24*time.Hour, "token lifetime")
	token.AddCommand(create, &cobra.Command{Use: "ls", Short: "List issued tokens", Args: cobra.NoArgs, RunE: runAdmin}, &cobra.Command{Use: "revoke <id>", Short: "Revoke a token", Args: cobra.ExactArgs(1), RunE: runAdmin})
	revoke := &cobra.Command{Use: "revoke [node-id]", Short: "Revoke one node or all nodes of an owner", Args: cobra.MaximumNArgs(1), RunE: runAdmin}
	revoke.Flags().StringVar(&f.revokeOwner, "owner", "", "revoke all nodes belonging to this owner")
	node.AddCommand(revoke, &cobra.Command{Use: "ls", Short: "List registered nodes", Args: cobra.NoArgs, RunE: runAdmin})
	set := &cobra.Command{Use: "set", Short: "Replace access rules", Args: cobra.NoArgs, RunE: runAdmin}
	set.Flags().StringVar(&f.aclFile, "file", "", "YAML file containing a list of access rules")
	_ = set.MarkFlagRequired("file")
	acl.AddCommand(set, &cobra.Command{Use: "ls", Short: "List access rules", Args: cobra.NoArgs, RunE: runAdmin})
	return group
}

// runNodeCommand 通过 Runtime RPC 操作 World 节点。
func (f *worldCommandFlags) runNodeCommand(cmd *cobra.Command, args *AppArgs, argv []string) error {
	return runRuntimeCommand(cmd.Context(), args, func(ctx context.Context, client runtimeclient.Client, _ runtimeConnection, _ string) error {
		var result any
		var err error
		switch cmd.Name() {
		case "status":
			result, err = client.GetWorldStatus(ctx)
		case "peers":
			result, err = client.ListWorldPeers(ctx)
		case "send":
			result, err = client.SendWorldMessage(ctx, world.SendMessageRequest{
				To: argv[0], Message: argv[1], ThreadID: f.thread, Wait: f.wait,
			})
		case "forward":
			result, err = client.ForwardWorldConversation(ctx, world.ForwardRequest{
				ConversationID: defs.ConversationID(argv[0]), To: argv[1], Note: f.note,
			})
		case "messages":
			params := world.ListMessagesParams{
				ConversationID: f.conversation, Direction: f.messageDirection,
				Kind: f.messageKind, Status: f.messageStatus, PeerAddresses: f.messagePeers,
			}
			if cmd.Flags().Changed("limit") {
				params.Limit = &f.messageLimit
			}
			if f.messageCursorID != "" {
				params.CursorID = &f.messageCursorID
			}
			result, err = client.ListWorldMessages(ctx, params)
		case "cancel":
			result, err = client.CancelWorldMessage(ctx, argv[0])
		}
		if err != nil {
			return err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return printWorldResult(args.Stdout, f.jsonOutput, data)
	})
}

// runAdminCommand 通过控制面管理凭据操作入网码、节点和 ACL。
func (f *worldCommandFlags) runAdminCommand(cmd *cobra.Command, args *AppArgs, argv []string) error {
	// 1. 默认连接本机控制面，显式地址用于远程管理。
	base := f.serverURL
	token := f.adminToken
	if base != "" && token == "" {
		return errors.New("--admin-token is required with --server")
	}
	if base == "" {
		token = cmp.Or(token, args.Config.World.Server.AdminToken)
		listen := args.Config.World.Server.Listen
		if args.Config.World.Server.Enabled {
			file, reachable, err := getReachableRuntimeServerFile(cmd.Context(), getRuntimeServerFilePath(args.Config))
			if err != nil {
				return err
			}
			if reachable {
				endpoint, err := url.Parse(file.URL)
				if err != nil {
					return err
				}
				listen = endpoint.Host
			}
		}
		host, port, err := net.SplitHostPort(listen)
		if err != nil {
			return err
		}
		if host == "" || net.ParseIP(host).IsUnspecified() {
			host = "localhost"
		}
		base = "http://" + net.JoinHostPort(host, port)
	}
	path := "/" + cmd.Parent().Name()
	if cmd.Parent().Name() != "acl" {
		path += "s"
	}
	method := http.MethodGet
	var body any
	// 2. 按参数提交管理操作，并保留服务端的业务错误。
	switch cmd.Name() {
	case "create":
		method, body = http.MethodPost, worldserver.CreateJoinTokenRequest{Owner: f.tokenOwner, Tags: f.tags, Reusable: f.reusable, ExpiresAt: time.Now().Add(f.ttl).UnixMilli()}
	case "revoke":
		if cmd.Parent().Name() == "token" {
			method, path = http.MethodDelete, path+"/"+url.PathEscape(argv[0])
			break
		}
		if (len(argv) == 1) == (f.revokeOwner != "") {
			return errors.New("specify one node ID or --owner")
		}
		method = http.MethodPost
		if f.revokeOwner != "" {
			path = "/owners/" + url.PathEscape(f.revokeOwner) + "/revoke"
		} else {
			path += "/" + url.PathEscape(argv[0]) + "/revoke"
		}
	case "set":
		file, err := os.Open(f.aclFile)
		if err != nil {
			return err
		}
		defer file.Close()
		var rules []worldserver.ACLRule
		decoder := yaml.NewDecoder(file)
		decoder.KnownFields(true)
		if err := decoder.Decode(&rules); err != nil {
			return err
		}
		method, body = http.MethodPut, rules
	}
	return printWorldRequest(cmd.Context(), args.Stdout, f.jsonOutput, method, strings.TrimRight(base, "/")+"/world/v1/admin"+path, token, body)
}

// runWorldServe 在独立监听器上运行配置启用的控制面和 relay。
func (f *worldCommandFlags) runWorldServe(ctx context.Context, args *AppArgs) (err error) {
	// 1. 启动遥测，退出时最后关闭。
	tel, shutdownTelemetry, err := newTelemetry(ctx, args.Config, args.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	}()
	// 2. 绑定独立监听地址。
	cfg := args.Config.World
	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	// 3. 控制面自行运行配置的 relay，单独部署时只挂载公共 relay 路由。
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	landing := "/web/v1/"
	pageRole := "relay"
	var server worldserver.WorldServer
	var publicRelay *worldserver.Relay
	if cfg.Server.Enabled {
		pageRole = "control"
		cfg.Node.Enabled = false
		cfg.Server.AdminToken = cmp.Or(f.adminToken, cfg.Server.AdminToken)
		server, err = newConfiguredWorldServer(cfg, listener.Addr().(*net.TCPAddr), args.Config.GetDataDirectory(), config.NewConfigFiles(args.ConfigOpt), tel)
		if err != nil {
			return err
		}
		server.RegisterRoutes(engine)
		world.RegisterDirectRoutes(engine, nil, server)
		if cfg.GetRelayMode() != "public" {
			landing = "/web/v1/world/admin"
		}
	} else if cfg.GetRelayMode() == "public" {
		publicRelay = worldserver.NewRelay(cfg.RelayInstances)
		publicRelay.RegisterRoutes(engine)
		engine.GET("/world/v1/healthz", worldserver.HandleHealthz)
	}
	if assets := www.GetAssets(); assets != nil {
		runtimeserver.RegisterWebRoutes(engine, assets, landing, pageRole)
	}
	if f.jsonOutput {
		err = json.NewEncoder(args.Stdout).Encode(map[string]string{"listen": listener.Addr().String()})
	} else {
		_, err = fmt.Fprintf(args.Stdout, "serving OpenVaala World on %s\n", listener.Addr())
	}
	if err != nil {
		return err
	}
	address := listener.Addr().(*net.TCPAddr)
	ctx = telemetry.ContextWithTelemetry(ctx, tel)
	ctx = telemetry.ContextWithAttrs(ctx, telemetry.ServerAddress(address.IP.String()), telemetry.ServerPort(address.Port))
	telemetry.GetLogger(ctx).Info("world server listening")
	_, err = runServeHTTP(ctx, listener, engine, nil, nil, nil, server, publicRelay, nil, false)
	return err
}

// newConfiguredWorldServer 校验控制面配置，首次运行时将随机管理令牌原子写回。
func newConfiguredWorldServer(cfg config.WorldConfig, address *net.TCPAddr, dataDirectory string, files *config.ConfigFiles, tel *telemetry.Telemetry) (worldserver.WorldServer, error) {
	// 1. 缺少凭据时生成随机值，带域名和初始规则创建控制面。
	token := cfg.Server.AdminToken
	if token == "" {
		var err error
		token, err = newRandomHex(32)
		if err != nil {
			return nil, err
		}
	}
	opt := worldserver.NewWorldServerOpt(cfg.Server.Domain, filepath.Join(dataDirectory, "worldserver", "world.db"), token)
	opt.Channels = world.NewDomainChannels()
	if address.IP.IsUnspecified() {
		address = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: address.Port}
	}
	opt.LocalRelayURL = "http://" + address.String()
	opt.Entries.Relays = cfg.Relays
	if cfg.Node.Enabled {
		opt.Entries.DirectURLs = cfg.Node.DirectURLs
	}
	opt.RelayMode = cfg.GetRelayMode()
	opt.RelayInstances = cfg.RelayInstances
	opt.StaticACL, opt.Telemetry = cfg.Server.ACL, tel
	server, err := worldserver.NewWorldServer(opt)
	if err != nil {
		return nil, err
	}
	// 2. 校验成功后才保存首次生成的令牌。
	if cfg.Server.AdminToken == "" {
		if err := files.SetWorldAdminToken(token); err != nil {
			return nil, err
		}
	}
	return server, nil
}

// joinWorld 用本机身份签名入网请求，注册成功后写入节点配置。
func (f *worldCommandFlags) joinWorld(cmd *cobra.Command, args *AppArgs, argv []string) error {
	// 1. 准备节点名与私有身份。
	serverURL, err := worldserver.ParseJoinCode(argv[0])
	if err != nil {
		return err
	}
	name, err := world.GetJoinNodeName(cmp.Or(f.name, args.Config.World.Node.Name))
	if err != nil {
		return err
	}
	id, err := world.LoadOrCreateIdentity(filepath.Join(args.Config.GetDataDirectory(), "world", "identity.json"))
	if err != nil {
		return err
	}
	// 2. 提交签名和公钥，仅在控制面确认后启用节点。
	result, err := world.RegisterNode(cmd.Context(), id, serverURL, name, argv[0])
	if err != nil {
		return err
	}
	if err := config.NewConfigFiles(args.ConfigOpt).SetWorldNodeRegistration(result.NodeName, result.Domain, serverURL); err != nil {
		return err
	}
	// 3. 标准输出仅包含注册结果，重启提示写到标准错误。
	fmt.Fprintln(args.Stderr, "Restart any running openvaala serve to enable this node.")
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return printWorldResult(args.Stdout, f.jsonOutput, data)
}

func requestWorld(ctx context.Context, method, endpoint, token string, body any) (json.RawMessage, error) {
	// 1. 使用调用方上下文和凭据发送请求。
	var content io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		content = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, content)
	if err != nil {
		return nil, err
	}
	if content != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 2. 返回 JSON 原文或带状态码的控制面、节点错误。
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("world request: %s %s", resp.Status, bytes.TrimSpace(data))
	}
	return data, nil
}

func printWorldRequest(ctx context.Context, out io.Writer, jsonOutput bool, method, endpoint, token string, body any) error {
	// 1. 将成功响应写入命令输出。
	data, err := requestWorld(ctx, method, endpoint, token, body)
	if err != nil {
		return err
	}
	return printWorldResult(out, jsonOutput, data)
}

func printWorldResult(out io.Writer, jsonOutput bool, data json.RawMessage) error {
	// 1. 无正文的运维操作返回成功，其他响应按用户选择输出紧凑或缩进 JSON。
	if len(data) == 0 {
		if !jsonOutput {
			_, err := fmt.Fprintln(out, "ok")
			return err
		}
		data = json.RawMessage("null")
	}
	encoder := json.NewEncoder(out)
	if !jsonOutput {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(data)
}
