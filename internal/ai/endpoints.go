package ai

import (
	"strings"
)

// Base URL 原样拼接契约:用户填什么前缀就用什么前缀(含 /v1、/v2、/api/paas/v4 等),
// 仅去除首尾空白与尾部斜杠;绝不猜测或改写版本段 —— 裸根形式(如 https://api.openai.com)
// 不再自动补 /v1,由默认值/预设直接携带完整前缀。
func apiEndpoint(provider, base, resource string) string {
	if resource == "models" {
		if _, catalogBase, special := catalogDestination(provider, base); special {
			return catalogBase + "/models"
		}
	}
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/" + resource
}
