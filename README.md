# llog

基于 [go.uber.org/zap](https://github.com/uber-go/zap) 日志库的封装，提供了简单的配置方式

## 特性

- 基于高性能的zap日志库
- 支持同时输出到控制台和文件
- 支持日志文件自动轮转和压缩
- 支持多环境配置（开发环境、生产环境）
- 支持错误日志远程上报（支持多服务器、失败重试、健康检查）
- 支持批量发送和异步处理
- 支持panic自动恢复和记录
- 完整的日志级别支持（Debug/Info/Warn/Error/Fatal）

## 安装

```bash
go get github.com/plutodemon/llog
```

## 配置说明

通过TOML文件进行配置，支持以下配置项：

```toml
# 基础配置
console = true            # 是否输出到控制台
file = true               # 是否输出到文件
file_path = "logs"        # 日志文件路径
max_size = 64             # 每个日志文件的最大大小（MB）
max_age = 7               # 保留旧日志文件的最大天数
max_backups = 30          # 保留的旧日志文件的最大数量
compress = true           # 是否压缩旧日志文件
local_time = true         # 是否使用本地时间
format = "%s.log"         # 日志文件名格式
is_dev = false            # 是否为开发环境
level = "info"            # 日志级别 (debug/info/warn/error/fatal)
output_format = "json"    # 日志输出格式（json/text）

# 错误日志服务器配置（可选）
[[error_servers]]
url = "http://log-server:8080/logs"  # 服务器地址
min_level = "error"                  # 最小日志级别
timeout = 5                          # HTTP请求超时时间（秒）
retry = 3                            # 发送失败时的重试次数
batch_size = 100                     # 批量发送的日志条数
enabled = true                       # 是否启用此服务器
```

## 使用示例

```go
package main

import (
	"github.com/plutodemon/llog"
)

func main() {
	// 初始化日志配置
	config := &llog.LogSetting{
		Console:      true,
		File:         true,
		FilePath:     "logs",
		Level:        "info",
		OutputFormat: "json",
	}

	err := llog.Init(config)
	if err != nil {
		panic(err)
	}
	defer llog.Cleanup()

	// 使用defer处理panic
	defer llog.HandlePanic()

	// 记录不同级别的日志
	llog.Debug("这是一条调试日志")
	llog.Info("这是一条信息日志")
	llog.Warn("这是一条警告日志")
	llog.Error("这是一条错误日志")

	// 格式化日志
	llog.InfoF("用户 %s 登录成功", "张三")
	llog.ErrorF("处理失败: %v", err)
}

```

## 注意事项

1. 在程序退出前调用`llog.Cleanup()`确保日志被正确写入
2. 使用`llog.HandlePanic()`可以自动捕获和记录panic
3. 错误日志服务器配置是可选的，可以配置多个服务器
4. 日志文件会自动按照配置的大小进行轮转
5. 支持开发环境和生产环境的不同配置
