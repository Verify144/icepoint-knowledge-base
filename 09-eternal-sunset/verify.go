// Package verify 把"服务器上的实际方块"与"本地期望方块"做逐格差分。
//
// 成本结构是这一层的关键：回拉 SubChunk 的**网络开销是 O(子区块)**，一个子区块覆盖
// 16×16×16=4096 格，所以一整栋大建筑只需几百次回拉请求；而比对是逐格精确进行的，
// CPU 开销是 O(方块)。旧引擎按"区块"回拉并按需逐格补验，网络与 CPU 两项都没摊薄。
//
// 只检查建筑本身涉及的格子：期望索引里没有的位置（例如镂空内部、周边地形）属于
// "本来就不该由我们决定"，一律跳过 —— 把它们当空气去校验会制造海量误报。
package verify

import (
	"context"
	"fmt"
	"sort"

	"github.com/Verify144/eternal-sunset/blockstate"
	"github.com/Verify144/eternal-sunset/emit"
	"github.com/Verify144/eternal-sunset/merge"
	"github.com/Verify144/eternal-sunset/protocol"
)

// Sub 是一个子区块坐标（以 16 格为单位，允许负数）。
type Sub struct{ X, Y, Z int32 }

// SubOf 把方块坐标换算到子区块坐标。向下取整，负坐标不能靠 >>4 处理。
func SubOf(p merge.Pos) Sub {
	return Sub{X: floorDiv(int64(p.X), 16), Y: floorDiv(int64(p.Y), 16), Z: floorDiv(int64(p.Z), 16)}
}

func floorDiv(a, b int64) int32 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return int32(q)
}

func floorMod(a, b int64) int32 { return int32(((a % b) + b) % b) }

// Expected 是可查询的期望方块索引：给定世界坐标，返回应当是什么方块。
// 内部按子区块分桶，避免为一次校验构建整栋建筑的逐格大表。
type Expected struct {
	origin [3]int32
	bySub  map[Sub][]merge.Op
	subs   []Sub
}

// NewExpected 建立索引。ops 是 merge 产出的相对坐标盒体，origin 是落点。
func NewExpected(ops []merge.Op, origin [3]int32) *Expected {
	e := &Expected{origin: origin, bySub: map[Sub][]merge.Op{}}
	seen := map[Sub]bool{}
	for _, op := range ops {
		lo := shift(op.Min, origin)
		hi := shift(op.Max, origin)
		s0, s1 := SubOf(lo), SubOf(hi)
		for sy := s0.Y; sy <= s1.Y; sy++ {
			for sz := s0.Z; sz <= s1.Z; sz++ {
				for sx := s0.X; sx <= s1.X; sx++ {
					s := Sub{sx, sy, sz}
					e.bySub[s] = append(e.bySub[s], op)
					if !seen[s] {
						seen[s] = true
						e.subs = append(e.subs, s)
					}
				}
			}
		}
	}
	sort.Slice(e.subs, func(i, j int) bool {
		a, b := e.subs[i], e.subs[j]
		if a.Y != b.Y {
			return a.Y < b.Y
		}
		if a.Z != b.Z {
			return a.Z < b.Z
		}
		return a.X < b.X
	})
	return e
}

// Subs 返回需要回拉的子区块列表 —— 决定网络请求数。
func (e *Expected) Subs() []Sub { return e.subs }

func shift(p merge.Pos, o [3]int32) merge.Pos {
	return merge.P(p.X+o[0], p.Y+o[1], p.Z+o[2])
}

// At 返回某世界坐标应有的方块。ok=false 表示该位置不由本次导入决定，应跳过。
// 若多个盒体覆盖同一格（不应发生，merge 保证互斥），取最后一个。
func (e *Expected) At(p merge.Pos) (merge.Block, bool) {
	var found bool
	var out merge.Block
	for _, op := range e.bySub[SubOf(p)] {
		lo := shift(op.Min, e.origin)
		hi := shift(op.Max, e.origin)
		if p.X >= lo.X && p.X <= hi.X && p.Y >= lo.Y && p.Y <= hi.Y && p.Z >= lo.Z && p.Z <= hi.Z {
			out, found = op.Block, true
		}
	}
	return out, found
}

// Snapshotter 提供某个子区块的实际内容。真实实现走协议回包，测试用合成实现。
type Snapshotter interface {
	SubChunk(ctx context.Context, s Sub) (*protocol.SubChunkData, error)
}

type Mismatch struct {
	At   merge.Pos
	Want merge.Block
	Got  merge.Block
	// ReadFailed 表示该子区块根本没读到，整块都算不确定。
	ReadFailed bool
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%v: want %s%s, got %s%s", m.At, m.Want.Name, m.Want.States, m.Got.Name, m.Got.States)
}

// Stats 是一次差分的工作量与结果。
type Stats struct {
	SubChunks      int // 回拉请求数 —— 网络开销
	CellsChecked   int // 逐格比对数 —— CPU 开销
	Mismatches     int
	ReadFailures   int
	UnreadableCell int // BlockAt 读不出的格子数（解码不支持该存储格式等）
}

// Diff 逐格比对给定子区块集合。subs 为 nil 时取 Expected 的全部子区块。
//
// 单个子区块读失败不会中止整场校验：记为 ReadFailures 并继续，由上层决定重试范围 ——
// 一次抖动不该让已完成的一整栋建筑重来。
func Diff(ctx context.Context, e *Expected, snap Snapshotter, subs []Sub) ([]Mismatch, Stats, error) {
	if subs == nil {
		subs = e.subs
	}
	var (
		mis   []Mismatch
		st    Stats
		canon = func(b merge.Block) string { return b.Name + b.States }
	)
	st.SubChunks = len(subs)

	for _, s := range subs {
		if err := ctx.Err(); err != nil {
			return mis, st, err
		}
		data, err := snap.SubChunk(ctx, s)
		if err != nil {
			st.ReadFailures++
			mis = append(mis, Mismatch{At: merge.P(s.X*16, s.Y*16, s.Z*16), ReadFailed: true,
				Want: merge.Block{Name: "<整个子区块>"}})
			continue
		}
		base := merge.P(s.X*16, s.Y*16, s.Z*16)
		for ly := int32(0); ly < 16; ly++ {
			for lz := int32(0); lz < 16; lz++ {
				for lx := int32(0); lx < 16; lx++ {
					p := merge.P(base.X+lx, base.Y+ly, base.Z+lz)
					want, ok := e.At(p)
					if !ok {
						continue // 不由本次导入决定
					}
					st.CellsChecked++
					got, ok := data.BlockAt(int(lx), int(ly), int(lz))
					if !ok {
						st.UnreadableCell++
						continue
					}
					actual := merge.Block{Name: got.Name, States: blockstate.Encode(got.States)}
					if canon(actual) != canon(want) {
						mis = append(mis, Mismatch{At: p, Want: want, Got: actual})
					}
				}
			}
		}
	}
	st.Mismatches = len(mis)
	return mis, st, nil
}

// RepairCommands 把不一致的格子重新走一遍合并与文本化。
//
// 复用 merge 是有意的：相邻的错位格子通常成批出现（一整段 fill 没落地），
// 重新按盒体合并能把修复命令数压回远低于逐格 setblock 的水平。
func RepairCommands(mis []Mismatch, origin [3]int32, lim emit.Limits) ([]emit.Command, int) {
	var pos []merge.Pos
	var bl []merge.Block
	skipped := 0
	for _, m := range mis {
		if m.ReadFailed {
			skipped++ // 整块没读到，不能凭猜测重铺，交上层决定重试点
			continue
		}
		rel := merge.P(m.At.X-origin[0], m.At.Y-origin[1], m.At.Z-origin[2])
		pos = append(pos, rel)
		bl = append(bl, m.Want)
	}
	if len(pos) == 0 {
		return nil, skipped
	}
	ops := merge.Boxes(pos, bl)
	return emit.Commands(ops, origin, lim), skipped
}

