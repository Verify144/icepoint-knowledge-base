package blockid

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// table.tsv 由仓库外的离线生成器产出（依赖参照库读网易的 .nbt），
// 每行 `runtimeID \t 方块名 \t [状态]`，共 15839 行、ID 无重复。
// 生成过程与判据见 cmd/calib/FINDINGS.md；运行期不需要那个 .nbt 文件。
//
//go:embed blockid_table.tsv
var tableRaw []byte

// Entry 是一个方块状态：名字加规范化后的状态后缀。
type Entry struct {
	ID     uint32
	Name   string
	Suffix string // "[k=v,...]"，键已排序；无状态时为空串
	States map[string]any
}

// Table 是 runtime ID 到方块的索引。
type Table struct {
	byID map[uint32]Entry
}

// String 便于在日志里报告表规模。
func (t *Table) String() string { return fmt.Sprintf("方块表 %d 条", len(t.byID)) }

func (t *Table) Len() int { return len(t.byID) }

// Lookup 按线上 runtime ID 取方块。没命中时返回 false 而不是编一个
// "runtime:123" 之类的占位名 —— 那会把"表没覆盖"伪装成"服务器上是这个方块"。
func (t *Table) Lookup(id uint32) (Entry, bool) {
	e, ok := t.byID[id]
	return e, ok
}

var (
	loadOnce sync.Once
	loaded   *Table
	loadErr  error
)

// MustTable 返回内嵌表；解析失败是构建期问题，直接 panic。
func MustTable() *Table {
	loadOnce.Do(func() { loaded, loadErr = ParseTable(tableRaw) })
	if loadErr != nil {
		panic(loadErr)
	}
	return loaded
}

// ParseTable 解析表的文本格式。单独露出来是为了能让测试直接喂坏数据进来验判据。
func ParseTable(b []byte) (*Table, error) {
	t := &Table{byID: make(map[uint32]Entry, 16000)}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 1<<16)
	line := 0
	for sc.Scan() {
		line++
		row := sc.Text()
		if row == "" || strings.HasPrefix(row, "#") {
			continue
		}
		f := strings.Split(row, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("blockid: 第 %d 行有 %d 列，需要 3 列", line, len(f))
		}
		id, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("blockid: 第 %d 行 ID %q 不是数: %w", line, f[0], err)
		}
		if f[1] == "" {
			return nil, fmt.Errorf("blockid: 第 %d 行没有方块名", line)
		}
		states, err := parseSuffix(f[2])
		if err != nil {
			return nil, fmt.Errorf("blockid: 第 %d 行: %w", line, err)
		}
		e := Entry{ID: uint32(id), Name: f[1], Suffix: f[2], States: states}
		if dup, ok := t.byID[e.ID]; ok {
			return nil, fmt.Errorf("blockid: ID %d 同时是 %s%s 和 %s%s —— 表或算法有问题",
				e.ID, dup.Name, dup.Suffix, e.Name, e.Suffix)
		}
		t.byID[e.ID] = e
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("blockid: 读表失败: %w", err)
	}
	if len(t.byID) == 0 {
		return nil, fmt.Errorf("blockid: 表是空的")
	}
	return t, nil
}

// parseSuffix 解出 "[a=b,c=d]"。值统一按字符串存：命令文本和 blockstate.Encode
// 都只用到它的字面形式，存成数字反而会在 int32(3)/float64(3) 之间引入格式漂移。
func parseSuffix(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("状态后缀 %q 不是 [...] 形式", s)
	}
	body := s[1 : len(s)-1]
	out := map[string]any{}
	for _, part := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("状态项 %q 不是 key=value", part)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("状态键 %q 重复", k)
		}
		out[k] = v
	}
	return out, nil
}

