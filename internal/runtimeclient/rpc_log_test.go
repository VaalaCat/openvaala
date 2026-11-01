package runtimeclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/VaalaCat/openvaala/internal/runtimeclient"
	"github.com/VaalaCat/openvaala/pkg/defs"
	"github.com/VaalaCat/openvaala/pkg/runtimeprotocol"
	"github.com/coder/websocket"
)

// TestClientRPCLog 检查关闭采集、有界记录、独立副本、暂停清空及连接归属。
func TestClientRPCLog(t *testing.T) {
	// 1. 用真实请求覆盖关闭和开启采集。
	for _, capacity := range []int{0, 8} {
		t.Run(map[int]string{0: "disabled", 8: "bounded"}[capacity], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			client, connections := startProtocolClient(t, runtimeclient.ClientOpt{RPCFrameCapacity: capacity}, nil)
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
			firstConnection := <-connections
			for range 12 {
				if _, err := client.ListModels(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			frames, err := client.GetRPCFrames(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if capacity == 0 {
				if len(frames) != 0 {
					t.Fatalf("disabled log = %v", frames)
				}
				return
			}
			if len(frames) != capacity {
				t.Fatalf("log length = %d", len(frames))
			}
			for index, frame := range frames {
				if frame.ConnectionID != 1 || frame.Time.IsZero() || !json.Valid(frame.Payload) {
					t.Fatalf("invalid frame: %+v", frame)
				}
				if index > 0 && frame.Sequence <= frames[index-1].Sequence {
					t.Fatal("frame sequence is not increasing")
				}
			}
			// 2. 暂停后请求仍成功，调用方修改副本不影响保存记录。
			lastSequence := frames[len(frames)-1].Sequence
			if err := client.SetRPCLogPaused(ctx, true); err != nil {
				t.Fatal(err)
			}
			frames[0].Payload[0] = '!'
			for range 3 {
				if _, err := client.ListModels(ctx, ""); err != nil {
					t.Fatal(err)
				}
			}
			paused, err := client.GetRPCFrames(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if paused[len(paused)-1].Sequence != lastSequence || !json.Valid(paused[0].Payload) {
				t.Fatal("paused log changed or returned shared memory")
			}
			if err := client.ClearRPCFrames(ctx); err != nil {
				t.Fatal(err)
			}
			cleared, err := client.GetRPCFrames(ctx)
			if err != nil || len(cleared) != 0 {
				t.Fatalf("clear = %v, %v", cleared, err)
			}
			// 3. 恢复采集并重连，请求编号可重置，但连接编号与记录序号继续递增。
			if err := client.SetRPCLogPaused(ctx, false); err != nil {
				t.Fatal(err)
			}
			_ = firstConnection.CloseNow()
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionReconnecting))
			if err := client.Reconnect(ctx); err != nil {
				t.Fatal(err)
			}
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
			if _, err := client.ListModels(ctx, ""); err != nil {
				t.Fatal(err)
			}
			reconnected, err := client.GetRPCFrames(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(reconnected) == 0 {
				t.Fatal("no frames after resume")
			}
			for _, frame := range reconnected {
				if frame.ConnectionID != 2 || frame.Sequence <= lastSequence {
					t.Fatalf("reconnected frame = %+v", frame)
				}
			}
		})
	}
}

// TestClientRPCLogOverload 检查观察队列拥塞计数、合并通知和业务响应继续可用。
func TestClientRPCLogOverload(t *testing.T) {
	// 1. 单核运行时连续发送无等待的响应帧，使容量 1 的观察队列实际溢出。
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, burst := range []int{10000} {
		t.Run("burst", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			client, _ := startProtocolClient(t, runtimeclient.ClientOpt{RPCFrameCapacity: 1}, func(ctx context.Context, conn *websocket.Conn, request runtimeprotocol.JSONRPCRequest) bool {
				if request.Method != "model/list" {
					return false
				}
				for range burst {
					if err := conn.Write(ctx, websocket.MessageText, []byte(`{"id":999999,"result":{}}`)); err != nil {
						return true
					}
				}
				return false
			})
			waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
			// 2. 采样不能阻塞响应；读取记录后观察最新丢帧计数，不把日志当成完整捕获。
			if _, err := client.ListModels(ctx, ""); err != nil {
				t.Fatal(err)
			}
			frames, err := client.GetRPCFrames(ctx)
			if err != nil || len(frames) != 1 {
				t.Fatalf("frames = %v, %v", frames, err)
			}
			event := waitProtocolEvent(t, client, func(event runtimeclient.ClientEvent) bool {
				changed, ok := event.(*runtimeclient.RPCLogChanged)
				return ok && changed.DroppedFrames > 0
			}).(*runtimeclient.RPCLogChanged)
			if event.Paused {
				t.Fatal("overload paused business capture")
			}
		})
	}
}

// TestClientRPCLogConcurrentAccess 检查采集期间并发查询、暂停、清空与业务请求。
func TestClientRPCLogConcurrentAccess(t *testing.T) {
	// 1. 独立持续消费事件，不在事件读取任务内等待其他 Client 调用。
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, _ := startProtocolClient(t, runtimeclient.ClientOpt{RPCFrameCapacity: 32}, nil)
	waitProtocolEvent(t, client, isConnectionState(runtimeclient.ConnectionConnected))
	eventsDone := make(chan struct{})
	stopEvents := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for {
			select {
			case <-stopEvents:
				return
			case _, ok := <-client.Events():
				if !ok {
					return
				}
			}
		}
	}()
	defer func() { close(stopEvents); <-eventsDone }()
	// 2. 通过公开 API 并发执行读取与状态操作，race 检查记录与返回副本。
	var tasks sync.WaitGroup
	for _, operation := range []string{"request", "read", "clear", "pause"} {
		tasks.Go(func() {
			for index := range 80 {
				var err error
				switch operation {
				case "request":
					_, err = client.ListModels(ctx, "")
				case "read":
					var frames []runtimeclient.RPCFrame
					frames, err = client.GetRPCFrames(ctx)
					if len(frames) > 0 && len(frames[0].Payload) > 0 {
						frames[0].Payload[0] = '!'
					}
				case "clear":
					err = client.ClearRPCFrames(ctx)
				case "pause":
					err = client.SetRPCLogPaused(ctx, index%2 == 0)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	tasks.Wait()
}

// TestClientRPCLogKeepsTurnTerminal 检查高频调试采样期间业务内容仍完整结束。
func TestClientRPCLogKeepsTurnTerminal(t *testing.T) {
	// 1. 在真实 Runtime 上建立容量很小的观察客户端，沿用已有模型和事件夹具。
	for _, capacity := range []int{1, 16} {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			h := startClientHarness(t, clientTestToken)
			opt := runtimeclient.NewClientOpt("ws://"+h.proxy.address+"/api/v1/openvaala", clientTestToken)
			opt.RPCFrameCapacity = capacity
			client, err := runtimeclient.NewClient(opt)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- client.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				for range client.Events() {
				}
				<-done
			})
			h.client, h.events = client, client.Events()
			record := h.openNewConversation(t)
			h.submit(t, record, "stream")
			// 2. 多段模型输出产生大量 Item 通知，最终 TurnFinished 不能被调试流量丢弃。
			chunks := make([]string, 2000)
			for index := range chunks {
				chunks[index] = "chunk "
			}
			h.model.receive(t).reply <- clientModelResponse{chunks: chunks}
			events := h.collectEvents(t, isTurnFinished)
			finished := events[len(events)-1].(*runtimeclient.TurnFinished)
			if finished.SessionID != record.RootSessionID || finished.Status != defs.TurnStatusCompleted {
				t.Fatalf("terminal = %+v", finished)
			}
			frames, err := client.GetRPCFrames(t.Context())
			if err != nil || len(frames) > capacity {
				t.Fatalf("frames %d, %v", len(frames), err)
			}
		})
	}
}
