package world

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/VaalaCat/openvaala/pkg/worldserver"
	"golang.org/x/crypto/hkdf"
)

// FrameTransport 按序传输完整帧，支持一个写方与一个读方并发使用。
type FrameTransport interface {
	// WriteFrame 投递一帧，返回后调用方可复用输入切片。
	WriteFrame(ctx context.Context, frame []byte) error
	// ReadFrame 读取下一帧，返回的切片归读方使用。
	ReadFrame(ctx context.Context) ([]byte, error)
	// Close 可并发重复调用，使阻塞及后续读写返回错误。
	Close() error
}

// SecureChannel 在已认证节点间收发消息；一个写方可与一个读方 并发，同方向调用由调用方串行化。
type SecureChannel interface {
	// WriteMessage 加密并发送一条消息。
	WriteMessage(ctx context.Context, payload []byte) error
	// ReadMessage 认证并解密下一条消息。
	ReadMessage(ctx context.Context) ([]byte, error)
	// Close 关闭通道，唤醒阻塞的收发操作。
	Close() error
}

type secureChannelImpl struct {
	transport     FrameTransport
	channelID     []byte
	sendCipher    cipher.AEAD
	recvCipher    cipher.AEAD
	sentNonce     uint64
	receivedNonce uint64
}

const (
	channelProtocol   = "openvaala-world-e2ee/v2"
	maxChannelMessage = worldserver.ChannelFrameLimit - 8 - 16
	helloHeaderSize   = 3 + 16 + ed25519.PublicKeySize + 32
)

// ChannelPurpose 指定端到端握手认证的业务用途。
type ChannelPurpose string

const (
	ChannelPurposeWorld  ChannelPurpose = "world"
	ChannelPurposeRPC    ChannelPurpose = "rpc"
	ChannelPurposeDomain ChannelPurpose = "domain"
)

var channelPurposes = [...]ChannelPurpose{"", ChannelPurposeWorld, ChannelPurposeRPC, ChannelPurposeDomain}

// AcceptedChannel 返回用途及经签名验证的发起方公钥，匿名 RPC 的公钥为空。
type AcceptedChannel struct {
	Channel SecureChannel
	Purpose ChannelPurpose
	PeerKey ed25519.PublicKey
}

// DialSecureChannel 以预期的响应方公钥验证握手，匿名发起方只能使用 RPC。
func DialSecureChannel(ctx context.Context, transport FrameTransport, local *NodeIdentity, peerKey ed25519.PublicKey, purpose ChannelPurpose) (_ SecureChannel, err error) {
	if transport == nil {
		return nil, errors.New("missing world frame transport")
	}
	defer func() {
		if err != nil {
			transport.Close()
		}
	}()
	code := slices.Index(channelPurposes[:], purpose)
	if code <= 0 || len(peerKey) != ed25519.PublicKeySize || local == nil && purpose != ChannelPurposeRPC {
		return nil, errors.New("invalid world channel purpose or identity")
	}
	id := make([]byte, 16)
	rand.Read(id)
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var static *ecdh.PrivateKey
	if local != nil {
		static, err = local.validateKeys()
		if err != nil {
			return nil, err
		}
	}
	hello := channelHello(byte(code), id, peerKey, ephemeral, local)
	if err = transport.WriteFrame(ctx, hello); err != nil {
		return nil, err
	}
	ready, err := transport.ReadFrame(ctx)
	if err != nil {
		return nil, err
	}
	publicSize := 32
	if local != nil {
		publicSize += 32
	}
	if len(ready) != publicSize+ed25519.SignatureSize || !ed25519.Verify(peerKey, channelTranscript(hello, ready[:publicSize]), ready[publicSize:]) {
		return nil, errors.New("invalid world ready signature")
	}
	return deriveSecureChannel(transport, id, ephemeral, ready[:32], static, ready[32:publicSize], hello, ready, true)
}

// AcceptSecureChannel 核对用途、目标和签名后返回应用通道。
func AcceptSecureChannel(ctx context.Context, transport FrameTransport, local NodeIdentity) (_ AcceptedChannel, err error) {
	if transport == nil {
		return AcceptedChannel{}, errors.New("missing world frame transport")
	}
	defer func() {
		if err != nil {
			transport.Close()
		}
	}()
	hello, err := transport.ReadFrame(ctx)
	if err != nil {
		return AcceptedChannel{}, err
	}
	if len(hello) < helloHeaderSize || hello[0] != 2 || hello[1] == 0 || int(hello[1]) >= len(channelPurposes) || !bytes.Equal(hello[19:51], local.SignPublic) {
		return AcceptedChannel{}, errors.New("invalid world hello header")
	}
	result := AcceptedChannel{Purpose: channelPurposes[hello[1]]}
	var static *ecdh.PrivateKey
	var peerBox []byte
	switch hello[2] {
	case 0:
		if len(hello) != helloHeaderSize || result.Purpose != ChannelPurposeRPC {
			return AcceptedChannel{}, errors.New("invalid anonymous world hello")
		}
	case 1:
		const signedSize = helloHeaderSize + 64
		if len(hello) != signedSize+ed25519.SignatureSize {
			return AcceptedChannel{}, errors.New("invalid signed world hello length")
		}
		result.PeerKey = ed25519.PublicKey(hello[helloHeaderSize : helloHeaderSize+32])
		if !ed25519.Verify(result.PeerKey, channelTranscript(hello[:signedSize]), hello[signedSize:]) {
			return AcceptedChannel{}, errors.New("invalid world hello signature")
		}
		peerBox = hello[helloHeaderSize+32 : signedSize]
	default:
		return AcceptedChannel{}, errors.New("invalid world hello identity flag")
	}
	box, err := local.validateKeys()
	if err != nil {
		return AcceptedChannel{}, err
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return AcceptedChannel{}, err
	}
	if result.PeerKey != nil {
		static = box
	}
	ready := channelReady(hello, ephemeral, local, static != nil)
	result.Channel, err = deriveSecureChannel(transport, hello[3:19], ephemeral, hello[51:helloHeaderSize], static, peerBox, hello, ready, false)
	if err != nil {
		return AcceptedChannel{}, err
	}
	if err = transport.WriteFrame(ctx, ready); err != nil {
		return AcceptedChannel{}, err
	}
	return result, nil
}

// channelHello 编码发起方声明，具名模式签署完整身份和用途。
func channelHello(code byte, id, peerKey []byte, ephemeral *ecdh.PrivateKey, local *NodeIdentity) []byte {
	hello := slices.Concat([]byte{2, code, 0}, id, peerKey, ephemeral.PublicKey().Bytes())
	if local == nil {
		return hello
	}
	hello[2] = 1
	hello = append(hello, local.SignPublic...)
	hello = append(hello, local.BoxPublic...)
	return append(hello, ed25519.Sign(local.SignPrivate, channelTranscript(hello))...)
}

// channelReady 将本端的密钥交换公钥签入发起方的完整握手记录。
func channelReady(hello []byte, ephemeral *ecdh.PrivateKey, local NodeIdentity, named bool) []byte {
	ready := ephemeral.PublicKey().Bytes()
	if named {
		ready = append(ready, local.BoxPublic...)
	}
	return append(ready, ed25519.Sign(local.SignPrivate, channelTranscript(hello, ready))...)
}

func channelTranscript(records ...[]byte) []byte {
	transcript := []byte(channelProtocol)
	for _, record := range records {
		transcript = append(transcript, record...)
	}
	return transcript
}

// deriveSecureChannel 将双方的握手记录绑定到两个方向独立的消息密钥。
func deriveSecureChannel(transport FrameTransport, id []byte, ephemeral *ecdh.PrivateKey, peerEphemeral []byte, static *ecdh.PrivateKey, peerBox, hello, ready []byte, dial bool) (SecureChannel, error) {
	shared, err := exchangeChannelKey(ephemeral, peerEphemeral)
	if err != nil {
		return nil, err
	}
	if static != nil {
		bound, err := exchangeChannelKey(static, peerBox)
		if err != nil {
			return nil, err
		}
		shared = append(shared, bound...)
	}
	var keys [64]byte
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, id, channelTranscript(hello, ready)), keys[:]); err != nil {
		return nil, err
	}
	var directions [2]cipher.AEAD
	for i := range directions {
		block, err := aes.NewCipher(keys[i*32 : (i+1)*32])
		if err != nil {
			return nil, err
		}
		directions[i], err = cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
	}
	send, recv := directions[0], directions[1]
	if !dial {
		send, recv = recv, send
	}
	return &secureChannelImpl{transport: transport, channelID: id, sendCipher: send, recvCipher: recv}, nil
}

func (c *secureChannelImpl) WriteMessage(ctx context.Context, payload []byte) error {
	// 1. 分配下一个序号，耗尽时关闭通道。
	if len(payload) > maxChannelMessage {
		return errors.New("world message exceeds 16 MiB")
	}
	if c.sentNonce == math.MaxUint64 {
		c.Close()
		return errors.New("world send nonce exhausted")
	}
	c.sentNonce++
	nonce := channelNonce(c.sentNonce)
	// 2. 以序号为帧头、通道标识为关联数据加密。
	frame := make([]byte, 8, 8+len(payload)+c.sendCipher.Overhead())
	copy(frame, nonce[4:])
	frame = c.sendCipher.Seal(frame, nonce[:], payload, c.channelID)
	if err := c.transport.WriteFrame(ctx, frame); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (c *secureChannelImpl) ReadMessage(ctx context.Context) (_ []byte, err error) {
	// 1. 读取下一帧；传输、序号及认证错误均终止通道。
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	frame, err := c.transport.ReadFrame(ctx)
	if err != nil {
		return nil, err
	}
	if len(frame) > worldserver.ChannelFrameLimit {
		return nil, errors.New("world frame exceeds limit")
	}
	if len(frame) < 8+c.recvCipher.Overhead() {
		return nil, errors.New("short world ciphertext frame")
	}
	sequence := binary.LittleEndian.Uint64(frame[:8])
	if sequence == 0 || sequence != c.receivedNonce+1 {
		return nil, fmt.Errorf("invalid world frame nonce %d after %d", sequence, c.receivedNonce)
	}
	// 2. 校验通道标识与密文，认证成功后记录序号。
	nonce := channelNonce(sequence)
	plaintext, err := c.recvCipher.Open(nil, nonce[:], frame[8:], c.channelID)
	if err != nil {
		return nil, fmt.Errorf("authenticate world frame: %w", err)
	}
	c.receivedNonce = sequence
	return plaintext, nil
}

func (c *secureChannelImpl) Close() error {
	// 1. 关闭底层传输并唤醒对端。
	return c.transport.Close()
}
