package executor

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/mcpclient"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
)

// ClientMCPServers 是各客户端连接为本对话登记的全部 MCP server：按登记先后选出生效的连接，交出它们的工具；
// 登记随客户端连接增减，从下一轮起生效。
type ClientMCPServers interface {
	// SetMCPServers 换掉一条客户端连接登记的 MCP 连接；传空表示移除。只由 Conversation 的 Run 调用，与读取可以并发。
	// 两条客户端连接登记同一个 server id 时，先登记的生效；换掉自己的登记不改变它的先后。
	SetMCPServers(clientID string, connections []mcpclient.Connection)
	// GetMCPTools 返回生效的客户端 MCP 工具，每个绑定自己的连接：没连上的等，最多等各自的启动超时，连不上的跳过并记 Warn。
	// 同 id 的 server 被先登记的挡掉时，每次读取记一条 Warn client mcp server skipped。
	GetMCPTools(ctx context.Context) ([]Tool, error)
	// GetMCPServers 返回生效的客户端 MCP 连接此刻的情况，还没连上的也立刻返回；重复 id 里被跳过的不在其中。
	GetMCPServers() []mcpclient.ConnectionInfo
}

// clientMCPServersImpl 按客户端连接记录登记的 MCP 连接；这些连接的 Run 由登记者运行。
type clientMCPServersImpl struct {
	// registrations 按客户端连接首次登记的先后排列，没有登记时为 nil。切片和每一项构造后不再变，登记变化时整体换成新切片：
	// 唯一的写入者是 Conversation 的 Run（SetMCPServers），各 Session 的 Turn 并发读取。
	registrations atomic.Pointer[[]clientMCPRegistration]
}

// clientMCPRegistration 是一条客户端连接登记的全部 MCP 连接。
type clientMCPRegistration struct {
	clientID    string
	connections []clientMCPConnection
}

// clientMCPConnection 是登记的一个 MCP 连接和它的 server id；server id 在登记时读一次，之后读取不再询问连接。
type clientMCPConnection struct {
	serverID   string
	connection mcpclient.Connection
}

// skippedClientMCPServer 是被先登记的同 id server 挡掉的一个客户端 MCP server。
type skippedClientMCPServer struct {
	serverID     string
	clientID     string
	keptClientID string
}

// NewClientMCPServers 创建空的客户端 MCP 登记列表。
func NewClientMCPServers() ClientMCPServers {
	// 1. 创建尚无登记的实例。
	return &clientMCPServersImpl{}
}

// SetMCPServers 替换或移除一个客户端登记的 MCP 连接。
func (c *clientMCPServersImpl) SetMCPServers(clientID string, connections []mcpclient.Connection) {
	// 1. 读取各连接的 server ID。
	registered := make([]clientMCPConnection, 0, len(connections))
	for _, connection := range connections {
		registered = append(registered, clientMCPConnection{
			serverID:   connection.GetInfo().ServerID,
			connection: connection,
		})
	}
	// 2. 复制登记列表，在原位置替换或移除登记，新客户端追加到末尾；没有连接且原来也没登记时直接返回。
	var registrations []clientMCPRegistration
	if loaded := c.registrations.Load(); loaded != nil {
		registrations = slices.Clone(*loaded)
	}
	index := slices.IndexFunc(registrations, func(registration clientMCPRegistration) bool { return registration.clientID == clientID })
	switch {
	case len(registered) == 0 && index >= 0:
		registrations = slices.Delete(registrations, index, index+1)
	case len(registered) == 0:
		return
	case index >= 0:
		registrations[index] = clientMCPRegistration{
			clientID:    clientID,
			connections: registered,
		}
	default:
		registrations = append(registrations, clientMCPRegistration{
			clientID:    clientID,
			connections: registered,
		})
	}
	// 3. 发布更新后的登记列表。
	c.registrations.Store(&registrations)
}

// GetMCPTools 读取生效的客户端 MCP 工具，并记录被跳过的重复 server。
func (c *clientMCPServersImpl) GetMCPTools(ctx context.Context) ([]Tool, error) {
	// 1. 选出生效连接，给重复 server 记录 Warn。
	ctx = telemetry.ContextWithAttrs(ctx, telemetry.ExecutorID(defs.ClientExecutorID))
	selected, skipped := c.selectConnections()
	for _, server := range skipped {
		telemetry.GetLogger(ctx).Warn("client mcp server skipped", "mcp_server_id", server.serverID,
			"client_id", server.clientID, "kept_client_id", server.keptClientID, "reason", "server id is already registered by another client")
	}
	// 2. 读取选中连接的工具。
	return getMCPTools(ctx, selected)
}

// GetMCPServers 返回生效的客户端 MCP 连接状态。
func (c *clientMCPServersImpl) GetMCPServers() []mcpclient.ConnectionInfo {
	// 1. 选出生效连接并读取状态。
	selected, _ := c.selectConnections()
	return getConnectionInfos(selected)
}

// selectConnections 按登记顺序筛选生效的连接。
func (c *clientMCPServersImpl) selectConnections() (selected []mcpclient.Connection, skipped []skippedClientMCPServer) {
	// 1. 读取当前登记列表。
	var registrations []clientMCPRegistration
	if loaded := c.registrations.Load(); loaded != nil {
		registrations = *loaded
	}
	// 2. 同 server ID 保留首次登记的连接，其余登记写入 skipped 并一同返回。
	keptClientIDs := make(map[string]string)
	for _, registration := range registrations {
		for _, registered := range registration.connections {
			if keptClientID, found := keptClientIDs[registered.serverID]; found {
				skipped = append(skipped, skippedClientMCPServer{
					serverID:     registered.serverID,
					clientID:     registration.clientID,
					keptClientID: keptClientID,
				})
				continue
			}
			keptClientIDs[registered.serverID] = registration.clientID
			selected = append(selected, registered.connection)
		}
	}
	return selected, skipped
}
