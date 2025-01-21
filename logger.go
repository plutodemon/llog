package llog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// 全局常量
const (
	defaultFlushInterval = 5 * time.Second  // 默认刷新间隔
	maxBufferSize        = 8192             // 最大缓冲区大小
	healthCheckInterval  = 30 * time.Second // 健康检查间隔
	healthCheckTimeout   = 3 * time.Second  // 健康检查超时时间
)

// DefaultConfig 默认日志配置
var DefaultConfig = LogSetting{
	Console:      true,
	File:         true,
	FilePath:     "logs",
	MaxSize:      64,
	MaxAge:       7,
	MaxBackups:   30,
	Compress:     true,
	LocalTime:    true,
	Format:       "%s.log",
	Level:        "info",
	OutputFormat: "json",
	ErrorServers: []ErrorServer{},
}

var (
	log         *zap.Logger
	sugar       *zap.SugaredLogger
	initialized bool
	buffers     map[string]*LogBuffer // URL -> LogBuffer
	bufferMu    sync.RWMutex
	hostname, _ = os.Hostname()

	// 对象池
	entryPool = sync.Pool{
		New: func() interface{} {
			return &LogEntry{}
		},
	}

	bufferPool = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, 4096))
		},
	}

	// 共享 HTTP 客户端
	httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,              // 最大空闲连接数
			MaxIdleConnsPerHost: 10,               // 每个主机的最大空闲连接数
			IdleConnTimeout:     30 * time.Second, // 空闲连接超时时间
			DisableCompression:  false,            // 启用压缩
			ForceAttemptHTTP2:   true,             // 尝试使用 HTTP/2
			MaxConnsPerHost:     20,               // 每个主机的最大连接数
			WriteBufferSize:     4096,             // 写缓冲区大小
			ReadBufferSize:      4096,             // 读缓冲区大小
			DisableKeepAlives:   false,            // 启用 keep-alive
		},
		Timeout: 30 * time.Second, // 默认超时时间
	}
)

// 添加日志级别映射缓存
var levelMap = map[string]zapcore.Level{
	"debug": zapcore.DebugLevel,
	"info":  zapcore.InfoLevel,
	"warn":  zapcore.WarnLevel,
	"error": zapcore.ErrorLevel,
	"fatal": zapcore.FatalLevel,
}

// getLogLevel 获取日志级别
func getLogLevel(level string) zapcore.Level {
	if level == "" {
		return zapcore.InfoLevel
	}
	if l, ok := levelMap[strings.ToLower(level)]; ok {
		return l
	}
	return zapcore.InfoLevel
}

// LogEntry 表示一条要发送到服务器的日志
type LogEntry struct {
	Time    int64  `json:"time"` // 使用 Unix 时间戳代替 time.Time
	Level   string `json:"level"`
	Message string `json:"message"`
	Host    string `json:"host"`
}

// LogBuffer 日志缓冲器
type LogBuffer struct {
	entries      []*LogEntry
	maxSize      int
	mu           sync.Mutex
	server       ErrorServer
	flushTicker  *time.Ticker  // 使用 Ticker 替代 Timer
	healthTicker *time.Ticker  // 健康检查定时器
	done         chan struct{} // 用于优雅关闭
	isHealthy    bool          // 服务器是否健康
}

// LogSetting 日志配置结构
type LogSetting struct {
	Console      bool          `json:"console" toml:"console"`             // 是否输出到控制台
	File         bool          `json:"file" toml:"file"`                   // 是否输出到文件
	FilePath     string        `json:"file_path" toml:"file_path"`         // 日志文件路径
	MaxSize      int           `json:"max_size" toml:"max_size"`           // 每个日志文件的最大大小（MB）
	MaxAge       int           `json:"max_age" toml:"max_age"`             // 保留旧日志文件的最大天数
	MaxBackups   int           `json:"max_backups" toml:"max_backups"`     // 保留的旧日志文件的最大数量
	Compress     bool          `json:"compress" toml:"compress"`           // 是否压缩旧日志文件
	LocalTime    bool          `json:"local_time" toml:"local_time"`       // 是否使用本地时间
	Format       string        `json:"format" toml:"format"`               // 日志文件名格式
	Level        string        `json:"level" toml:"level"`                 // 日志级别
	OutputFormat string        `json:"output_format" toml:"output_format"` // 日志输出格式 ("json" 或 "text")
	ErrorServers []ErrorServer `json:"error_servers" toml:"error_servers"` // 错误日志服务器配置
}

// ErrorServer 错误日志服务器配置
type ErrorServer struct {
	URL       string `json:"url" toml:"url"`               // 服务器地址
	MinLevel  string `json:"min_level" toml:"min_level"`   // 最小日志级别
	Timeout   int    `json:"timeout" toml:"timeout"`       // 超时时间（秒）
	Retry     int    `json:"retry" toml:"retry"`           // 重试次数
	BatchSize int    `json:"batch_size" toml:"batch_size"` // 批量发送大小
	Enabled   bool   `json:"enabled" toml:"enabled"`       // 是否启用
}

// 检查服务器地址是否有效
func checkServerHealth(url string, timeout time.Duration) bool {
	if url == "" {
		return false
	}

	// 创建一个 HEAD 请求来检查服务器
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 检查响应状态码
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Init 初始化日志系统
func Init(logConfig *LogSetting) error {
	if initialized {
		return nil
	}

	if logConfig == nil {
		logConfig = &DefaultConfig
	}

	// 初始化缓冲区
	buffers = make(map[string]*LogBuffer)

	// 初始化错误日志服务器
	for i := range logConfig.ErrorServers {
		server := &logConfig.ErrorServers[i]
		if server.Timeout <= 0 {
			server.Timeout = 5
		}
		if server.Retry <= 0 {
			server.Retry = 3
		}
		if server.BatchSize <= 0 {
			server.BatchSize = 100
		}
		if server.MinLevel == "" {
			server.MinLevel = "error"
		}

		// 为每个启用的服务器创建缓冲区
		if server.Enabled {
			// 检查服务器健康状态
			isHealthy := checkServerHealth(server.URL, healthCheckTimeout)
			if !isHealthy {
				sugar.Warnf("服务器 %s 不可访问，暂时禁用", server.URL)
				continue
			}

			buffer := &LogBuffer{
				entries:   make([]*LogEntry, 0, server.BatchSize),
				maxSize:   server.BatchSize,
				server:    *server,
				done:      make(chan struct{}),
				isHealthy: true,
			}

			// 启动定时刷新
			buffer.flushTicker = time.NewTicker(defaultFlushInterval)
			// 启动健康检查
			buffer.healthTicker = time.NewTicker(healthCheckInterval)

			go buffer.flushLoop()
			go buffer.healthCheckLoop()

			buffers[server.URL] = buffer
		}
	}

	// 创建日志目录
	if logConfig.File {
		logDir := filepath.Clean(logConfig.FilePath)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return fmt.Errorf("创建日志目录失败: %v", err)
		}
	}

	// 设置日志级别
	logLevel := getLogLevel(logConfig.Level)

	// 创建编码器配置
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05.000"),
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var cores []zapcore.Core

	// 添加控制台输出
	if logConfig.Console {
		consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)
		cores = append(cores, zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), logLevel))
	}

	// 添加文件输出
	if logConfig.File {
		// 使用当前日期作为日志文件名
		fileName := fmt.Sprintf(logConfig.Format, time.Now().Format("2006-01-02"))
		path := filepath.Join(logConfig.FilePath, fileName)

		var fileEncoder zapcore.Encoder
		if logConfig.OutputFormat != "text" {
			fileEncoder = zapcore.NewJSONEncoder(encoderConfig)
		} else {
			fileEncoder = zapcore.NewConsoleEncoder(encoderConfig)
		}

		writer := zapcore.AddSync(&lumberjack.Logger{
			Filename:   filepath.Clean(path),
			MaxSize:    logConfig.MaxSize,
			MaxBackups: logConfig.MaxBackups,
			MaxAge:     logConfig.MaxAge,
			Compress:   logConfig.Compress,
			LocalTime:  logConfig.LocalTime,
		})
		cores = append(cores, zapcore.NewCore(fileEncoder, writer, logLevel))
	}

	// 创建核心
	core := zapcore.NewTee(cores...)

	// 创建logger
	log = zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(1),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)

	// 创建sugar logger
	sugar = log.Sugar()

	// 输出初始化信息
	sugar.Infof("日志系统初始化完成，日志级别: %s", strings.ToUpper(logConfig.Level))

	initialized = true
	return nil
}

// FormatError 格式化错误信息，去除重复
func FormatError(err error) string {
	if err == nil {
		return ""
	}

	parts := strings.Split(err.Error(), ": ")
	seen := make(map[string]bool)
	var unique []string

	for _, part := range parts {
		if !seen[part] {
			seen[part] = true
			unique = append(unique, part)
		}
	}

	return strings.Join(unique, ": ")
}

// Debug 输出调试级别日志
func Debug(args ...interface{}) {
	if !initialized {
		return
	}
	sugar.Debug(args...)
}

// Info 输出信息级别日志
func Info(args ...interface{}) {
	if !initialized {
		return
	}
	sugar.Info(args...)
}

// Warn 输出警告级别日志
func Warn(args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprint(args...)
	sugar.Warn(args...)
	sendToErrorServers(zapcore.WarnLevel, msg)
}

// Error 输出错误级别日志
func Error(args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprint(args...)
	sugar.Error(args...)
	sendToErrorServers(zapcore.ErrorLevel, msg)
}

// Fatal 输出致命错误日志并退出程序
func Fatal(args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprint(args...)
	sendToErrorServers(zapcore.FatalLevel, msg)
	sugar.Fatal(args...)
}

func DebugF(format string, args ...interface{}) {
	if !initialized {
		return
	}
	sugar.Debugf(format, args...)
}

func InfoF(format string, args ...interface{}) {
	if !initialized {
		return
	}
	sugar.Infof(format, args...)
}

func WarnF(format string, args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprintf(format, args...)
	sugar.Warnf(format, args...)
	sendToErrorServers(zapcore.WarnLevel, msg)
}

func ErrorF(format string, args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprintf(format, args...)
	sugar.Errorf(format, args...)
	sendToErrorServers(zapcore.ErrorLevel, msg)
}

func FatalF(format string, args ...interface{}) {
	if !initialized {
		return
	}
	msg := fmt.Sprintf(format, args...)
	sendToErrorServers(zapcore.FatalLevel, msg)
	sugar.Fatalf(format, args...)
}

// Sync 同步日志到磁盘
func Sync() {
	if !initialized || log == nil {
		return
	}

	_ = log.Sync()
	if sugar != nil {
		_ = sugar.Sync()
	}
}

// Cleanup 清理日志资源
func Cleanup() {
	if !initialized {
		return
	}

	if log != nil {
		Sync()
	}

	// 清理并刷新所有缓冲区
	bufferMu.Lock()
	defer bufferMu.Unlock()

	for _, buffer := range buffers {
		if buffer == nil {
			continue
		}
		if buffer.flushTicker != nil {
			buffer.flushTicker.Stop()
		}
		if buffer.healthTicker != nil {
			buffer.healthTicker.Stop()
		}
		close(buffer.done)
		// 使用 recover 保护 Flush 操作
		func() {
			defer func() {
				if r := recover(); r != nil {
					sugar.Errorf("清理缓冲区时发生错误: %v", r)
				}
			}()
			buffer.Flush()
		}()
	}
	buffers = nil
	initialized = false
}

// HandlePanic 处理panic并记录日志
func HandlePanic() {
	if r := recover(); r != nil {
		stack := debug.Stack()
		errorMsg := fmt.Sprintf("程序发生严重错误: %v\n堆栈信息:\n%s", r, stack)
		Error(errorMsg)
		Sync()
		os.Exit(1)
	}
}

func (b *LogBuffer) flushLoop() {
	for {
		select {
		case <-b.flushTicker.C:
			b.Flush()
		case <-b.done:
			return
		}
	}
}

func (b *LogBuffer) healthCheckLoop() {
	for {
		select {
		case <-b.healthTicker.C:
			isHealthy := checkServerHealth(b.server.URL, healthCheckTimeout)
			b.mu.Lock()
			if b.isHealthy != isHealthy {
				if isHealthy {
					sugar.Infof("服务器 %s 恢复可用", b.server.URL)
				} else {
					sugar.Warnf("服务器 %s 不可访问", b.server.URL)
				}
				b.isHealthy = isHealthy
			}
			b.mu.Unlock()
		case <-b.done:
			return
		}
	}
}

func (b *LogBuffer) Add(level zapcore.Level, msg string) {
	b.mu.Lock()
	if !b.isHealthy {
		b.mu.Unlock()
		return
	}

	entry := entryPool.Get().(*LogEntry)
	entry.Time = time.Now().Unix()
	entry.Level = level.String()
	entry.Message = msg
	entry.Host = hostname

	b.entries = append(b.entries, entry)
	shouldFlush := len(b.entries) >= b.maxSize
	b.mu.Unlock()

	if shouldFlush {
		go b.Flush()
	}
}

func (b *LogBuffer) Flush() {
	if b == nil {
		return
	}

	b.mu.Lock()
	if len(b.entries) == 0 {
		b.mu.Unlock()
		return
	}

	entries := b.entries
	// 预分配新的切片以减少内存分配
	b.entries = make([]*LogEntry, 0, b.maxSize)
	b.mu.Unlock()

	// 发送日志
	b.sendWithRetry(entries)

	// 回收对象
	for _, entry := range entries {
		if entry != nil {
			entryPool.Put(entry)
		}
	}
}

func (b *LogBuffer) sendWithRetry(entries []*LogEntry) {
	// 获取缓冲区
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	// 使用 json.NewEncoder 减少一次内存分配
	if err := json.NewEncoder(buf).Encode(entries); err != nil {
		sugar.Errorf("序列化日志数据失败: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(b.server.Timeout)*time.Second)
	defer cancel()

	// 重试逻辑
	var err error
	for attempt := 1; attempt <= b.server.Retry; attempt++ {
		if err = b.sendRequest(ctx, buf.Bytes()); err == nil {
			return
		}

		if attempt == b.server.Retry {
			sugar.Errorf("发送日志到服务器 %s 失败（最后一次尝试）: %v", b.server.URL, err)
			return
		}

		// 使用指数退避策略
		backoff := time.Duration(attempt*attempt) * 100 * time.Millisecond
		if backoff > time.Second {
			backoff = time.Second
		}
		time.Sleep(backoff)
	}
}

func (b *LogBuffer) sendRequest(ctx context.Context, data []byte) error {
	b.mu.Lock()
	if !b.isHealthy {
		b.mu.Unlock()
		return fmt.Errorf("服务器 %s 不可用", b.server.URL)
	}
	b.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, "POST", b.server.URL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("创建请求失败: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "llog/1.0")

	// 使用共享的 HTTP 客户端
	httpClient.Timeout = time.Duration(b.server.Timeout) * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		// 如果请求失败，标记服务器为不健康
		b.mu.Lock()
		b.isHealthy = false
		b.mu.Unlock()
		return fmt.Errorf("发送请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("服务器返回错误状态码: %d", resp.StatusCode)
	}

	return nil
}

// sendToErrorServers 发送日志到错误服务器
func sendToErrorServers(level zapcore.Level, msg string) {
	if !initialized {
		return
	}

	bufferMu.RLock()
	defer bufferMu.RUnlock()

	for _, buffer := range buffers {
		if !buffer.server.Enabled {
			continue
		}

		serverLevel := getLogLevel(buffer.server.MinLevel)
		if level < serverLevel {
			continue
		}

		buffer.Add(level, msg)
	}
}
