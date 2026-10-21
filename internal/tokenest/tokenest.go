// Package tokenest 提供上下文 token 的字符近似估算(ai 与 opschat 共用;
// 独立小包以维持 opschat 不 import ai 的分层约束)。
//
// 口径:中日韩(CJK)字符每字约 1 token(UTF-8 占 3 字节,按字节/4 会低估一半),
// 其余内容按 4 字节 ≈ 1 token。无 tokenizer 依赖,刻意保守(宁可高估触发
// 提前压缩,不冒险溢出)。与 ZCode(chars/3)、OpenCode(chars/4) 的简化口径同类,
// 仅对 CJK 部分做了修正。
package tokenest

import "unicode"

// Estimate 返回 s 的近似 token 数(向上取整;空串为 0)。
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	cjk, rest := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			rest += utf8RuneLen(r)
		}
	}
	tokens := cjk + (rest+3)/4
	return tokens
}

func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意
		r >= 0x3400 && r <= 0x4DBF, // 扩展 A
		r >= 0x20000 && r <= 0x2A6DF, // 扩展 B
		r >= 0x3040 && r <= 0x30FF, // 平假名/片假名
		r >= 0xAC00 && r <= 0xD7AF, // 谚文音节
		r >= 0x3000 && r <= 0x303F, // CJK 标点
		unicode.Is(unicode.Han, r):
		return true
	}
	return false
}

// utf8RuneLen 返回 rune 的 UTF-8 编码字节数(与 len(string(r)) 等价的快速路径)。
func utf8RuneLen(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case r < 0x800:
		return 2
	case r < 0x10000:
		return 3
	default:
		return 4
	}
}
