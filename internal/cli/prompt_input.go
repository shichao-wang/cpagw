package cli

import (
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// promptInput 保留 term.File 接口，但不提供 cancelreader.File 的 Name。
// 锁定版本的 epoll 取消器会在内部 reader 未退出时关闭信号 FD；由本工具
// 使用有界 poll 取消读取并等待结束，避免依赖该存在竞态的生命周期。
type promptInput struct {
	file  *os.File
	fd    int
	mu    sync.Mutex
	stop  bool
	reads sync.WaitGroup
}

func newPromptInput(file *os.File) *promptInput {
	return &promptInput{file: file, fd: int(file.Fd())}
}

func (r *promptInput) Fd() uintptr                    { return uintptr(r.fd) }
func (r *promptInput) Write(data []byte) (int, error) { return r.file.Write(data) }

func (r *promptInput) Read(data []byte) (int, error) {
	r.mu.Lock()
	if r.stop {
		r.mu.Unlock()
		return 0, io.EOF
	}
	r.reads.Add(1)
	r.mu.Unlock()
	defer r.reads.Done()
	for {
		r.mu.Lock()
		stopped := r.stop
		r.mu.Unlock()
		if stopped {
			return 0, io.EOF
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, 10); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, err
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				return 0, io.EOF
			}
			continue
		}
		n, err := unix.Read(r.fd, data)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if n == 0 && err == nil {
			err = io.EOF
		}
		return n, err
	}
}

func (r *promptInput) Close() error {
	r.mu.Lock()
	r.stop = true
	r.mu.Unlock()
	r.reads.Wait()
	// 输入文件由调用方拥有，只终止本会话读取，不关闭 stdin。
	return nil
}
