// Package loadplan 规划"如何让建筑涉及的每个区块真正处于加载状态"。
//
// 为什么必须规划而不是随手加个 tickingarea：基岩版对 ticking 区域有两个硬上限
// （数值为公开文档行为，**尚未对本项目目标服务器实测**，故均可配置）：
//
//   - 单个 tickingarea 最多覆盖 MaxChunksPerArea 个区块（默认 12）
//   - 全服同时存在的自定义 tickingarea 数量最多 MaxAreas 个（默认 10）
//
// 旧导入器在这两点上都会失效：它先尝试用一个区域罩住整栋建筑（上限 256 区块，
// 远超 12，服务端会直接拒绝），失败后回退成逐区块添加**无名字**区域且发完不等；
// 一旦累计数量触到 10 的上限就静默失败，后面那些区块从未加载，
// fill 打上去不落地 —— 表现为"放置丢失"，且没有任何错误冒出来。
//
// 本包把这两个上限作为约束，输出若干"批次"（wave）：每批内的区域数量与各自跨度
// 都合规，批次之间的并集恰好覆盖所有需要的区块。区域一律带名字，便于换批时先移除。
package loadplan

import (
	"fmt"
	"sort"
)

// 默认上限，取自基岩版公开文档；未经目标服务器实测。
const (
	DefaultMaxChunksPerArea = 12
	DefaultMaxAreas         = 10
)

// Limits 是规划要遵守的服务端约束。
type Limits struct {
	MaxChunksPerArea int
	MaxAreas         int
}

func (l Limits) withDefaults() Limits {
	if l.MaxChunksPerArea <= 0 {
		l.MaxChunksPerArea = DefaultMaxChunksPerArea
	}
	if l.MaxAreas <= 0 {
		l.MaxAreas = DefaultMaxAreas
	}
	return l
}

type Chunk struct{ X, Z int32 }

// Area 是一块以区块坐标表示的矩形加载区域（闭区间）。
type Area struct {
	Name   string
	X1, Z1 int32
	X2, Z2 int32
}

func (a Area) Chunks() int {
	return int(a.X2-a.X1+1) * int(a.Z2-a.Z1+1)
}

func (a Area) Contains(c Chunk) bool {
	return c.X >= a.X1 && c.X <= a.X2 && c.Z >= a.Z1 && c.Z <= a.Z2
}

// Wave 是一批可同时存在的区域。
type Wave struct {
	Index  int
	Areas  []Area
	Chunks int // 本批覆盖的区块数
}

type Plan struct {
	Limits Limits
	Waves  []Wave
	Needed int // 需要加载的区块总数
}

// BlocksOf 把方块坐标换算到区块坐标。假设 y 不参与分块。
func BlocksOf(x, z int32) Chunk { return Chunk{X: x >> 4, Z: z >> 4} }

// Make 规划加载批次。重复区块会被去重。
func Make(chunks []Chunk, lim Limits) Plan {
	lim = lim.withDefaults()

	set := make(map[Chunk]bool, len(chunks))
	for _, c := range chunks {
		set[c] = true
	}
	needed := make([]Chunk, 0, len(set))
	for c := range set {
		needed = append(needed, c)
	}

	areas := packAreas(needed, lim.MaxChunksPerArea)
	waves := packWaves(areas, lim.MaxAreas)

	p := Plan{Limits: lim, Waves: waves, Needed: len(needed)}
	for i := range p.Waves {
		p.Waves[i].Index = i
		n := 0
		for _, a := range p.Waves[i].Areas {
			n += a.Chunks()
		}
		p.Waves[i].Chunks = n
	}
	return p
}

// packAreas 先把区块压成尽量少的矩形，且每个矩形不超过 per 个区块。
//
// 步骤与 merge 包同构：先在同一 z 行内把连续 x 合成 run，再把 x 跨度相同的 run
// 沿 z 接高，直到跨度乘积触顶。
func packAreas(needed []Chunk, per int) []Area {
	byRow := map[int32][]int32{}
	for _, c := range needed {
		byRow[c.Z] = append(byRow[c.Z], c.X)
	}
	zs := make([]int32, 0, len(byRow))
	for z := range byRow {
		zs = append(zs, z)
	}
	sort.Slice(zs, func(i, j int) bool { return zs[i] < zs[j] })
	for _, z := range zs {
		sort.Slice(byRow[z], func(i, j int) bool { return byRow[z][i] < byRow[z][j] })
	}

	// 每行的 x 连续段：z -> []run{x1,x2}。单段长度受 per 约束，
	// 且切分对齐到 per 的整数倍边界 —— 只有相邻行产生完全相同的 x 跨度，
	// 后面的"沿 z 接高"才合并得起来；否则实心大区域会退化成一条条 1 格高的窄区域。
	rowRuns := make(map[int32][]xrun, len(zs))
	for _, z := range zs {
		xs := byRow[z]
		for i := 0; i < len(xs); {
			j := i
			for j+1 < len(xs) && xs[j+1] == xs[j]+1 {
				j++
			}
			rowRuns[z] = append(rowRuns[z], splitRun(xs[i], xs[j], per)...)
			i = j + 1
		}
	}

	var out []Area
	used := make(map[int32]map[int]bool, len(zs)) // z -> run 下标 -> 是否已被占用
	for _, z := range zs {
		used[z] = map[int]bool{}
	}
	nameSeq := 0
	for _, z := range zs {
		for ri, r := range rowRuns[z] {
			if used[z][ri] {
				continue
			}
			used[z][ri] = true
			width := int(r.x2-r.x1) + 1
			x1, x2 := r.x1, r.x2
			depth := int32(1)
			for {
				nz := z + depth
				if width*int(depth+1) > per {
					break
				}
				idx := -1
				for k, cand := range rowRuns[nz] {
					if !used[nz][k] && cand.x1 == x1 && cand.x2 == x2 {
						idx = k
						break
					}
				}
				if idx < 0 {
					break
				}
				used[nz][idx] = true
				depth++
			}
			nameSeq++
			out = append(out, Area{
				Name: fmt.Sprintf("sunset_ta_%03d", nameSeq),
				X1:   x1, Z1: z, X2: x2, Z2: z + depth - 1,
			})
		}
	}
	return out
}

// xrun 是同一区块行内一段连续的 x 区间（闭区间）。
type xrun struct{ x1, x2 int32 }

// splitRun 把一段连续 x 切成不超过 per 的子段，切口对齐到 per 的整数倍。
// 对齐让相同形状的相邻行切出完全一致的子段，从而使沿 z 的接高合并能够成立。
func splitRun(x1, x2 int32, per int) []xrun {
	if per <= 0 {
		return []xrun{{x1, x2}}
	}
	var out []xrun
	start := x1
	for {
		// 从 start 起，最多还能延伸到 per 格，但不能跨过下一个对齐边界
		limit := start + int32(per) - 1
		next := int32(floorDiv(int64(start), int64(per))+1) * int32(per)
		if next <= x2 && next-1 < limit {
			limit = next - 1
		}
		if limit > x2 {
			limit = x2
		}
		out = append(out, xrun{start, limit})
		if limit == x2 {
			return out
		}
		start = limit + 1
	}
}

// floorDiv 是数学向下取整除法。Go 的 / 对负数向零截断，会把对齐边界算错。
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// packWaves 把矩形装进批次，每批不超过 maxAreas 个区域。
// 先按覆盖区块数降序（首适降序），批数更接近下界。
func packWaves(areas []Area, maxAreas int) []Wave {
	sorted := make([]Area, len(areas))
	copy(sorted, areas)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Chunks() != b.Chunks() {
			return a.Chunks() > b.Chunks()
		}
		return a.Name < b.Name
	})

	nWaves := (len(sorted) + maxAreas - 1) / maxAreas
	waves := make([]Wave, nWaves)
	// 首轮各批取最小覆盖：按降序轮流放入当前区块数最少的批，避免尾批过大
	for _, a := range sorted {
		best := 0
		for i := range waves {
			if len(waves[i].Areas) >= maxAreas {
				continue
			}
			if len(waves[best].Areas) == 0 || len(waves[i].Areas) < len(waves[best].Areas) {
				best = i
			}
		}
		waves[best].Areas = append(waves[best].Areas, a)
	}
	out := waves[:0]
	for _, w := range waves {
		if len(w.Areas) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// SetupCommands 生成启用本批所需的命令（只有 add）。
//
// 时序固定为：add → 打本批的 fill → Settle → Teardown。移除放在 Settle 之后而不是
// 加之前，是因为 tickingarea remove 对不存在的名字会报错；冰川的 Settle 会把"命令被
// 放弃"上报成错误，混进 remove 失败就会让整批导入无故中止。
func (w Wave) SetupCommands() []string {
	out := make([]string, 0, len(w.Areas))
	for _, a := range w.Areas {
		x1, z1 := a.X1<<4, a.Z1<<4
		x2, z2 := (a.X2<<4)+15, (a.Z2<<4)+15
		out = append(out, fmt.Sprintf("tickingarea add %d 0 %d %d 0 %d %s", x1, z1, x2, z2, a.Name))
	}
	return out
}

// TeardownCommands 生成本批的移除命令。应在本批 fill 落定之后调用。
func (w Wave) TeardownCommands() []string {
	out := make([]string, 0, len(w.Areas))
	for _, a := range w.Areas {
		out = append(out, fmt.Sprintf("tickingarea remove %s", a.Name))
	}
	return out
}

// ChunksIn 返回本批区域覆盖到的区块集合，供上层把 fill 命令按批分组。
// 落在任何批次之外的区块不会被返回 —— 那属于规划错误，Make 已断言覆盖完整。
func (w Wave) ChunksIn() map[Chunk]bool {
	m := map[Chunk]bool{}
	for _, a := range w.Areas {
		for z := a.Z1; z <= a.Z2; z++ {
			for x := a.X1; x <= a.X2; x++ {
				m[Chunk{x, z}] = true
			}
		}
	}
	return m
}

