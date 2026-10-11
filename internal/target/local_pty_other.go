//go:build !linux && !darwin && !freebsd && !dragonfly && !netbsd && !openbsd

package target

// 非类 Unix 平台(Windows 等)无 PTY 支持:本机容器交互终端返回人读错误,其余本机能力
// (Exec/ExecStream/Upload)不受影响。

import "context"

// startLocalInteractive 在非类 Unix 平台不可用。
func startLocalInteractive(_ context.Context, _ []string) (Session, error) {
	return nil, ErrLocalInteractiveUnsupported
}
