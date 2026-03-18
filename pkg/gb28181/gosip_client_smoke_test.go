package gb28181

import (
	"testing"

	"github.com/ghettovoice/gosip"
)

// TestGosipClientExists 用于编译期验证当前 gosip 依赖是否包含 Client 能力。
// 如果 gosip.Client / ClientConfig / NewClient 不存在，本测试在编译阶段就会失败。
func TestGosipClientExists(t *testing.T) {
	var _ gosip.Client
	var _ gosip.ClientConfig

	// 仅测试符号是否存在，不真正发起网络请求。
	_ = gosip.NewClient
}
