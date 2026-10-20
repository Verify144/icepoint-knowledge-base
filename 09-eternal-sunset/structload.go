// Package structload 把冰结构（IceStructure）解析出的建筑接进落日永恒的执行链。
//
// 格式解析不在这里做：37 种结构格式由 IceStructure 负责，而且它的 Java→基岩 名/状态
// 映射（bedrock.JavaToBedrock）正是我们必须走的一步 —— 网易服务端只认基岩名。
// 本包只管它管不到的三件事：坐标归一、空气剔除、以及把"带 NBT 的方块"标出来
// （那决定 plan 的阶段划分：带 NBT 的必须等所有 fill 落完再摆）。
package structload

import (
	"fmt"
	"os"

	ice "github.com/Verify144/IceStructure"

	"github.com/Verify144/eternal-sunset/blockstate"
	"github.com/Verify144/eternal-sunset/merge"
	"github.com/Verify144/eternal-sunset/plan"
)

// MaxFileBytes 是结构文件的体积上界。
//
// IceStructure 的 ParseFile 吃的是整块 []byte，所以必须先看体积再分配：文件声明
// 多大就分配多少，等于让一个陌生文件决定我们的内存 —— 这项目已经为此付过一次
// fatal "cannot allocate memory"。
const MaxFileBytes = 64 << 20

// Stats 是一次转换的口径，供日志与断言。
type Stats struct {
	Total     int // 输入方块数
	Skipped   int // 被剔掉的空气/虚空
	Mapped    int // 交给 plan 的方块数
	NBTBlocks int // 带 NBT，必须排在 fill 之后
	Unmapped  int // Java→基岩 转换失败，已跳过
	Size      [3]int32
}

func (s Stats) String() string {
	return fmt.Sprintf("方块 %d，跳过空气 %d，转换失败 %d → 待执行 %d（其中带 NBT %d），尺寸 %d×%d×%d",
		s.Total, s.Skipped, s.Unmapped, s.Mapped, s.NBTBlocks, s.Size[0], s.Size[1], s.Size[2])
}

// Load 读文件并自动识别格式。
func Load(path string) (*ice.Structure, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() <= 0 {
		return nil, fmt.Errorf("structload: %s 是空文件", path)
	}
	if st.Size() > MaxFileBytes {
		return nil, fmt.Errorf("structload: %s 有 %d 字节，超过上界 %d", path, st.Size(), int64(MaxFileBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse 从字节里识别格式并解出建筑。
func Parse(data []byte) (*ice.Structure, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("structload: 空数据")
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("structload: 数据 %d 字节，超过上界 %d", len(data), MaxFileBytes)
	}
	s, err := ice.ParseFile(data)
	if err != nil {
		return nil, fmt.Errorf("structload: %w", err)
	}
	if s == nil {
		return nil, fmt.Errorf("structload: 解析返回空建筑")
	}
	if len(s.Blocks) == 0 {
		return nil, fmt.Errorf("structload: 建筑里一个方块都没有（格式 %v）", s.Format)
	}
	return s, nil
}

// Skip 是"不该往世界里写"的方块名。
//
// structure_void 在结构文件里是"占位、保留原样"的意思，air 同理；把它们当方块铺
// 下去会把建筑内部掏空。
var skip = map[string]bool{
	"air":                      true,
	"minecraft:air":            true,
	"minecraft:structure_void": true,
	"structure_void":           true,
}

// Blocks 转成 plan.Build 的输入。
//
// 坐标一律归一到"以 0 为角"的相对坐标（不同格式的原点不一致：有的从负数开始），
// 落点由 plan.Options.Origin 去加 —— 两边都加一遍会把建筑整体推走一个自身尺寸。
func Blocks(s *ice.Structure) ([]plan.Block, Stats) {
	st := Stats{Total: len(s.Blocks)}
	sx, sy, sz := s.Bounds.Size()
	st.Size = [3]int32{sx, sy, sz}

	out := make([]plan.Block, 0, len(s.Blocks))
	for _, b := range s.Blocks {
		if skip[b.BlockName] {
			st.Skipped++
			continue
		}
		name, states, ok := b.ToBedrock()
		if !ok || name == "" {
			// 转换失败就跳过并计数：宁可少铺一格，也不要把 Java 名原样发给
			// 只认基岩名的服务端 —— 那样 fill 会整条报错，比缺一格难查得多。
			st.Unmapped++
			continue
		}
		if skip[name] {
			st.Skipped++
			continue
		}
		out = append(out, plan.Block{
			At: merge.P(
				b.X-s.Bounds.MinX,
				b.Y-s.Bounds.MinY,
				b.Z-s.Bounds.MinZ,
			),
			Name:   name,
			States: blockstate.Encode(states),
			Data:   b.NBT,
		})
		if len(b.NBT) > 0 {
			st.NBTBlocks++
		}
	}
	st.Mapped = len(out)
	return out, st
}

