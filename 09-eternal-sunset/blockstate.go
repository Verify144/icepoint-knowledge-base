// Package blockstate 提供方块状态的唯一规范化表示。
//
// 期望侧（merge/emit）持有已编码的后缀串，服务器回包侧（protocol.Block）持有
// map[string]any。差分校验要把两者放在一起比，必须共用同一个编码函数 —— 否则
// "石头被判定不是石头"这类误报会因为键序或数字格式不同而源源不断地产生。
//
// 语法分歧：旧导入器的 encodeStates 产出的是 Java/JSON 风格的 "key":value，
// 而基岩版命令使用 [key=value]。旧引擎从未连真实服务器验证过，所以这个差异
// 一直没暴露。本包按基岩版语法实现，若真服校准后发现目标版本另有要求，改这里一处即可。
package blockstate

import (
	"fmt"
	"sort"
)

// Encode 把方块状态编成命令用的后缀串，形如 "[a=b,c=d]"。
// 无状态返回空串（而非 "[]"），以便与方块名直接拼接。
// 键按字典序排序，保证同一份数据总得到同一串 —— 这是比较可用、且命令文本可复现的前提。
func Encode(states map[string]any) string {
	if len(states) == 0 {
		return ""
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]byte, 0, 16)
	out = append(out, '[')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, Value(states[k])...)
	}
	out = append(out, ']')
	return string(out)
}

// Value 把状态值写成命令里的裸形式。
//
// 特别注意 float64：JSON 解出的数字统一是 float64，整数值必须写成 "3" 而不是
// "3.000000"，否则与期望侧（写为 3）永远不相等，会误报成方块不对。
func Value(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case float32:
		return Value(float64(val))
	case int:
		return fmt.Sprintf("%d", val)
	case int32:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

