module github.com/q191201771/lal

// 使用 Go 1.21 编译，确保依赖版本兼容当前编译器
go 1.21

require (
	github.com/q191201771/naza v0.30.49
	// 降级到兼容 Go 1.21 的版本，避免 go >= 1.24.0 的要求
	golang.org/x/text v0.14.0
)
