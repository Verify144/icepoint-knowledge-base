// Package emit 把 merge 产出的盒体编成基岩版放置命令文本。
//
// 两条来自服务端真实约束的规则：
//
//  1. fill 有体积上限，超限会被服务端直接拒绝。因此大盒体沿最长轴递归切块。
//     上限值见 DefaultFillMaxVolume —— 它**尚未对本项目目标服务器实测确认**，
//     先按常见取值实现并可经 Limits 覆盖；切块逻辑本身不依赖具体数值。
//  2. fill 用默认 replace 模式，会清空区域内原有方块及其掉落物。带 NBT 的方块
//     （箱子、告示牌、命令方块）必须等所有 fill 落完之后再放，否则会被后续
//     fill 抹掉。这个先后顺序由调用方保证。
package emit

import (
	"fmt"

	"github.com/Verify144/eternal-sunset/merge"
)

// DefaultFillMaxVolume 是基岩版 fill 的常见体积上限。未经目标服务器实测，见包注释。
const DefaultFillMaxVolume int64 = 32768

// Limits 是命令文本化阶段要遵守的服务端约束。
type Limits struct {
	// FillMaxVolume 是单条 fill 允许覆盖的最大方块数。<=0 表示取默认值。
	FillMaxVolume int64
}

func (l Limits) fillMax() int64 {
	if l.FillMaxVolume > 0 {
		return l.FillMaxVolume
	}
	return DefaultFillMaxVolume
}

type Command struct {
	Text string
	// Blocks 是这条命令覆盖的方块数，用于统计与守恒校验。
	Blocks int64
	// Fill 为 true 表示 fill 命令，否则是单格 setblock。
	Fill bool
}

// corner 是内部用的坐标表示，用数组以便按轴索引。
type corner [3]int32

func toCorner(p merge.Pos) corner { return corner{p.X, p.Y, p.Z} }

func volume(lo, hi corner) int64 {
	return int64(hi[0]-lo[0]+1) * int64(hi[1]-lo[1]+1) * int64(hi[2]-lo[2]+1)
}

func longestAxis(lo, hi corner) int {
	d0 := int64(hi[0] - lo[0] + 1)
	d1 := int64(hi[1] - lo[1] + 1)
	d2 := int64(hi[2] - lo[2] + 1)
	switch {
	case d0 >= d1 && d0 >= d2:
		return 0
	case d1 >= d2:
		return 1
	default:
		return 2
	}
}

// Commands 把盒体列表编成命令序列。origin 是建筑落点，盒体坐标视为相对落点。
func Commands(ops []merge.Op, origin [3]int32, lim Limits) []Command {
	max := lim.fillMax()
	out := make([]Command, 0, len(ops))
	for _, op := range ops {
		lo := corner{op.Min.X + origin[0], op.Min.Y + origin[1], op.Min.Z + origin[2]}
		hi := corner{op.Max.X + origin[0], op.Max.Y + origin[1], op.Max.Z + origin[2]}
		for _, seg := range split(lo, hi, max) {
			out = append(out, render(seg.lo, seg.hi, op.Block))
		}
	}
	return out
}

type segment struct{ lo, hi corner }

// split 把体积超过 max 的长方体切成若干段，保证每段都不超上限。
//
// 采用沿最长轴二分递归：单刀切分在「截面积本身已超上限」时会失效（切到每片
// 只剩一格厚仍然超限），必须递归下去。二分的体积每轮严格下降，因此必然终止；
// 段数上界约 2·⌈v/max⌉，对 fill 上限这种量级完全够用。
//
// 只沿单一维度切，所以每段仍是实心填满的长方体，不会引入空格或重复覆盖。
func split(lo, hi corner, max int64) []segment {
	if volume(lo, hi) <= max {
		return []segment{{lo, hi}}
	}
	axis := longestAxis(lo, hi)
	if hi[axis] == lo[axis] {
		// 所有轴都只剩一格，体积即 1；只要 max>=1 就不会走到这里。
		// max<1 属非法配置，退化为单格一段而不陷入无限递归。
		return []segment{{lo, hi}}
	}
	mid := lo[axis] + (hi[axis]-lo[axis])/2
	left := hi
	left[axis] = mid
	right := lo
	right[axis] = mid + 1
	out := split(lo, left, max)
	return append(out, split(right, hi, max)...)
}

// render 生成单条命令文本。Block.States 是已编码好的后缀（形如 "[a=b,c=d]" 或空串）。
func render(lo, hi corner, b merge.Block) Command {
	name := b.Name + b.States
	if lo == hi {
		return Command{
			Text:   fmt.Sprintf("setblock %d %d %d %s", lo[0], lo[1], lo[2], name),
			Blocks: 1,
		}
	}
	return Command{
		Text: fmt.Sprintf("fill %d %d %d %d %d %d %s",
			lo[0], lo[1], lo[2], hi[0], hi[1], hi[2], name),
		Blocks: volume(lo, hi),
		Fill:   true,
	}
}

// Summarize 汇总命令数、覆盖方块数与 fill 条数，供度量与断言使用。
func Summarize(cs []Command) (cmds, blocks, fills int64) {
	for _, c := range cs {
		cmds++
		blocks += c.Blocks
		if c.Fill {
			fills++
		}
	}
	return
}

// Texts 只取命令文本，供直接提交给冰川。
func Texts(cs []Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}

