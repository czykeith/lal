// Copyright 2019, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/q191201771/naza/pkg/bininfo"
	"github.com/q191201771/naza/pkg/nazalog"

	"github.com/q191201771/lal/pkg/base"
	"github.com/q191201771/lal/pkg/logic"
)

const (
	// 优雅关闭超时时间
	shutdownTimeout = 30 * time.Second
)

func main() {
	// 确保日志同步
	defer func() {
		nazalog.Sync()
	}()

	// 全局 panic 恢复处理
	defer func() {
		if r := recover(); r != nil {
			// 获取 panic 的堆栈信息
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			nazalog.Errorf("panic recovered: %v\n%s", r, buf[:n])
			os.Exit(1)
		}
	}()

	// 解析命令行参数
	confFilename, workDir, err := parseFlag()
	if err != nil {
		nazalog.Errorf("parse flag failed: %+v", err)
		os.Exit(1)
	}

	// 切换工作目录（如果指定）
	if workDir != "" {
		if err := changeWorkDir(workDir); err != nil {
			nazalog.Errorf("change work directory failed: dir=%s, err=%+v", workDir, err)
			os.Exit(1)
		}
	}

	// 创建上下文用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 设置信号处理，实现优雅关闭
	sigChan := make(chan os.Signal, 1)
	// 在 Windows 上，SIGTERM 可能不可用，但 SIGINT 可用（对应 Ctrl+C）
	// 在 Unix 系统上，两个信号都可用
	signals := []os.Signal{syscall.SIGINT}
	if runtime.GOOS != "windows" {
		signals = append(signals, syscall.SIGTERM)
	}
	signal.Notify(sigChan, signals...)

	// 在单独的 goroutine 中处理信号
	go func() {
		sig := <-sigChan
		nazalog.Infof("received signal: %v, starting graceful shutdown...", sig)
		cancel()
	}()

	// 创建 LAL 服务器实例
	lals := logic.NewLalServer(func(option *logic.Option) {
		option.ConfFilename = confFilename
	})

	if lals == nil {
		nazalog.Error("failed to create lal server instance")
		os.Exit(1)
	}

	// 在单独的 goroutine 中运行服务器
	serverErrChan := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				nazalog.Errorf("server panic recovered: %v\n%s", r, buf[:n])
				serverErrChan <- fmt.Errorf("server panic: %v", r)
			}
		}()

		err := lals.RunLoop()
		if err != nil {
			nazalog.Errorf("lal server run loop error: %+v", err)
		}
		serverErrChan <- err
	}()

	// 等待上下文取消或服务器错误
	select {
	case <-ctx.Done():
		// 收到关闭信号，执行优雅关闭
		nazalog.Infof("shutdown signal received, disposing server...")

		// 使用超时上下文确保不会无限期等待
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			// 调用 Dispose 方法进行优雅关闭
			lals.Dispose()
		}()

		select {
		case <-done:
			nazalog.Infof("server disposed successfully")
		case <-shutdownCtx.Done():
			nazalog.Warnf("server dispose timeout after %v", shutdownTimeout)
		}
		nazalog.Infof("lal server shutdown complete")

	case err := <-serverErrChan:
		// 服务器运行出错
		if err != nil {
			nazalog.Errorf("lal server exited with error: %+v", err)
			os.Exit(1)
		}
		nazalog.Infof("lal server exited normally")
	}
}

// parseFlag 解析命令行参数
// 返回: 配置文件路径, 工作目录, 错误
func parseFlag() (string, string, error) {
	binInfoFlag := flag.Bool("v", false, "show bin info")
	cf := flag.String("c", "", "specify conf file")
	p := flag.String("p", "", "specify current work directory")
	flag.Parse()

	// 显示版本信息
	if *binInfoFlag {
		_, _ = fmt.Fprint(os.Stderr, bininfo.StringifyMultiLine())
		_, _ = fmt.Fprintln(os.Stderr, base.LalFullInfo)
		os.Exit(0)
	}

	// 验证工作目录路径（如果提供）
	if *p != "" {
		absPath, err := filepath.Abs(*p)
		if err != nil {
			return "", "", fmt.Errorf("invalid work directory path: %w", err)
		}

		// 检查路径是否存在
		info, err := os.Stat(absPath)
		if err != nil {
			return "", "", fmt.Errorf("work directory does not exist: %w", err)
		}
		if !info.IsDir() {
			return "", "", fmt.Errorf("work directory path is not a directory: %s", absPath)
		}
		*p = absPath
	}

	// 验证配置文件路径（如果提供）
	if *cf != "" {
		absPath, err := filepath.Abs(*cf)
		if err != nil {
			return "", "", fmt.Errorf("invalid config file path: %w", err)
		}

		// 检查文件是否存在
		info, err := os.Stat(absPath)
		if err != nil {
			return "", "", fmt.Errorf("config file does not exist: %w", err)
		}
		if info.IsDir() {
			return "", "", fmt.Errorf("config file path is a directory: %s", absPath)
		}
		*cf = absPath
	}

	return *cf, *p, nil
}

// changeWorkDir 切换工作目录
func changeWorkDir(dir string) error {
	absPath, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("get absolute path failed: %w", err)
	}

	if err := os.Chdir(absPath); err != nil {
		return fmt.Errorf("change directory failed: %w", err)
	}

	// 验证切换是否成功
	currentDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get current directory failed: %w", err)
	}

	if currentDir != absPath {
		return fmt.Errorf("directory change verification failed: expected=%s, actual=%s", absPath, currentDir)
	}

	nazalog.Infof("work directory changed to: %s", absPath)
	return nil
}
