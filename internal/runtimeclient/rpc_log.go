package runtimeclient

import (
	"context"
	"slices"
	"time"
)

// GetRPCFrames 返回采集记录的独立副本，读取后允许再次交付变化通知。
func (c *clientImpl) GetRPCFrames(ctx context.Context) (frames []RPCFrame, err error) {
	// 1. 将读取请求交给记录所属循环。
	reply := make(chan []RPCFrame, 1)
	if err := c.sendFromCaller(ctx, &rpcLogRequested{operation: rpcLogRead, reply: reply}); err != nil {
		return nil, err
	}
	// 2. 等待独立副本或调用结束。
	select {
	case frames := <-reply:
		return frames, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.stopped:
		return nil, ErrClientStopped
	}
}

// ClearRPCFrames 清空已采集记录和拥塞计数，不重置帧序号。
func (c *clientImpl) ClearRPCFrames(ctx context.Context) error {
	// 1. 将清空请求交给记录所属循环。
	reply := make(chan []RPCFrame, 1)
	if err := c.sendFromCaller(ctx, &rpcLogRequested{operation: rpcLogClear, reply: reply}); err != nil {
		return err
	}
	// 2. 等待清空完成或调用结束。
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return ErrClientStopped
	}
}

// SetRPCLogPaused 暂停或恢复调试采集，不修改业务连接。
func (c *clientImpl) SetRPCLogPaused(ctx context.Context, paused bool) error {
	// 1. 将暂停状态交给记录所属循环。
	reply := make(chan []RPCFrame, 1)
	if err := c.sendFromCaller(ctx, &rpcLogRequested{operation: rpcLogPause, paused: paused, reply: reply}); err != nil {
		return err
	}
	// 2. 等待状态应用或调用结束。
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopped:
		return ErrClientStopped
	}
}

// handleRPCLogRequested 先应用已排队采样，再读取或修改记录。
func (c *clientImpl) handleRPCLogRequested(request *rpcLogRequested) {
	// 1. 只读取请求到达时已有的采样数量，后续网络流量不延长本次处理。
	for range len(c.rpcSamples) {
		c.appendRPCFrame(<-c.rpcSamples)
	}
	// 2. 读取返回按序独立副本；清空和暂停交付合并变化通知。
	switch request.operation {
	case rpcLogRead:
		frames := make([]RPCFrame, len(c.rpcFrames))
		for index := range frames {
			frames[index] = c.rpcFrames[(c.rpcNextIndex+index)%len(c.rpcFrames)]
			frames[index].Payload = slices.Clone(frames[index].Payload)
		}
		c.rpcLogDirty = c.rpcDropped.Load() != c.rpcReportedDropped
		c.rpcLogNotified = false
		request.reply <- frames
	case rpcLogClear:
		c.rpcFrames = nil
		c.rpcNextIndex = 0
		c.rpcDropped.Store(0)
		c.rpcLogDirty = true
		c.rpcLogNotified = false
		request.reply <- nil
	case rpcLogPause:
		c.rpcLogPaused.Store(request.paused)
		c.rpcLogDirty = true
		c.rpcLogNotified = false
		request.reply <- nil
	}
}

// appendRPCFrame 把采样写进唯一有界记录，容量耗尽时覆盖最旧记录。
func (c *clientImpl) appendRPCFrame(frame RPCFrame) {
	// 1. 暂停或未开启采集时不保存帧。
	if c.rpcLogPaused.Load() || c.opt.RPCFrameCapacity == 0 {
		return
	}
	c.rpcSequence++
	frame.Sequence = c.rpcSequence
	// 2. 记录未满时追加，满时覆盖最旧位置并推进索引。
	if len(c.rpcFrames) < c.opt.RPCFrameCapacity {
		c.rpcFrames = append(c.rpcFrames, frame)
	} else {
		c.rpcFrames[c.rpcNextIndex] = frame
		c.rpcNextIndex = (c.rpcNextIndex + 1) % len(c.rpcFrames)
	}
	c.rpcLogDirty = true
}

// observeRPCFrame 尽力采集一帧，队列拥塞只计数并合并唤醒，不阻塞网络读写。
func (c *clientConnection) observeRPCFrame(direction RPCDirection, payload []byte) {
	// 1. 关闭或暂停采集时不复制消息。
	if c.rpcSamples == nil || c.rpcLogPaused.Load() {
		return
	}
	frame := RPCFrame{ConnectionID: c.rpcConnectionID, Time: time.Now(), Direction: direction, Payload: slices.Clone(payload)}
	// 2. 投递失败只增加丢帧计数，通知循环刷新观察状态。
	select {
	case c.rpcSamples <- frame:
	default:
		c.rpcDropped.Add(1)
		select {
		case c.rpcDropWake <- struct{}{}:
		default:
		}
	}
}
