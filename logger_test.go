package slog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试服务器结构
type testLogServer struct {
	t            *testing.T
	server       *httptest.Server
	receivedLogs []*LogEntry // 修改为指针类型
	mu           sync.Mutex
}

// 创建测试服务器
func newTestLogServer(t *testing.T) *testLogServer {
	ts := &testLogServer{t: t}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 支持健康检查
		if r.Method == "HEAD" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != "POST" {
			t.Errorf("期望 POST 请求，得到 %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var logs []*LogEntry
		if err := json.Unmarshal(body, &logs); err != nil {
			t.Errorf("解析日志失败: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ts.mu.Lock()
		ts.receivedLogs = append(ts.receivedLogs, logs...)
		ts.mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	return ts
}

func (ts *testLogServer) close() {
	ts.server.Close()
}

func (ts *testLogServer) getLogs() []*LogEntry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.receivedLogs
}

// 测试初始化配置
func TestInit(t *testing.T) {
	// 创建临时目录
	tempDir, err := os.MkdirTemp("", "slog_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	config := &LogSetting{
		Console:    true,
		File:       true,
		FilePath:   tempDir,
		MaxSize:    1,
		MaxAge:     1,
		MaxBackups: 1,
		Compress:   false,
		LocalTime:  true,
		Format:     "test_%s.log",
		Level:      "debug",
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	// 验证日志文件是否创建
	files, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("读取日志目录失败: %v", err)
	}
	if len(files) == 0 {
		t.Error("未创建日志文件")
	}
}

// 测试日志级别和服务器发送
func TestLogLevelsAndServer(t *testing.T) {
	// 创建测试服务器
	ts := newTestLogServer(t)
	defer ts.close()

	// 创建临时目录
	tempDir, err := os.MkdirTemp("", "slog_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 配置日志系统
	config := &LogSetting{
		Console:  true,
		File:     true,
		FilePath: tempDir,
		Level:    "info",
		ErrorServers: []ErrorServer{
			{
				URL:       ts.server.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 1,
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	// 测试不同级别的日志
	Debug("debug message") // 不应该发送到服务器
	Info("info message")   // 不应该发送到服务器
	Error("error message") // 应该发送到服务器

	// 等待日志发送和处理
	time.Sleep(2 * time.Second)

	// 验证服务器接收的日志
	logs := ts.getLogs()
	if len(logs) == 0 {
		t.Error("服务器未收到日志")
		return
	}

	// 验证只有错误日志被发送
	for _, log := range logs {
		if log.Level != "error" {
			t.Errorf("收到非错误级别的日志: %s", log.Level)
		}
		if !strings.Contains(log.Message, "error message") {
			t.Errorf("日志消息不匹配: %s", log.Message)
		}
	}
}

// 测试批量发送
func TestBatchSending(t *testing.T) {
	ts := newTestLogServer(t)
	defer ts.close()

	config := &LogSetting{
		Console: true,
		Level:   "error",
		ErrorServers: []ErrorServer{
			{
				URL:       ts.server.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 3, // 设置批量大小为3
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	// 发送5条错误日志
	for i := 0; i < 5; i++ {
		Error(fmt.Sprintf("error message %d", i))
		time.Sleep(100 * time.Millisecond) // 添加短暂延迟确保顺序
	}

	// 等待所有日志发送完成
	// 由于 defaultFlushInterval 为 5 秒，我们等待 6 秒确保日志被发送
	time.Sleep(6 * time.Second)

	// 验证服务器接收的日志
	logs := ts.getLogs()
	if len(logs) != 5 {
		t.Errorf("期望收到5条日志，实际收到%d条", len(logs))
		return
	}

	// 验证日志内容和顺序
	for i, log := range logs {
		expectedMsg := fmt.Sprintf("error message %d", i)
		if !strings.Contains(log.Message, expectedMsg) {
			t.Errorf("日志消息不匹配，期望包含 %s，实际为 %s", expectedMsg, log.Message)
		}
	}
}

// 测试文件轮转
func TestLogRotation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "slog_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	config := &LogSetting{
		Console:    true,
		File:       true,
		FilePath:   tempDir,
		MaxSize:    1, // 1MB
		MaxBackups: 2,
		Format:     "test.log",
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	// 写入大量日志触发轮转
	bigMessage := strings.Repeat("a", 512*1024) // 512KB
	for i := 0; i < 3; i++ {
		Info(bigMessage)
	}

	// 等待文件写入
	time.Sleep(time.Second)

	// 检查备份文件
	files, err := filepath.Glob(filepath.Join(tempDir, "test.log*"))
	if err != nil {
		t.Fatalf("查找日志文件失败: %v", err)
	}

	// 应该有当前日志文件和最多2个备份
	if len(files) > 3 {
		t.Errorf("期望最多3个日志文件，实际有%d个", len(files))
	}
}

// 测试并发写入
func TestConcurrentWriting(t *testing.T) {
	ts := newTestLogServer(t)
	defer ts.close()

	config := &LogSetting{
		Console: true,
		Level:   "error",
		ErrorServers: []ErrorServer{
			{
				URL:       ts.server.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 100,
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	var wg sync.WaitGroup
	// 10个goroutine并发写入
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				Error(fmt.Sprintf("error from goroutine %d: %d", id, j))
				time.Sleep(10 * time.Millisecond) // 添加短暂延迟避免消息丢失
			}
		}(i)
	}

	wg.Wait()
	// 等待所有日志发送完成
	time.Sleep(3 * time.Second)

	logs := ts.getLogs()
	if len(logs) != 100 { // 10个goroutine * 10条日志
		t.Errorf("期望收到100条日志，实际收到%d条", len(logs))
	}
}

// 测试错误处理和重试
func TestErrorHandlingAndRetry(t *testing.T) {
	failCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(http.StatusOK)
			return
		}

		failCount++
		if failCount <= 2 { // 前两次请求失败
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &LogSetting{
		Console: true,
		Level:   "error",
		ErrorServers: []ErrorServer{
			{
				URL:       server.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     3, // 最多重试3次
				BatchSize: 1,
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	Error("test error")
	time.Sleep(5 * time.Second) // 增加等待时间确保重试完成

	if failCount < 2 {
		t.Errorf("重试次数不足，期望至少2次，实际%d次", failCount)
	}
}

// 测试服务器健康检查
func TestServerHealthCheck(t *testing.T) {
	// 创建一个模拟服务器，初始状态为健康
	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" || r.Method == "POST" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer healthyServer.Close()

	// 创建一个模拟服务器，总是返回错误
	unhealthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer unhealthyServer.Close()

	config := &LogSetting{
		Console: true,
		Level:   "error",
		ErrorServers: []ErrorServer{
			{
				URL:       healthyServer.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 1,
				Enabled:   true,
			},
			{
				URL:       unhealthyServer.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 1,
				Enabled:   true,
			},
			{
				URL:       "http://invalid-server:8080",
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 1,
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	time.Sleep(time.Second) // 等待初始化完成

	// 验证只有健康的服务器被初始化
	bufferMu.RLock()
	if len(buffers) != 1 {
		t.Errorf("期望1个活跃的缓冲区，实际有%d个", len(buffers))
	}
	if _, ok := buffers[healthyServer.URL]; !ok {
		t.Error("健康的服务器未被正确初始化")
	}
	bufferMu.RUnlock()
}

// 测试服务器状态变化
func TestServerStateTransition(t *testing.T) {
	// 创建一个可以控制状态的服务器
	serverStatus := true // 初始状态为健康
	statusMu := sync.Mutex{}
	receivedLogs := make([]*LogEntry, 0)
	logsMu := sync.Mutex{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			statusMu.Lock()
			healthy := serverStatus
			statusMu.Unlock()

			if healthy {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}

		if r.Method == "POST" {
			statusMu.Lock()
			healthy := serverStatus
			statusMu.Unlock()

			if !healthy {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			var logs []*LogEntry
			if err := json.Unmarshal(body, &logs); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			logsMu.Lock()
			receivedLogs = append(receivedLogs, logs...)
			logsMu.Unlock()

			w.WriteHeader(http.StatusOK)
			return
		}
	}))
	defer server.Close()

	config := &LogSetting{
		Console: true,
		Level:   "error",
		ErrorServers: []ErrorServer{
			{
				URL:       server.URL,
				MinLevel:  "error",
				Timeout:   1,
				Retry:     1,
				BatchSize: 1,
				Enabled:   true,
			},
		},
	}

	if err := Init(config); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	defer Cleanup()

	// 验证初始状态
	Error("initial message")
	time.Sleep(2 * time.Second)

	logsMu.Lock()
	initialCount := len(receivedLogs)
	logsMu.Unlock()
	if initialCount == 0 {
		t.Error("服务器健康时未收到初始消息")
	}

	// 模拟服务器变为不健康
	statusMu.Lock()
	serverStatus = false
	statusMu.Unlock()

	// 等待健康检查发现状态变化
	time.Sleep(healthCheckInterval + time.Second)

	// 发送消息，应该被跳过
	Error("should be skipped")
	time.Sleep(2 * time.Second)

	logsMu.Lock()
	afterUnhealthyCount := len(receivedLogs)
	logsMu.Unlock()
	if afterUnhealthyCount > initialCount {
		t.Error("不健康状态下收到了消息")
	}

	// 恢复服务器
	statusMu.Lock()
	serverStatus = true
	statusMu.Unlock()

	// 等待健康检查发现恢复
	time.Sleep(healthCheckInterval + time.Second)

	// 发送消息，应该成功
	Error("should be sent")
	time.Sleep(2 * time.Second)

	logsMu.Lock()
	finalCount := len(receivedLogs)
	logsMu.Unlock()
	if finalCount <= afterUnhealthyCount {
		t.Error("服务器恢复后未收到新消息")
	}
}
