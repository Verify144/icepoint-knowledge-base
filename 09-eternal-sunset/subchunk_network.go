package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// 子区块的网络序列化（1.18.10 之后那一套）。
//
// 这一份是按参照实现 + 真机数据两边对出来的，三处最容易搞错的地方都在注释里写死了：
//  1. 存储头部字节存的是 (位宽<<1)|标志，所以位宽 = 头部>>1；右移后为 0x7f 表示
//     "沿用上一层的调色板"，为 0 表示"单一值存储"（没有索引数据，调色板只有 1 项）。
//     最低那位是"子区块请求模式"标志：真机 0xAE 回包里它是 1（0x09、0x07 这种），
//     我们只解不编，所以不保留它 —— 别拿它去做逐字节回环断言。
//  2. 索引不是按字节位流铺开，而是按 **uint32 小端**打包；且位宽不整除 32 时每个
//     uint32 只用掉 floor(32/位宽) 个下标、其余补空，因此还要**多一个 uint32**
//     才装得下 4096 个下标。
//  3. 调色板的条目数和每个 runtime ID 都是 **zigzag varint32**，不是无符号 uvarint；
//     按 uvarint 读会把 10 读成 20、把 2150698529 读成 4288537533（这两个错真犯过）。
//
// 版本 1 只有一层且没有层数字节；版本 8 有层数；版本 9 在层数之后多一个"绝对 Y 下标"
// 字节。坐标线性序为 x*256 + z*16 + y（y 在最内），不是 y*256 + ...。

const (
	SubChunkVersionSingleLayer = 1
	SubChunkVersionMultiLayer  = 8
	SubChunkVersionIndexed     = 9

	// MaxSubChunkLayers 是单个子区块允许的图层数（现实中是 1 或 2：方块 + 水）。
	MaxSubChunkLayers = 256

	blocksPerSubChunk = 4096
)

const inheritPalette = 0x7f

type BlockLayer struct {
	// WordSize 是每个下标占的位数；0 表示单一值存储。
	WordSize int
	// Indices 是 4096 个调色板下标，顺序为 x*256 + z*16 + y。WordSize 为 0 时为 nil。
	Indices []uint16
	// RuntimePalette 按下标返回线上 runtime ID。名字要另查 ID 表，网络序列化里
	// 不含方块名 —— 这就是在区块包里搜不到 "minecraft:stone" 的原因。
	RuntimePalette []uint32
}

// SubChunkData 是一个 16×16×16 的子区块。
type SubChunkData struct {
	Version int
	// YIndex 只有版本 9 带：子区块在世界里的绝对 Y 下标。
	YIndex int
	Layers []BlockLayer
	// Blocks 由上层注入（bridge.Fetch 拿到子区块后设置），把 runtime ID 翻成方块。
	Blocks BlockResolver
}

func (l *BlockLayer) RuntimeIDAt(lx, ly, lz int) (uint32, bool) {
	if len(l.RuntimePalette) == 0 {
		return 0, false
	}
	if l.WordSize == 0 {
		return l.RuntimePalette[0], true
	}
	i := lx*256 + lz*16 + ly
	if i < 0 || i >= len(l.Indices) || int(l.Indices[i]) >= len(l.RuntimePalette) {
		return 0, false
	}
	return l.RuntimePalette[l.Indices[i]], true
}

// DecodeSubChunk 从网络序列化读一个子区块。
func DecodeSubChunk(r io.Reader) (*SubChunkData, error) {
	ver, err := readNBTByte(r)
	if err != nil {
		return nil, err
	}
	d := &SubChunkData{Version: int(ver)}

	count := 1
	switch ver {
	case SubChunkVersionSingleLayer:
	case SubChunkVersionMultiLayer, SubChunkVersionIndexed:
		c, err := readNBTByte(r)
		if err != nil {
			return nil, err
		}
		count = int(c)
		if count > MaxSubChunkLayers {
			return nil, fmt.Errorf("protocol: 子区块图层数 %d 超过上界 %d", count, MaxSubChunkLayers)
		}
		if ver == SubChunkVersionIndexed {
			y, err := readNBTByte(r)
			if err != nil {
				return nil, err
			}
			d.YIndex = int(int8(y))
		}
	default:
		return nil, fmt.Errorf("protocol: 未知的子区块版本 %d", ver)
	}

	var prevPalette []uint32
	var prevWordSize int
	for i := 0; i < count; i++ {
		layer, err := decodePalettedStorage(r, prevPalette, prevWordSize)
		if err != nil {
			return nil, fmt.Errorf("第 %d 层: %w", i, err)
		}
		prevPalette, prevWordSize = layer.RuntimePalette, layer.WordSize
		d.Layers = append(d.Layers, layer)
	}
	return d, nil
}

func decodePalettedStorage(r io.Reader, prevPalette []uint32, prevWordSize int) (BlockLayer, error) {
	var layer BlockLayer
	header, err := readNBTByte(r)
	if err != nil {
		return layer, err
	}
	wordSize := int(header >> 1)
	switch {
	case wordSize == inheritPalette:
		// "沿用上一层调色板"：头部右移后是 0x7f，索引仍按上一层的位宽排。
		if prevPalette == nil {
			return layer, fmt.Errorf("要求沿用调色板，但没有上一层可沿")
		}
		layer.WordSize = prevWordSize
		if layer.Indices, err = readIndices(r, prevWordSize); err != nil {
			return layer, err
		}
		layer.RuntimePalette = prevPalette
	case wordSize == 0:
		// 单一值存储：没有索引数据，调色板固定 1 项且不写条目数前缀。
		layer.WordSize = 0
		if layer.RuntimePalette, err = readRuntimePalette(r, 1, true); err != nil {
			return layer, err
		}
	case wordSize > 32:
		return layer, fmt.Errorf("位宽 %d 超出 32", wordSize)
	default:
		layer.WordSize = wordSize
		if layer.Indices, err = readIndices(r, wordSize); err != nil {
			return layer, err
		}
		n, err := ReadZigzagVarint(r)
		if err != nil {
			return layer, fmt.Errorf("读调色板条目数: %w", err)
		}
		if n <= 0 || n > MaxPaletteSize {
			return layer, fmt.Errorf("调色板条目数 %d 不合法（上界 %d）", n, MaxPaletteSize)
		}
		if layer.RuntimePalette, err = readRuntimePalette(r, int(n), false); err != nil {
			return layer, err
		}
	}
	return layer, nil
}

// indexWords 返回索引区占多少个 uint32。
//
// 位宽不整除 32 时（3/5/6/7…）每个 uint32 只装 floor(32/位宽) 个下标，
// 且必须再多给一个 uint32 才容得下第 4096 个下标 —— 少一个就会把调色板
// 当成索引读，症状是"条目数巨大/名字全错"而不是报错。
func indexWords(wordSize int) int {
	per := 32 / wordSize
	n := blocksPerSubChunk / per
	if 32%wordSize != 0 {
		n++
	}
	return n
}

func readIndices(r io.Reader, wordSize int) ([]uint16, error) {
	if wordSize <= 0 {
		return nil, nil
	}
	buf := make([]byte, indexWords(wordSize)*4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("读索引: %w", err)
	}
	return unpackIndexedWords(buf, wordSize), nil
}

// unpackIndexedWords 按"每个 uint32 装 floor(32/位宽) 个下标、余位补空"展开。
func unpackIndexedWords(buf []byte, wordSize int) []uint16 {
	per := 32 / wordSize
	filled := per * wordSize
	mask := uint32(1)<<uint(wordSize) - 1
	out := make([]uint16, 0, blocksPerSubChunk)
	for w := 0; w+3 < len(buf) && len(out) < blocksPerSubChunk; w += 4 {
		v := binary.LittleEndian.Uint32(buf[w:])
		for k := 0; k < per && len(out) < blocksPerSubChunk; k++ {
			bit := k * wordSize
			if bit+wordSize > filled {
				break
			}
			out = append(out, uint16((v>>uint(bit))&mask))
		}
	}
	for len(out) < blocksPerSubChunk {
		out = append(out, 0)
	}
	return out
}

func readRuntimePalette(r io.Reader, n int, noCount bool) ([]uint32, error) {
	out := make([]uint32, 0, n)
	if noCount {
		v, err := ReadZigzagVarint(r)
		if err != nil {
			return nil, fmt.Errorf("读单一值 runtime ID: %w", err)
		}
		return append(out, uint32(v)), nil
	}
	for i := 0; i < n; i++ {
		v, err := ReadZigzagVarint(r)
		if err != nil {
			return nil, fmt.Errorf("读第 %d 个 runtime ID: %w", i, err)
		}
		out = append(out, uint32(v))
	}
	return out, nil
}

// EncodeSubChunk 按网络序列化写回一个子区块，与 DecodeSubChunk 严格互逆
// （round-trip 由真机夹具的那段字节来验）。
func EncodeSubChunk(w io.Writer, d *SubChunkData) error {
	if err := writeNBTByte(w, byte(d.Version)); err != nil {
		return err
	}
	switch d.Version {
	case SubChunkVersionSingleLayer:
	case SubChunkVersionMultiLayer:
		if err := writeNBTByte(w, byte(len(d.Layers))); err != nil {
			return err
		}
	case SubChunkVersionIndexed:
		if err := writeNBTByte(w, byte(len(d.Layers))); err != nil {
			return err
		}
		if err := writeNBTByte(w, byte(int8(d.YIndex))); err != nil {
			return err
		}
	default:
		return fmt.Errorf("protocol: 不能编码子区块版本 %d", d.Version)
	}
	for i := range d.Layers {
		if err := encodeLayer(w, &d.Layers[i]); err != nil {
			return fmt.Errorf("第 %d 层: %w", i, err)
		}
	}
	return nil
}

func encodeLayer(w io.Writer, l *BlockLayer) error {
	if l.WordSize == 0 {
		if err := writeNBTByte(w, 0); err != nil {
			return err
		}
		return writeZigzag(w, int64(l.RuntimePalette[0]))
	}
	if err := writeNBTByte(w, byte(l.WordSize<<1)); err != nil {
		return err
	}
	buf := make([]byte, indexWords(l.WordSize)*4)
	packIntoWords(buf, l.Indices, l.WordSize)
	if _, err := w.Write(buf); err != nil {
		return err
	}
	if err := writeZigzag(w, int64(len(l.RuntimePalette))); err != nil {
		return err
	}
	for _, id := range l.RuntimePalette {
		if err := writeZigzag(w, int64(id)); err != nil {
			return err
		}
	}
	return nil
}

func packIntoWords(buf []uint8, indices []uint16, wordSize int) {
	per := 32 / wordSize
	mask := uint32(1)<<uint(wordSize) - 1
	for i := 0; i < len(indices) && i < blocksPerSubChunk; i++ {
		w := (i / per) * 4
		if w+3 >= len(buf) {
			return
		}
		bit := (i % per) * wordSize
		v := binary.LittleEndian.Uint32(buf[w:])
		v |= (uint32(indices[i]) & mask) << uint(bit)
		binary.LittleEndian.PutUint32(buf[w:], v)
	}
}

func writeZigzag(w io.Writer, v int64) error { return WriteZigzagVarint(w, v) }

// BlockResolver 把线上 runtime ID 翻成方块名与状态。
//
// 网络序列化里只有 runtime ID，没有方块名，所以"名字"必须由调用方注入一张表
// （由 netease_block_states 算 FNV1a 生成）。没有注入时 BlockAt 明确返回 not-ok：
// 让上层把它记成"读不到"，而不是拿 runtime:<id> 这种占位名去和期望值比对。
type BlockResolver func(runtimeID uint32) (Block, bool)

// RuntimeIDAt 返回子区块第一层某处的线上 runtime ID。
func (s *SubChunkData) RuntimeIDAt(lx, ly, lz int) (uint32, bool) {
	if len(s.Layers) == 0 {
		return 0, false
	}
	return s.Layers[0].RuntimeIDAt(lx, ly, lz)
}

// BlockAt 返回解析后的方块；未注入 Blocks 或该处无数据时返回 false。
func (s *SubChunkData) BlockAt(lx, ly, lz int) (Block, bool) {
	id, ok := s.RuntimeIDAt(lx, ly, lz)
	if !ok || s.Blocks == nil {
		return Block{}, false
	}
	return s.Blocks(id)
}

