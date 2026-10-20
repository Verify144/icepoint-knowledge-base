// Package merge 把方块集合压成尽量少的放置命令。
//
// 起点是旧导入器的做法：只在同一 (y,z) 行内沿 X 轴把连续同种方块合成 fill —— 一维。
// 本包在此之上再做两级：X-run → slab（沿 Z 展宽）→ cuboid（沿 Y 展宽）。
//
// 目标是减少命令条数 N。N 是服务端串行执行成本，任何调度层都消不掉它。
//
// 采用与坐标轴对齐的贪心，不追求理论最优：墙体、地板、地基这类实心结构本身高度规则，
// 贪心已能压到接近最优，而一般情形下的最优盒体覆盖是 NP 难问题。
//
// 关键不变式：每一级合并只吸收「其余两个轴上范围完全一致」的邻居，
// 因此合并出的盒体一定被输入方块填满，绝不会覆盖到空处。
package merge

import "sort"

// Block 是一个方块标识。相同值才可合并。
//
// States 是**已编码好**的后缀串，形如 "[wall_type=flat]"，无状态时为""。
// 本包只把它当作参与相等比较的不透明片段，拼接格式由调用方决定。
type Block struct {
	Name   string
	States string
}

type Pos struct{ X, Y, Z int32 }

// P 构造一个坐标。跨包使用时走它，避免无字段名的结构体字面量（go vet 会告警）。
func P(x, y, z int32) Pos { return Pos{X: x, Y: y, Z: z} }

// Op 是一条放置命令。Min==Max 时等价于单块 setblock。
type Op struct {
	Min, Max Pos
	Block    Block
}

func (o Op) Count() int64 {
	return int64(o.Max.X-o.Min.X+1) * int64(o.Max.Y-o.Min.Y+1) * int64(o.Max.Z-o.Min.Z+1)
}

// box 是合并过程的中间态：一个闭区间长方体。
type box struct {
	b                      Block
	x1, x2, y1, y2, z1, z2 int32
}

// Boxes 接收待放置的方块集合，返回命令数尽量少的操作序列。
// pos 与 blocks 必须等长；同一坐标重复出现时以最后一次为准。
func Boxes(pos []Pos, blocks []Block) []Op {
	if len(pos) != len(blocks) {
		panic("merge: pos 与 blocks 长度不一致")
	}

	uniq := make(map[Pos]Block, len(pos))
	order := make([]Pos, 0, len(pos))
	for i, p := range pos {
		if _, seen := uniq[p]; !seen {
			order = append(order, p)
		}
		uniq[p] = blocks[i]
	}

	// 第 1 级：同 (block, y, z) 聚簇，簇内按 x 排序，连续 x 合成 run。
	type rowKey struct {
		b    Block
		y, z int32
	}
	rows := map[rowKey][]int32{}
	for _, p := range order {
		k := rowKey{uniq[p], p.Y, p.Z}
		rows[k] = append(rows[k], p.X)
	}

	runs := make([]box, 0, len(order))
	for k, xs := range rows {
		sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
		for i := 0; i < len(xs); {
			j := i
			for j+1 < len(xs) && xs[j+1] == xs[j]+1 {
				j++
			}
			runs = append(runs, box{
				b: k.b, x1: xs[i], x2: xs[j],
				y1: k.y, y2: k.y, z1: k.z, z2: k.z,
			})
			i = j + 1
		}
	}

	slabs := mergeZ(runs)
	cuboids := mergeY(slabs)

	res := make([]Op, 0, len(cuboids))
	for _, bx := range cuboids {
		res = append(res, Op{
			Min:   Pos{bx.x1, bx.y1, bx.z1},
			Max:   Pos{bx.x2, bx.y2, bx.z2},
			Block: bx.b,
		})
	}
	// 输出顺序稳定，便于测试与 diff。
	sort.Slice(res, func(i, j int) bool {
		a, c := res[i].Min, res[j].Min
		if a.Y != c.Y {
			return a.Y < c.Y
		}
		if a.Z != c.Z {
			return a.Z < c.Z
		}
		if a.X != c.X {
			return a.X < c.X
		}
		return res[i].Block.Name < res[j].Block.Name
	})
	return res
}

// zKey 是沿 Z 合并时可互为邻居的判据：方块与 x/y 范围必须完全一致。
type zKey struct {
	b      Block
	x1, x2 int32
	y      int32
}

func mergeZ(in []box) []box {
	groups := map[zKey][]int{}
	for i := range in {
		k := zKey{in[i].b, in[i].x1, in[i].x2, in[i].y1}
		groups[k] = append(groups[k], i)
	}

	used := make([]bool, len(in))
	out := make([]box, 0, len(in))
	for _, idx := range groups {
		sort.Slice(idx, func(a, b int) bool { return in[idx[a]].z1 < in[idx[b]].z1 })
		for _, i := range idx {
			if used[i] {
				continue
			}
			cur := in[i]
			used[i] = true
			// 吃掉所有 z1 == cur.z2+1 的邻居；被吃者自身厚度可能 >1，需按序推进。
			for changed := true; changed; {
				changed = false
				for _, j := range idx {
					if used[j] || in[j].z1 != cur.z2+1 {
						continue
					}
					if in[j].z2 > cur.z2 {
						cur.z2 = in[j].z2
					}
					used[j] = true
					changed = true
				}
			}
			out = append(out, cur)
		}
	}
	return out
}

// yKey 是沿 Y 合并时可互为邻居的判据：方块与 x/z 范围必须完全一致。
type yKey struct {
	b      Block
	x1, x2 int32
	z1, z2 int32
}

func mergeY(in []box) []box {
	groups := map[yKey][]int{}
	for i := range in {
		k := yKey{in[i].b, in[i].x1, in[i].x2, in[i].z1, in[i].z2}
		groups[k] = append(groups[k], i)
	}

	used := make([]bool, len(in))
	out := make([]box, 0, len(in))
	for _, idx := range groups {
		sort.Slice(idx, func(a, b int) bool { return in[idx[a]].y1 < in[idx[b]].y1 })
		for _, i := range idx {
			if used[i] {
				continue
			}
			cur := in[i]
			used[i] = true
			for changed := true; changed; {
				changed = false
				for _, j := range idx {
					if used[j] || in[j].y1 != cur.y2+1 {
						continue
					}
					if in[j].y2 > cur.y2 {
						cur.y2 = in[j].y2
					}
					used[j] = true
					changed = true
				}
			}
			out = append(out, cur)
		}
	}
	return out
}

