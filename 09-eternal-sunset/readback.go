// Package readback 把"从服务器回读子区块"接成 verify 需要的 Snapshotter。
//
// 这一层存在的原因是 verify.Diff 的接口此前只有测试里的合成实现：差分逻辑验过，
// 但它从没对着真实回包跑过。这里补的是那条真链路 —— 桥接回读、注入方块表、
// 把坐标单位对齐（verify.Sub 与 protocol.SubChunkPos 都以 16 格为单位）。
package readback

import (
	"context"
	"fmt"

	"github.com/Verify144/eternal-sunset/blockid"
	"github.com/Verify144/eternal-sunset/protocol"
	"github.com/Verify144/eternal-sunset/verify"
)

// Fetcher 是 bridge.Bridge 的子区块回读能力。抽成接口是为了能让测试喂假数据。
type Fetcher interface {
	Fetch(ctx context.Context, pos []protocol.SubChunkPos) (
		map[protocol.SubChunkPos]*protocol.SubChunkData, map[protocol.SubChunkPos]error, error)
}

// Snapshot 按子区块坐标现取现解，并把 runtime ID 翻成方块。
type Snapshot struct {
	f   Fetcher
	tbl *blockid.Table
}

// New 建一个快照器。tbl 为 nil 时用内嵌的那张表。
func New(f Fetcher, tbl *blockid.Table) *Snapshot {
	if tbl == nil {
		tbl = blockid.MustTable()
	}
	return &Snapshot{f: f, tbl: tbl}
}

var _ verify.Snapshotter = (*Snapshot)(nil)

// SubChunk 取一个子区块。没取到时返回错误而不是空数据 —— 空数据会被差分逻辑
// 当成"读到空气"，把"读不到"误报成"方块不对"。
func (s *Snapshot) SubChunk(ctx context.Context, sub verify.Sub) (*protocol.SubChunkData, error) {
	p := protocol.SubChunkPos{X: sub.X, Y: sub.Y, Z: sub.Z}
	got, failed, err := s.f.Fetch(ctx, []protocol.SubChunkPos{p})
	if err != nil {
		return nil, err
	}
	d := got[p]
	if d == nil {
		if e, ok := failed[p]; ok {
			return nil, fmt.Errorf("readback: %v 没读到: %w", p, e)
		}
		return nil, fmt.Errorf("readback: %v 没读到", p)
	}
	tbl := s.tbl
	d.Blocks = func(id uint32) (protocol.Block, bool) {
		e, ok := tbl.Lookup(id)
		if !ok {
			return protocol.Block{}, false
		}
		return protocol.Block{Name: e.Name, States: e.States}, true
	}
	return d, nil
}

