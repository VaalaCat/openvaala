package runtimeserver

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/world"
	"github.com/VaalaCat/openvaala/pkg/worldserver"
)

func handleWorldGetStatus(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*world.NodeStatus, error) {
	status := world.NodeStatus{AdminEnabled: connection.worldRunning.ServerEnabled}
	if connection.worldNode == nil {
		if connection.worldServer != nil {
			attached := connection.worldServer.GetRelayAttachments()
			for _, base := range slices.Sorted(maps.Keys(attached)) {
				status.RelayAttachments = append(status.RelayAttachments, world.RelayStatus{URL: base, Connected: attached[base]})
			}
		}
		s := connection.server
		if s.opt.RegisteredNode.Domain != "" {
			status.NodeAddr = s.opt.RegisteredNode.NodeAddress()
		}
		if len(s.remoteTarget.Entries.Relays) > 0 {
			target := s.remoteTarget
			target.Token = connection.token
			link, err := world.FormatRemoteAccessURL(s.opt.RemoteAccessWebURL, target)
			if err != nil {
				return nil, err
			}
			status.RemoteAccessURL = link
			status.RemoteAccessLocalOnly = world.IsLocalPageURL(link)
			for _, base := range target.Entries.Relays {
				connected, _ := s.remoteAttachments.Load(base)
				status.RemoteAccessRelays = append(status.RemoteAccessRelays, world.RelayStatus{URL: base, Connected: connected})
			}
		}
		return &status, nil
	}
	status, err := connection.worldNode.GetStatus(ctx, connection.token)
	status.AdminEnabled = connection.worldRunning.ServerEnabled
	return &status, worldRPCError(err)
}

func handleWorldSubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	key := subscriptionKey{kind: subscriptionKindWorld}
	subscriptionCtx, registration, err := connection.registerSubscription(ctx, key)
	if err != nil {
		return nil, err
	}
	events, err := connection.worldNode.Subscribe(subscriptionCtx)
	if err != nil {
		connection.removeSubscription(key, registration)
		return nil, worldRPCError(err)
	}
	connection.startAfterResponse(func() {
		forwardGlobalSubscription(ctx, connection, key, registration, subscriptionCtx, events,
			func(event world.NodeNotification) *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{Method: "world/messageUpdated", Params: event}
			},
			func() *runtimeprotocol.JSONRPCMessage {
				return &runtimeprotocol.JSONRPCMessage{
					Method: "world/messageUpdatedClosed",
					Params: &runtimeprotocol.WorldMessageUpdatedClosedParams{Reason: "world message subscription closed"},
				}
			})
	})
	return &runtimeprotocol.EmptyResult{}, nil
}

func handleWorldUnsubscribe(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*runtimeprotocol.EmptyResult, error) {
	connection.cancelSubscription(ctx, subscriptionKey{kind: subscriptionKindWorld})
	return &runtimeprotocol.EmptyResult{}, nil
}

func handleWorldListPeers(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*world.ListPeersResult, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	result, err := connection.worldNode.ListPeers(ctx)
	return &result, worldRPCError(err)
}

func handleWorldSendMessage(ctx context.Context, connection *serverConnection, params *world.SendMessageRequest) (any, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	message, finished, err := connection.worldNode.EnqueueMessage(ctx, *params)
	if finished == nil {
		return &message, worldRPCError(err)
	}
	return deferredResponse(func() (any, error) {
		result, err := connection.worldNode.WaitMessage(ctx, message, finished)
		return &result, worldRPCError(err)
	}), nil
}

func handleWorldForward(ctx context.Context, connection *serverConnection, params *world.ForwardRequest) (*world.WorldMessage, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	result, err := connection.worldNode.Forward(ctx, connection.runtime, *params)
	return &result, worldRPCError(err)
}

func handleWorldListMessages(ctx context.Context, connection *serverConnection, params *world.ListMessagesParams) (*world.MessagePage, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	result, err := connection.worldNode.ListMessages(ctx, *params)
	return &result, worldRPCError(err)
}

func handleWorldCancelMessage(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WorldMessageParams) (*world.WorldMessage, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	result, err := connection.worldNode.CancelMessage(ctx, params.MessageID)
	return &result, worldRPCError(err)
}

func handleWorldListThreads(ctx context.Context, connection *serverConnection, params *world.ListThreadsParams) (*world.ThreadPage, error) {
	if connection.worldNode == nil {
		return nil, worldRPCError(errWorldNodeDisabled)
	}
	result, err := connection.worldNode.ListThreads(ctx, *params)
	return &result, worldRPCError(err)
}

func handleWorldAdminListUsers(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) (*worldserver.ListUsersResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	users, err := connection.worldServer.ListUsers(ctx)
	return &worldserver.ListUsersResult{Users: users}, worldAdminRPCError(ctx, err)
}

func handleWorldAdminGetACLMatrix(ctx context.Context, connection *serverConnection, params *worldserver.GetACLMatrixRequest) (*worldserver.ACLMatrix, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	matrix, err := connection.worldServer.GetACLMatrix(ctx, params.Rules)
	return &matrix, worldAdminRPCError(ctx, err)
}

func handleWorldAdminListNodes(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) ([]worldserver.AdminNode, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, err := connection.worldServer.GetNodes(ctx)
	return result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminRevokeNode(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WorldNodeParams) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	err := connection.worldServer.RevokeNode(ctx, params.NodeID)
	return &runtimeprotocol.EmptyResult{}, worldAdminRPCError(ctx, err)
}

func handleWorldAdminRevokeOwner(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WorldOwnerParams) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	err := connection.worldServer.RevokeOwner(ctx, params.Owner)
	return &runtimeprotocol.EmptyResult{}, worldAdminRPCError(ctx, err)
}

func handleWorldAdminListTokens(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) ([]worldserver.JoinTokenInfo, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, err := connection.worldServer.GetJoinTokens(ctx)
	return result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminCreateToken(ctx context.Context, connection *serverConnection, params *worldserver.CreateJoinTokenRequest) (*worldserver.CreateJoinTokenResponse, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, err := connection.worldServer.CreateJoinToken(ctx, *params)
	return &result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminRevokeToken(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WorldTokenParams) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	err := connection.worldServer.RevokeJoinToken(ctx, params.ID)
	return &runtimeprotocol.EmptyResult{}, worldAdminRPCError(ctx, err)
}

func handleWorldAdminGetACL(ctx context.Context, connection *serverConnection, _ *runtimeprotocol.EmptyParams) ([]worldserver.ACLRule, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, err := connection.worldServer.GetACL(ctx)
	return result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminReplaceACL(ctx context.Context, connection *serverConnection, params *[]worldserver.ACLRule) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	err := connection.worldServer.ReplaceACL(ctx, *params)
	return &runtimeprotocol.EmptyResult{}, worldAdminRPCError(ctx, err)
}

var (
	errWorldNodeDisabled  = errors.New("world node is not enabled")
	errWorldAdminDisabled = errors.New("world administration is not enabled")
)

func worldRPCError(err error) error {
	if err == nil {
		return nil
	}
	return newWorldRPCError(world.ErrorCode(err), err.Error())
}

func worldAdminRPCError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, worldserver.ErrDomainKeyConflict) {
		return newWorldRPCError("world_domain_name_taken", err.Error())
	}
	var invalid *worldserver.InvalidAdminRequestError
	if errors.As(err, &invalid) {
		return newWorldRPCError("invalid_arguments", err.Error())
	}
	telemetry.GetLogger(ctx).Warn("world control request failed", "error", err)
	return newWorldRPCError("world_request_failed", "world control request failed")
}

func newWorldRPCError(code, message string) *runtimeprotocol.JSONRPCError {
	rpcCode := runtimeprotocol.ErrorCodeRuntime
	if code == "invalid_arguments" {
		rpcCode = runtimeprotocol.ErrorCodeInvalidParams
	}
	return &runtimeprotocol.JSONRPCError{Code: rpcCode, Message: message, Data: &runtimeprotocol.JSONRPCErrorData{Code: code}}
}

func handleWorldAdminListDomains(ctx context.Context, connection *serverConnection, params *runtimeprotocol.EmptyParams) (*worldserver.ListDomainsResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, err := connection.worldServer.GetDomains(ctx)
	return &result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminAddDomain(ctx context.Context, connection *serverConnection, params *worldserver.AddDomainRequest) (*worldserver.PeerDomain, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	result, _, err := connection.worldServer.AddDomain(ctx, params.Card)
	return &result, worldAdminRPCError(ctx, err)
}

func handleWorldAdminDomainDecision(accept bool) func(context.Context, *serverConnection, *runtimeprotocol.WorldDomainParams) (*runtimeprotocol.EmptyResult, error) {
	return func(ctx context.Context, c *serverConnection, p *runtimeprotocol.WorldDomainParams) (*runtimeprotocol.EmptyResult, error) {
		if c.worldServer == nil {
			return nil, worldRPCError(errWorldAdminDisabled)
		}
		return &runtimeprotocol.EmptyResult{}, worldAdminRPCError(ctx, c.worldServer.DecideDomain(ctx, p.Domain, accept))
	}
}

func handleWorldAdminDeleteDomain(ctx context.Context, connection *serverConnection, params *runtimeprotocol.WorldDomainParams) (*runtimeprotocol.EmptyResult, error) {
	if connection.worldServer == nil {
		return nil, worldRPCError(errWorldAdminDisabled)
	}
	err := connection.worldServer.DeleteDomain(ctx, params.Domain)
	result := runtimeprotocol.EmptyResult{}
	return &result, worldAdminRPCError(ctx, err)
}
