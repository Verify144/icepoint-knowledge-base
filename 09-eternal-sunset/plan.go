// Package plan 把一栋建筑拆成有序的执行阶段，并驱动它们按序完成。
//
// 需要"阶段"而不是把所有命令一股脑交给引擎，源于两条服务器事实：
//
//  1. fill 的 replace 模式会抹掉区域内已有的方块实体，所以带 NBT 的方块必须在
//     所有 fill 落完之后才摆。
//  2. NBT 搬运借用同一个暂存格，两条搬运交错就会互相污染。因此搬运不仅要在 fill
//     之后，组内还必须逐条串行。
//
// 这两条以前只能靠调用方自觉遵守；这里把它们做成驱动器无法绕过的结构。
package plan

import (
	"context"
	"fmt"

	"github.com/Verify144/eternal-sunset/emit"
	"github.com/Verify144/eternal-sunset/loadplan"
	"github.com/Verify144/eternal-sunset/merge"
)

// Runner 是驱动器需要的最小提交能力。*bridge.Bridge 结构性满足它（签名须一致），
// 测试用假实现。
type Runner interface {
	// Send 提交一批命令，返回即表示已入队（不代表已执行完）。
	Send(ctx context.Context, cmds ...string) error
	// Settle 阻塞直到所有已提交命令进入终态；期间有命令被放弃则返回错误。
	Settle(ctx context.Context) error
}

type Phase struct {
	Label string
	// Groups 是该阶段内的独立工作单元。
	// 非 Strict 阶段：所有 group 的命令一起提交，阶段末统一等待。
	// Strict 阶段：逐 group、组内逐条提交并等待，以保证跨命令的顺序依赖。
	Groups [][]string
	Strict bool
}

type Options struct {
	Origin     [3]int32
	Scratch    ScratchCell
	StructName string
	FillLimit  int64

	// MaxChunksPerArea / MaxAreas 是加载批次的服务端约束，<=0 时取 loadplan 默认值。
	MaxChunksPerArea int
	MaxAreas         int
}

// Block 是输入方块。Data 非空即视为需要搬运的方块实体。
type Block struct {
	At     merge.Pos
	Name   string
	States string
	Data   map[string]any
}

// Stats 描述拆分结果，供度量与断言。
type Stats struct {
	Blocks     int
	FillCmds   int
	FillBlocks int64
	NBTBlocks  int
	NBTCmds    int
	Waves      int
	LoadCmds   int
}

// Build 把方块列表拆成有序阶段。
//
// 顺序由三条服务器事实决定，任一条被打破都会静默出错而不是报错：
//
//  1. 未加载的区块收不到 fill —— 所以先按 loadplan 划出加载波，每波独立做盒体合并。
//     盒体因此绝不跨波；若允许跨波，一条 fill 会在部分区块尚未加载时打出，只落下一半。
//  2. fill 的 replace 模式会抹掉区域内已有的方块实体 —— 所以带 NBT 的方块必须在所有
//     fill 之后。
//  3. NBT 搬运共用同一暂存格 —— 所以搬运组内必须逐条串行。
func Build(in []Block, opt Options) ([]Phase, Stats) {
	var (
		nbt     []NBTBlock
		byChunk = map[loadplan.Chunk][]Block{}
		st      Stats
	)
	st.Blocks = len(in)
	for _, b := range in {
		if len(b.Data) > 0 {
			nbt = append(nbt, NBTBlock{At: b.At, Name: b.Name, States: b.States, Data: b.Data})
			continue
		}
		c := loadplan.BlocksOf(b.At.X+opt.Origin[0], b.At.Z+opt.Origin[2])
		byChunk[c] = append(byChunk[c], b)
	}
	st.NBTBlocks = len(nbt)

	chunks := make([]loadplan.Chunk, 0, len(byChunk))
	for c := range byChunk {
		chunks = append(chunks, c)
	}

	var phases []Phase
	if len(chunks) > 0 {
		lp := loadplan.Make(chunks, loadplan.Limits{
			MaxChunksPerArea: opt.MaxChunksPerArea, MaxAreas: opt.MaxAreas,
		})
		st.Waves = len(lp.Waves)
		for _, w := range lp.Waves {
			phases = append(phases, Phase{Label: fmt.Sprintf("load#%d", w.Index),
				Groups: [][]string{w.SetupCommands()}})
			st.LoadCmds += len(w.SetupCommands())

			var texts []string
			for c, bs := range byChunk {
				if !w.ChunksIn()[c] {
					continue
				}
				plainPos := make([]merge.Pos, 0, len(bs))
				plainBl := make([]merge.Block, 0, len(bs))
				for _, b := range bs {
					plainPos = append(plainPos, b.At)
					plainBl = append(plainBl, merge.Block{Name: b.Name, States: b.States})
				}
				ops := merge.Boxes(plainPos, plainBl)
				cmds := emit.Commands(ops, opt.originOrDefault(), emit.Limits{FillMaxVolume: opt.FillLimit})
				_, covered, _ := emit.Summarize(cmds)
				st.FillBlocks += covered
				texts = append(texts, emit.Texts(cmds)...)
			}
			st.FillCmds += len(texts)
			phases = append(phases, Phase{Label: fmt.Sprintf("fill#%d", w.Index),
				Groups: [][]string{texts}})

			// 移除放在 fill 落定之后：remove 不存在的名字会失败，而 Settle 会把失败
			// 上报成中止，放进前面那批里就会无故打断整场导入。
			td := w.TeardownCommands()
			phases = append(phases, Phase{Label: fmt.Sprintf("unload#%d", w.Index),
				Groups: [][]string{td}})
			st.LoadCmds += len(td)
		}
	}

	if len(nbt) > 0 {
		name := opt.StructName
		if name == "" {
			name = "EternalSunset_scratch"
		}
		groups := make([][]string, 0, len(nbt))
		for _, nb := range nbt {
			groups = append(groups, nb.WithOrigin(opt.originOrDefault()).Commands(opt.Scratch, name))
		}
		st.NBTCmds = len(groups) * 6
		phases = append(phases, Phase{Label: "nbt", Groups: groups, Strict: true})
	}
	return phases, st
}

func (o Options) originOrDefault() [3]int32 { return o.Origin }

// Run 按序执行阶段：上一阶段彻底落定后才进入下一阶段。
//
// Strict 阶段一旦某条命令失败就中止该 group（继续下去只会在错误的暂存格内容上
// save/load），并尽力清空暂存格，然后把错误上报。
func Run(ctx context.Context, r Runner, phases []Phase, scratch ScratchCell) error {
	for _, ph := range phases {
		if !ph.Strict {
			for _, g := range ph.Groups {
				if err := r.Send(ctx, g...); err != nil {
					return fmt.Errorf("阶段 %s 提交失败: %w", ph.Label, err)
				}
			}
			if err := r.Settle(ctx); err != nil {
				return fmt.Errorf("阶段 %s 未落定: %w", ph.Label, err)
			}
			continue
		}

		for i, g := range ph.Groups {
			for j, c := range g {
				if err := r.Send(ctx, c); err != nil {
					return fmt.Errorf("阶段 %s 第 %d 组第 %d 条提交失败: %w", ph.Label, i, j, err)
				}
				if err := r.Settle(ctx); err != nil {
					cleanupScratch(ctx, r, scratch)
					return fmt.Errorf("阶段 %s 第 %d 组在第 %d 条处中止（暂存格已尝试清空）: %w",
						ph.Label, i, j, err)
				}
			}
		}
	}
	return nil
}

// cleanupScratch 在搬运中止时把暂存格清成空气，避免残留方块污染下一次搬运。
// 清理本身尽力而为：连这一步都失败时只能留给上层重连逻辑。
func cleanupScratch(ctx context.Context, r Runner, cell ScratchCell) {
	cmd := fmt.Sprintf("setblock %d %d %d air", cell[0], cell[1], cell[2])
	if err := r.Send(ctx, cmd); err != nil {
		return
	}
	_ = r.Settle(ctx)
}

