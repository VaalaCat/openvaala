package world

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

type testFrameTransport struct {
	alterWrite func([]byte)
	in         chan []byte
	out        chan []byte
	done       chan struct{}
	stop       *sync.Once
}

func testTransportPair() (*testFrameTransport, *testFrameTransport) {
	// 1. 两个方向独立排队，关闭任一端会唤醒另一端。
	a := &testFrameTransport{
		in:   make(chan []byte, 8),
		out:  make(chan []byte, 8),
		done: make(chan struct{}),
		stop: new(sync.Once),
	}
	b := &testFrameTransport{
		in:   a.out,
		out:  a.in,
		done: a.done,
		stop: a.stop,
	}
	return a, b
}

func (p *testFrameTransport) WriteFrame(ctx context.Context, frame []byte) error {
	// 1. 投递整帧或返回关闭、取消错误。
	select {
	case <-p.done:
		return io.ErrClosedPipe
	default:
	}
	frame = bytes.Clone(frame)
	if p.alterWrite != nil {
		p.alterWrite(frame)
	}
	select {
	case p.out <- frame:
		return nil
	case <-p.done:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *testFrameTransport) ReadFrame(ctx context.Context) ([]byte, error) {
	// 1. 读取下一帧或返回关闭、取消错误。
	select {
	case <-p.done:
		return nil, io.ErrClosedPipe
	default:
	}
	select {
	case frame := <-p.in:
		return frame, nil
	case <-p.done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *testFrameTransport) Close() error {
	// 1. 同时释放读写两端。
	p.stop.Do(func() { close(p.done) })
	return nil
}

func TestWorldChannelPurposes(t *testing.T) {
	alice, bob, other := testNodeIdentity(t), testNodeIdentity(t), testNodeIdentity(t)
	for _, tc := range []struct {
		name    string
		local   *NodeIdentity
		purpose ChannelPurpose
		key     []byte
		ok      bool
	}{
		{"anonymous rpc", nil, ChannelPurposeRPC, bob.SignPublic, true},
		{"anonymous world", nil, ChannelPurposeWorld, bob.SignPublic, false},
		{"anonymous domain", nil, ChannelPurposeDomain, bob.SignPublic, false},
		{"world", &alice, ChannelPurposeWorld, bob.SignPublic, true},
		{"rpc", &alice, ChannelPurposeRPC, bob.SignPublic, true},
		{"domain", &alice, ChannelPurposeDomain, bob.SignPublic, true},
		{"unknown purpose", &alice, "invalid", bob.SignPublic, false},
		{"wrong target", &alice, ChannelPurposeWorld, other.SignPublic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			a, b := testTransportPair()
			defer a.Close()
			result := make(chan AcceptedChannel, 1)
			go func() { ch, _ := AcceptSecureChannel(ctx, b, bob); result <- ch }()
			_, err := DialSecureChannel(ctx, a, tc.local, tc.key, tc.purpose)
			accepted := <-result
			if (err == nil) != tc.ok || (accepted.Channel != nil) != tc.ok || ctx.Err() != nil {
				t.Fatalf("dial=%v accept=%+v", err, accepted)
			}
			if tc.ok && (accepted.Purpose != tc.purpose || tc.local == nil && len(accepted.PeerKey) != 0 || tc.local != nil && !bytes.Equal(accepted.PeerKey, alice.SignPublic)) {
				t.Fatal(accepted)
			}
		})
	}
}

func TestWorldChannelFrameLimit(t *testing.T) {
	// 1. 每个场景使用新的通道和通讯录中的公钥。
	alice, bob := testNodeIdentity(t), testNodeIdentity(t)
	for _, scenario := range []string{"duplex", "tamper", "replay", "out of order", "reflection"} {
		t.Run(scenario, func(t *testing.T) {
			// 1. 通过帧传输建立双向通道。
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			a, b := testTransportPair()
			defer a.Close()
			type accepted struct {
				channel SecureChannel
				err     error
			}
			result := make(chan accepted, 1)
			go func() {
				ch, err := AcceptSecureChannel(ctx, b, bob)
				result <- accepted{
					channel: ch.Channel,
					err:     err,
				}
			}()
			dial, err := DialSecureChannel(ctx, a, &alice, bob.SignPublic, ChannelPurposeWorld)
			accept := <-result
			if err != nil || accept.err != nil {
				t.Fatalf("dial=%v accept=%v", err, accept.err)
			}
			// 2. 正常场景并发收发；其余场景检查传输上的密文帧。
			message := []byte("hello from alice, for bob only")
			if scenario == "duplex" {
				message = bytes.Repeat([]byte{0xaf}, 16<<20)
				sendErr := make(chan error, 1)
				a.alterWrite = func(frame []byte) {
					if len(frame) != (16<<20)+24 {
						t.Errorf("frame size %d", len(frame))
					}
				}
				go func() { sendErr <- dial.WriteMessage(ctx, message) }()
				reply := bytes.Repeat([]byte{0xbc}, 16<<20)
				if err = accept.channel.WriteMessage(ctx, reply); err != nil {
					t.Fatal(err)
				}
				got, err := dial.ReadMessage(ctx)
				if err != nil || !bytes.Equal(got, reply) {
					t.Fatalf("reply size=%d error=%v", len(got), err)
				}
				if err = <-sendErr; err != nil {
					t.Fatal(err)
				}
				got, err = accept.channel.ReadMessage(ctx)
				if err != nil || !bytes.Equal(got, message) {
					t.Fatalf("message size=%d error=%v", len(got), err)
				}
				if err := dial.WriteMessage(ctx, append(message, 0)); err == nil {
					t.Fatal("oversize accepted")
				}
				return
			}
			if err = dial.WriteMessage(ctx, message); err != nil {
				t.Fatal(err)
			}
			frame := <-b.in
			if len(frame) != 8+len(message)+16 || bytes.Contains(frame, message) {
				t.Fatalf("unexpected encrypted frame: %x", frame)
			}
			receiver := accept.channel
			switch scenario {
			case "tamper":
				frame[len(frame)-1] ^= 1
			case "replay":
				b.in <- bytes.Clone(frame)
				if _, err = receiver.ReadMessage(ctx); err != nil {
					t.Fatal(err)
				}
			case "out of order":
				if err = dial.WriteMessage(ctx, message); err != nil {
					t.Fatal(err)
				}
				next := <-b.in
				if binary.LittleEndian.Uint64(next) != binary.LittleEndian.Uint64(frame)+1 {
					t.Fatal("nonce did not increase by one")
				}
				frame = next
			case "reflection":
				receiver = dial
			}
			if scenario == "reflection" {
				a.in <- frame
			} else {
				b.in <- frame
			}
			// 3. 异常密文不得交付消息，并立即关闭帧传输。
			got, err := receiver.ReadMessage(ctx)
			if err == nil || got != nil || ctx.Err() != nil {
				t.Fatalf("payload=%q error=%v", got, err)
			}
			select {
			case <-a.done:
			default:
				t.Fatal("transport left open")
			}
		})
	}
}

func TestSecureChannelHandshakeTamper(t *testing.T) {
	// 1. 检查用途、身份标志和响应密钥均受握手验证。
	alice, bob := testNodeIdentity(t), testNodeIdentity(t)
	for _, side := range []string{"hello", "ready", "purpose", "anonymous world", "unknown flag"} {
		t.Run(side, func(t *testing.T) {
			// 1. 转发握手帧时改变已签名的一个字节。
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			a, b := testTransportPair()
			defer a.Close()
			sender := a
			if side == "ready" {
				sender = b
			}
			identity, purpose := &alice, ChannelPurposeWorld
			switch side {
			case "anonymous world":
				identity, purpose = nil, ChannelPurposeRPC
				sender.alterWrite = func(frame []byte) { frame[1] = 1 }
			case "purpose":
				sender.alterWrite = func(frame []byte) { frame[1] = 3 }
			case "unknown flag":
				sender.alterWrite = func(frame []byte) { frame[2] = 2 }
			default:
				sender.alterWrite = func(frame []byte) { frame[16] ^= 1 }
			}
			// 2. 验证接收篡改帧的一方立即拒绝通道。
			acceptErr := make(chan error, 1)
			go func() {
				_, err := AcceptSecureChannel(ctx, b, bob)
				acceptErr <- err
			}()
			_, dialErr := DialSecureChannel(ctx, a, identity, bob.SignPublic, purpose)
			err := <-acceptErr
			if dialErr == nil || (side != "ready" && err == nil) || ctx.Err() != nil {
				t.Fatalf("dial=%v accept=%v context=%v", dialErr, err, ctx.Err())
			}
			select {
			case <-a.done:
			default:
				t.Fatal("transport left open")
			}
		})
	}
}

func TestWorldChannelVectors(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Purpose         ChannelPurpose `json:"purpose"`
			ID              []byte         `json:"channel_id"`
			DialSeed        []byte         `json:"dial_sign_seed"`
			AcceptSeed      []byte         `json:"accept_sign_seed"`
			DialBox         []byte         `json:"dial_box_private"`
			AcceptBox       []byte         `json:"accept_box_private"`
			DialEphemeral   []byte         `json:"dial_ephemeral_private"`
			AcceptEphemeral []byte         `json:"accept_ephemeral_private"`
			Hello           []byte         `json:"hello"`
			Ready           []byte         `json:"ready"`
			DialPlaintext   []byte         `json:"dial_plaintext"`
			AcceptPlaintext []byte         `json:"accept_plaintext"`
			DialFrame       []byte         `json:"dial_frame"`
			AcceptFrame     []byte         `json:"accept_frame"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("testdata/channel-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &vectors); err != nil || len(vectors.Cases) != 2 {
		t.Fatalf("vectors: %v", err)
	}
	key := func(value []byte) *ecdh.PrivateKey {
		k, err := ecdh.X25519().NewPrivateKey(value)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	identity := func(seed, box []byte) *NodeIdentity {
		if len(seed) == 0 {
			return nil
		}
		sign := ed25519.NewKeyFromSeed(seed)
		return &NodeIdentity{SignPublic: sign.Public().(ed25519.PublicKey), SignPrivate: sign, BoxPublic: key(box).PublicKey().Bytes(), BoxPrivate: box}
	}
	for _, tc := range vectors.Cases {
		t.Run(string(tc.Purpose), func(t *testing.T) {
			dial, accept := identity(tc.DialSeed, tc.DialBox), identity(tc.AcceptSeed, tc.AcceptBox)
			dialEp, acceptEp := key(tc.DialEphemeral), key(tc.AcceptEphemeral)
			hello := channelHello(byte(slices.Index(channelPurposes[:], tc.Purpose)), tc.ID, accept.SignPublic, dialEp, dial)
			ready := channelReady(hello, acceptEp, *accept, dial != nil)
			if !bytes.Equal(hello, tc.Hello) || !bytes.Equal(ready, tc.Ready) {
				t.Fatal("handshake differs from vector")
			}
			var dialBox, acceptBox *ecdh.PrivateKey
			var peerBox []byte
			if dial != nil {
				dialBox = key(tc.DialBox)
				acceptBox = key(tc.AcceptBox)
				peerBox = dial.BoxPublic
			}
			a, b := testTransportPair()
			defer a.Close()
			first, err := deriveSecureChannel(a, tc.ID, dialEp, acceptEp.PublicKey().Bytes(), dialBox, accept.BoxPublic, hello, ready, true)
			if err != nil {
				t.Fatal(err)
			}
			second, err := deriveSecureChannel(b, tc.ID, acceptEp, dialEp.PublicKey().Bytes(), acceptBox, peerBox, hello, ready, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, direction := range []struct {
				channel          SecureChannel
				frames           chan []byte
				plaintext, frame []byte
			}{
				{first, b.in, tc.DialPlaintext, tc.DialFrame}, {second, a.in, tc.AcceptPlaintext, tc.AcceptFrame},
			} {
				if err := direction.channel.WriteMessage(t.Context(), direction.plaintext); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(<-direction.frames, direction.frame) {
					t.Fatal("first encrypted frame differs from vector")
				}
			}
		})
	}
}
