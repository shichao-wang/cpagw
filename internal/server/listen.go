package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/shichao-wang/cpa-tui/internal/store"
)

const probeTimeout = 300 * time.Millisecond

func checkConfiguredPort(ctx context.Context, s *store.Store) error {
	state, err := s.Read()
	if err != nil {
		return err
	}
	return checkListenAvailable(ctx, state.Listen)
}

// checkListenAvailable 先探测同端口是否已有服务监听，再验证地址可绑定。
//
// 只做绑定检查并不足够：macOS 允许通配监听（如 *:8317）与具体 loopback 地址
// （如 127.0.0.1:8317）同时绑成功，因此绑定成功不能证明端口空闲。已有的通配
// 监听可能接走发往该地址的连接，必须先探测。
//
// 探测与正式绑定之间仍存在时间窗口，正式启动后的实例就绪校验负责兜底。
func checkListenAvailable(ctx context.Context, address string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("监听地址必须是 host:port")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("只允许 loopback 监听地址")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("监听端口必须在 1–65535 之间")
	}
	for _, target := range probeTargets(host, portText) {
		listening, probeErr := probeListening(ctx, target)
		if probeErr != nil {
			// 无法判断时必须拒绝启动，不能把不确定当成空闲。
			return fmt.Errorf("无法确认地址 %s 是否可用：%w", target, probeErr)
		}
		if listening {
			return fmt.Errorf("监听地址 %s 已被占用（检测到 %s 已有服务监听），请停止占用服务或用 --listen 指定其他端口", address, target)
		}
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("监听地址 %s 已被占用，请停止占用服务或用 --listen 指定其他端口", address)
		}
		return fmt.Errorf("无法监听地址 %s：%w", address, err)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("关闭端口预检查监听器失败：%w", err)
	}
	return nil
}

// probeTargets 覆盖同端口的全部 loopback 地址，避免只检查单一地址时
// 被通配监听或另一地址族上的监听绕过。
func probeTargets(host, port string) []string {
	seen := map[string]bool{}
	targets := make([]string, 0, 3)
	for _, candidate := range []string{host, "127.0.0.1", "::1"} {
		target := net.JoinHostPort(candidate, port)
		if !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	return targets
}

// probeListening 只建立 TCP 连接并立即关闭，不发送任何应用数据。
func probeListening(ctx context.Context, address string) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err == nil {
		connection.Close()
		return true, nil
	}
	// 明确拒绝或本机不支持该地址族，说明该地址上没有监听者，可继续检查。
	// 目标始终是 loopback：本机地址族不可用或没有路由即代表该地址没有监听者。
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) {
		return false, nil
	}
	return false, err
}
