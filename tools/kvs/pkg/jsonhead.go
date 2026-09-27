package pkg

// jsonhead.go — 只读 JSON 顶层头部的零分配扫描器。
//
// 背景：中继的 data 帧载荷对服务端是不透明的（原样转发给对端），但旧实现每帧都做
// 一次完整 json.Unmarshal，等于把可能几 MB 的 payload 解析、复制一遍再丢掉。
// 这里只扫描到能确定某个顶层成员为止：命中（type 在最前面，客户端就是这样发的）
// 时完全不碰 payload；未命中时也只是线性扫描（不分配、不解码）。
//
// 仅用于"是否需要走快路径"的判断：判断不出来（ok=false）一律回退完整解析，
// 因此行为只会更慢，不会更松。

import (
	"bytes"
	"encoding/json"
)

// jsonHeadStringIs 判断顶层对象里 key 的值是否为字符串 want。
// 扫描在读到该成员后立即停止；结构不符合预期返回 false（调用方回退完整解析）。
func jsonHeadStringIs(frame []byte, key, want string) bool {
	i := skipJSONSpace(frame, 0)
	if i >= len(frame) || frame[i] != '{' {
		return false
	}
	i++
	for {
		i = skipJSONSpace(frame, i)
		if i >= len(frame) || frame[i] != '"' {
			return false
		}
		kStart, kEnd, next, ok := jsonStringBounds(frame, i)
		if !ok {
			return false
		}
		i = skipJSONSpace(frame, next)
		if i >= len(frame) || frame[i] != ':' {
			return false
		}
		i = skipJSONSpace(frame, i+1)
		if jsonStringEquals(frame, kStart, kEnd, key) {
			if i >= len(frame) || frame[i] != '"' {
				return false
			}
			vStart, vEnd, _, ok := jsonStringBounds(frame, i)
			if !ok {
				return false
			}
			return jsonStringEquals(frame, vStart, vEnd, want)
		}
		next, ok = skipJSONValue(frame, i)
		if !ok {
			return false
		}
		i = skipJSONSpace(frame, next)
		if i < len(frame) && frame[i] == ',' {
			i++
			continue
		}
		return false
	}
}

// jsonStringEquals 比较 b[start:end] 与 want。无转义的常见情况直接字节比较
// （零分配）；含转义时才解码。
func jsonStringEquals(b []byte, start, end int, want string) bool {
	if start > end || end > len(b) {
		return false
	}
	raw := b[start:end]
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw) == want
	}
	var s string
	if err := json.Unmarshal(b[start-1:end+1], &s); err != nil {
		return false
	}
	return s == want
}

// jsonStringBounds 返回 b[i]=='\"' 处字符串的内容区间 [start, end) 与结束后的位置。
// 不解码，只按转义规则跳过 \\ 后的一个字节。
func jsonStringBounds(b []byte, i int) (start, end, next int, ok bool) {
	if i >= len(b) || b[i] != '"' {
		return 0, 0, i, false
	}
	i++
	start = i
	for i < len(b) {
		switch c := b[i]; {
		case c == '"':
			return start, i, i + 1, true
		case c == '\\':
			i += 2
		case c < 0x20:
			return 0, 0, i, false
		default:
			i++
		}
	}
	return 0, 0, i, false
}

// skipJSONValue 从 i 开始跳过一个完整 JSON 值，返回其后的位置。
// 只做结构扫描（不分配、不解码），嵌套只数同种括号，深度用显式计数器，不递归。
func skipJSONValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return i, false
	}
	switch b[i] {
	case '"':
		_, _, next, ok := jsonStringBounds(b, i)
		return next, ok
	case '{', '[':
		open := b[i]
		closer := byte('}')
		if open == '[' {
			closer = ']'
		}
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				_, _, next, ok := jsonStringBounds(b, j)
				if !ok {
					return 0, false
				}
				j = next - 1
			case open:
				depth++
			case closer:
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	default:
		// 数字 / true / false / null：扫到分隔符为止
		j := i
		for j < len(b) {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return j, true
			}
			j++
		}
		return j, true
	}
}

// skipJSONSpace 跳过空白，返回第一个非空白位置。
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}
