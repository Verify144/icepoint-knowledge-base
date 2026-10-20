// Package bridge 把 g79 客户端连接接到冰川：
// 命令从引擎出队后经 SendPacket 发出，回执由**恰好一个** reader goroutine 注入 Ack。
//
// 之所以强调单实例：g79 的 Connection.ReadPacket 内部持有读缓冲与 pending 队列，
// 并发调用会互相吞包。
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	glacier "github.com/Verify144/glacier"
	g79packet "github.com/Verify144/icepointg79/packet"

	"github.com/Verify144/eternal-sunset/protocol"
)

// PacketConn 是 g79 *Connection 的形状。抽象出来是为了让适配层能用假连接完整单测，
// 不必连真实租赁服。
type PacketConn interface {
	SendPacket(g79packet.Packet) error
	ReadPacket() (g79packet.Packet, error)
}

// decoders 把 g79 认不出的玩法包 ID 转成自有类型。
//
// 0xAE 已在真机校准（发 0xAF 会收到它，且偏移字段 echo 请求内容）；
// 0x4F 仍是从旧导入器沿用的推测值 —— 真机上至今没有服务端回过命令回执。
var decoders = map[uint32]func() g79packet.Packet{
	protocol.IDCommandOutput:    func() g79packet.Packet { return &protocol.CommandOutput{} },
	protocol.IDSubChunkResponse: func() g79packet.Packet { return &protocol.SubChunkResponse{} },
	// UpdateAbilities 带 CommandPermissions —— 导入前先读它，可以在发第一条命令之前
	// 就知道这个账号在这个服上到底有没有命令权限，而不是发完再靠"没有任何回执"去猜。
	protocol.IDUpdateAbilities: func() g79packet.Packet { return &protocol.UpdateAbilities{} },
}

// Bridge 是冰川与 g79 连接之间的适配层。方法均可并发调用（ReadLoop 除外，只能跑一个）。
type Bridge struct {
	conn   PacketConn
	eng    *glacier.Engine
	prefix string
	origin int64
	suc    successFunc

	chunks       *chunks
	dimension    int32
	chunkTimeout time.Duration

	seq       atomic.Int64
	undecoded atomic.Int64 // 收到但无解码器的包数
	malformed atomic.Int64 // 有解码器但解码失败的包数

	// g79 的 Connection.SendPacket 是阻塞写且不接受 context。这里用固定 worker 池
	// 包住它：worker 数恒定，因此连接卡死最多占用 SendWorkers 个 goroutine，
	// 而冰川那边只需尊重 ctx 的快路径调用，不必为每条命令再付一次 goroutine 创建。
	jobs     chan sendJob
	quit     chan struct{}
	quitOnce sync.Once
}

type sendJob struct {
	pkt  g79packet.Packet
	done chan error
}

// New 创建桥并启动冰川引擎。调用方需自行运行 ReadLoop，并在结束时 Close。
func New(conn PacketConn, gcfg glacier.Config, cfg Config) *Bridge {
	if cfg.RequestIDPrefix == "" {
		cfg.RequestIDPrefix = "sunset"
	}
	if cfg.Origin == 0 {
		cfg.Origin = protocol.CommandOriginAutomationPlayer
	}
	if cfg.Success == nil {
		cfg.Success = isSuccess
	}
	if cfg.ChunkTimeout <= 0 {
		cfg.ChunkTimeout = 5 * time.Second
	}
	workers := cfg.SendWorkers
	if workers <= 0 {
		workers = 4
	}
	b := &Bridge{
		conn: conn, prefix: cfg.RequestIDPrefix, origin: cfg.Origin, suc: cfg.Success,
		jobs:         make(chan sendJob, workers*2),
		quit:         make(chan struct{}),
		chunks:       newChunks(),
		dimension:    cfg.Dimension,
		chunkTimeout: cfg.ChunkTimeout,
	}
	for i := 0; i < workers; i++ {
		go b.writeWorker()
	}
	b.eng = glacier.New(glacier.SenderFunc(b.send), gcfg)
	return b
}

func (b *Bridge) writeWorker() {
	for {
		select {
		case j := <-b.jobs:
			j.done <- b.conn.SendPacket(j.pkt)
		case <-b.quit:
			return
		}
	}
}

// send 实现 glacier.Transport。Key 与 RequestID 必须一致，否则回执配不上对。
//
// 尊重 ctx：ctx 到期就返回错误，即使底层写还卡在 worker 里。这样冰川能继续推进，
// 而卡住的写被固定数量的 worker 兜住。
func (b *Bridge) send(ctx context.Context, cmd glacier.Command) error {
	done := make(chan error, 1)
	job := sendJob{
		done: done,
		pkt: &protocol.CommandRequest{
			Command:   string(cmd.Payload),
			Origin:    b.origin,
			RequestID: cmd.Key,
		},
	}
	select {
	case b.jobs <- job:
	case <-ctx.Done():
		return fmt.Errorf("%w: 发送队列已满", ctx.Err())
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: 等待发包写出", ctx.Err())
	}
}

// Config 是适配层自身的参数。引擎参数直接传给冰川。
type Config struct {
	// RequestIDPrefix 用于生成 RequestID；同一连接内必须唯一。
	RequestIDPrefix string
	// Origin 是命令发起者类型，默认 AutomationPlayer。
	Origin int64
	// Success 判定一条回执是否成功。默认规则见 isSuccess。
	Success func(*protocol.CommandOutput) bool
	// SendWorkers 是包住阻塞写的固定 goroutine 数，默认 4。
	// 连接卡死时最多占用这么多个 goroutine，不会随命令数增长。
	SendWorkers int

	// Dimension 是回读子区块用的维度，默认 0（主世界）。
	Dimension int32
	// ChunkTimeout 是单个子区块回读的等待上限，默认 5s。
	ChunkTimeout time.Duration
}

type successFunc = func(*protocol.CommandOutput) bool

func (b *Bridge) Engine() *glacier.Engine { return b.eng }

// Command 生成一条带唯一 RequestID 的命令，ID 同时作为冰川的配对 Key。
func (b *Bridge) Command(text string) glacier.Command {
	n := b.seq.Add(1)
	return glacier.Command{
		Key:     fmt.Sprintf("%s-%d", b.prefix, n),
		Payload: []byte(text),
	}
}

// Send 实现 plan.Runner。
func (b *Bridge) Send(ctx context.Context, texts ...string) error {
	cmds := make([]glacier.Command, 0, len(texts))
	for _, t := range texts {
		cmds = append(cmds, b.Command(t))
	}
	return b.eng.Submit(ctx, cmds...)
}

// ReadLoop 独占连接读取，把 CommandOutput 回执注入引擎。
// 必须只运行一个实例；返回意味着连接已断或 ctx 结束。
func (b *Bridge) ReadLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		p, err := b.conn.ReadPacket()
		if err != nil {
			return err
		}
		if u, ok := p.(*g79packet.UnknownPacket); ok {
			dec, known := decoders[u.PacketID]
			if !known {
				b.undecoded.Add(1)
				continue
			}
			np := dec()
			if err := np.Decode(bytes.NewReader(u.Raw)); err != nil {
				b.malformed.Add(1)
				continue
			}
			p = np
		}
		if out, ok := p.(*protocol.CommandOutput); ok {
			b.eng.Ack(out.RequestID, b.suc(out))
		}
		if sc, ok := p.(*protocol.SubChunkResponse); ok {
			b.chunks.index(sc)
		}
	}
}

// Stats 汇总适配层自身看到的计数。
type Stats struct {
	UndecodedPackets int64
	MalformedPackets int64
	CommandsIssued   int64
	Engine           glacier.Stats
}

func (b *Bridge) Stats() Stats {
	return Stats{
		UndecodedPackets: b.undecoded.Load(),
		MalformedPackets: b.malformed.Load(),
		CommandsIssued:   b.seq.Load(),
		Engine:           b.eng.Snapshot(),
	}
}

// Settle 等待全部在途命令落定，并在期间有命令被彻底放弃时返回错误。
//
// 引擎的 Wait 只表示"都进入终态"，不区分成功与被丢弃；阶段驱动需要后者，
// 否则一批 fill 全被服务端拒绝也会被判成阶段完成而继续往下摆 NBT。
func (b *Bridge) Settle(ctx context.Context) error {
	before := b.eng.Snapshot().Dropped
	if err := b.eng.Wait(ctx); err != nil {
		return err
	}
	s := b.eng.Snapshot()
	if s.Dropped > before {
		return fmt.Errorf("%w: %d 条命令被放弃（累计 in-flight=%d）", ErrCommandsLost, s.Dropped-before, s.InFlight)
	}
	return nil
}

// ErrCommandsLost 表示有命令在重试耗尽后被彻底放弃。
var ErrCommandsLost = errors.New("bridge: 有命令被彻底放弃")

// Close 停止引擎与空闲 worker。不会关闭底层连接。
//
// 已经卡在 SendPacket 里的 worker 无法被唤醒（该写不接受 context），它会留到连接
// 自己崩掉为止 —— 数量上限就是 SendWorkers。
func (b *Bridge) Close() error {
	b.quitOnce.Do(func() { close(b.quit) })
	return b.eng.Close()
}

// isSuccess 判定命令是否真正执行成功。
//
// 这里刻意不沿用旧导入器的 `SuccessCount > 0`：SuccessCount 语义是「受影响的输出条目数」，
// 部分成功命令会返回 0，而失败也可能带消息。末条消息的 Success 位才是执行结果信号。
//
// 但这一判读本身也来自字段布局推测，需真服校准后再定论。
func isSuccess(out *protocol.CommandOutput) bool {
	if n := len(out.Messages); n > 0 {
		return out.Messages[n-1].Success
	}
	return out.SuccessCount > 0
}

