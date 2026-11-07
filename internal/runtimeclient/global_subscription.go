package runtimeclient

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
)

// globalSubscription 保存一种全局订阅（运行中对话、运行中终端）的调用方意图和当前连接上的订阅状态；
// serve 每次推送完整集合，订阅失败或被 serve 关闭后按退避重试，状态只由 Client 的事件循环读写。
type globalSubscription[T any] struct {
	client *clientImpl
	opt    globalSubscriptionOpt[T]
	// wants 保存调用方的订阅意图；其余字段属于当前连接的订阅。
	wants         bool
	isSubscribing bool
	isSubscribed  bool
	generation    uint64
	// stagedResult 是订阅请求完成前收到的最新完整集合。
	stagedResult   *T
	retryTimer     *time.Timer
	failedAttempts int
	cancelRequest  context.CancelFunc
}

// globalSubscriptionOpt 指定一种全局订阅的协议方法和交付给调用方的事件，构造后只读。
type globalSubscriptionOpt[T any] struct {
	// SubscribeMethod、UnsubscribeMethod 是订阅和退订请求；ChangedMethod、ClosedMethod 是 serve 推送完整集合和关闭订阅的通知。
	SubscribeMethod, UnsubscribeMethod, ChangedMethod, ClosedMethod string
	// NewEvent 把订阅结果变成交付给调用方的事件；result 为 nil 表示集合未知，err 是未知的原因。
	NewEvent func(result *T, err error) ClientEvent
}

// newGlobalSubscription 创建属于 client 的一种全局订阅，不发起请求。
func newGlobalSubscription[T any](client *clientImpl, opt globalSubscriptionOpt[T]) *globalSubscription[T] {
	return &globalSubscription[T]{client: client, opt: opt}
}

// subscribe 登记订阅意图，初始集合和后续变化经 Events 交付；重复调用合并。
func (s *globalSubscription[T]) subscribe(ctx context.Context) error {
	// 1. 将请求交给唯一事件循环。
	reply := make(chan struct{}, 1)
	if err := s.client.sendFromCaller(ctx, &globalSubscribeRequested[T]{subscription: s, reply: reply}); err != nil {
		return err
	}
	// 2. 等待订阅意图登记完成，不等待网络返回。
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.client.stopped:
		return ErrClientStopped
	}
}

// handleSubscribeRequested 首次登记订阅意图并在当前连接上发起订阅，之后的请求直接回复。
func (s *globalSubscription[T]) handleSubscribeRequested(ctx context.Context, input *globalSubscribeRequested[T]) {
	// 1. 首次启用时交付未知状态并发起订阅。
	if !s.wants {
		s.wants = true
		s.client.emit(ctx, s.opt.NewEvent(nil, nil))
		s.start(ctx)
	}
	input.reply <- struct{}{}
}

// handleConnectionLost 当前连接断开：作废本连接的订阅，已启用时交付未知状态。
func (s *globalSubscription[T]) handleConnectionLost(ctx context.Context, reason error) {
	s.reset()
	if s.wants {
		s.client.emit(ctx, s.opt.NewEvent(nil, reason))
	}
}

// reset 作废当前订阅请求、暂存通知和重试等待，保留调用方订阅意图。
func (s *globalSubscription[T]) reset() {
	// 1. 取消网络请求和重试，清除当前连接的订阅记录。
	s.generation++
	if s.cancelRequest != nil {
		s.cancelRequest()
		s.cancelRequest = nil
	}
	s.stopRetryTimer()
	s.isSubscribing = false
	s.isSubscribed = false
	s.stagedResult = nil
}

// start 在当前连接建立订阅，网络响应交回循环。
func (s *globalSubscription[T]) start(ctx context.Context) {
	// 1. 只有已启用且已连接时发起一次订阅。
	c := s.client
	if !s.wants || c.connection == nil || s.isSubscribing || s.isSubscribed {
		return
	}
	s.isSubscribing = true
	s.generation++
	generation, connection := s.generation, c.connection
	requestCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	s.cancelRequest = cancel
	// 2. 重试先清理可能残留的旧订阅，再订阅；请求不占用事件循环。
	retry := s.failedAttempts > 0
	c.tasks.Go(func() {
		defer cancel()
		var result T
		var err error
		if retry {
			err = connection.call(requestCtx, s.opt.UnsubscribeMethod, &runtimeprotocol.EmptyParams{}, nil)
			if err == nil {
				prepared := make(chan bool, 1)
				c.sendFromTask(requestCtx, &globalSubscriptionPrepared[T]{subscription: s, connection: connection, generation: generation, reply: prepared})
				select {
				case isCurrent := <-prepared:
					if !isCurrent {
						return
					}
				case <-requestCtx.Done():
					err = requestCtx.Err()
				}
			}
		}
		if err == nil {
			err = connection.call(requestCtx, s.opt.SubscribeMethod, &runtimeprotocol.EmptyParams{}, &result)
		}
		c.sendFromTask(ctx, &globalSubscribed[T]{subscription: s, connection: connection, generation: generation, result: result, err: err})
	})
}

// handlePrepared 重试任务退订完成：订阅仍是当前的就清除旧订阅的暂存集合，回复是否继续订阅。
func (s *globalSubscription[T]) handlePrepared(input *globalSubscriptionPrepared[T]) {
	isCurrent := input.connection == s.client.connection && input.generation == s.generation
	if isCurrent {
		s.stagedResult = nil
	}
	input.reply <- isCurrent
}

// handleSubscribed 应用订阅响应，再交付请求期间暂存的最新完整集合。
func (s *globalSubscription[T]) handleSubscribed(ctx context.Context, input *globalSubscribed[T]) {
	// 1. 丢弃旧连接、旧订阅的响应。
	if input.connection != s.client.connection || input.generation != s.generation {
		return
	}
	s.isSubscribing = false
	s.cancelRequest = nil
	// 2. 失败时标为未知，保持其余功能并安排订阅重试。
	if input.err != nil {
		s.stagedResult = nil
		s.failedAttempts++
		s.client.emit(ctx, s.opt.NewEvent(nil, input.err))
		s.retryTimer = time.NewTimer(getReconnectDelay(s.failedAttempts))
		return
	}
	// 3. 初始集合先交付，随后交付暂存通知；每条都独立于 Client 内部状态。
	s.isSubscribed = true
	s.failedAttempts = 0
	s.client.emit(ctx, s.opt.NewEvent(&input.result, nil))
	if s.stagedResult != nil {
		s.client.emit(ctx, s.opt.NewEvent(s.stagedResult, nil))
		s.stagedResult = nil
	}
}

// handleNotification 接收 serve 推送的完整集合，或订阅被 serve 关闭的通知。
func (s *globalSubscription[T]) handleNotification(ctx context.Context, method string, params json.RawMessage) {
	// 1. 未启用订阅时忽略通知，关闭时作废本次同步并安排重试。
	if !s.wants {
		return
	}
	if method == s.opt.ClosedMethod {
		var closed struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(params, &closed); err != nil {
			return
		}
		s.reset()
		s.failedAttempts++
		s.client.emit(ctx, s.opt.NewEvent(nil, errors.New(closed.Reason)))
		s.retryTimer = time.NewTimer(getReconnectDelay(s.failedAttempts))
		return
	}
	// 2. 订阅响应还没应用时保留最新完整集合，之后直接交付。
	var result T
	if err := json.Unmarshal(params, &result); err != nil {
		return
	}
	if s.isSubscribing {
		s.stagedResult = &result
		return
	}
	if s.isSubscribed {
		s.client.emit(ctx, s.opt.NewEvent(&result, nil))
	}
}

// getRetryTimer 返回订阅重试等待的通道；没有等待时返回 nil，永远不会就绪。
func (s *globalSubscription[T]) getRetryTimer() <-chan time.Time {
	if s.retryTimer == nil {
		return nil
	}
	return s.retryTimer.C
}

// handleRetryTimerElapsed 重试等待结束，再次发起订阅。
func (s *globalSubscription[T]) handleRetryTimerElapsed(ctx context.Context) {
	s.retryTimer = nil
	s.start(ctx)
}

// stopRetryTimer 取消订阅重试等待。
func (s *globalSubscription[T]) stopRetryTimer() {
	if s.retryTimer != nil {
		s.retryTimer.Stop()
		s.retryTimer = nil
	}
}
