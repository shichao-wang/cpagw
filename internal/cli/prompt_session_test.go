package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestTerminalPrompterCloseBeforeStart(t *testing.T) {
	p := newTerminalPrompter(strings.NewReader("unused"), io.Discard)
	for range 2 {
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.ReadLine(context.Background(), "closed"); !errors.Is(err, context.Canceled) {
		t.Fatal("关闭后不应重新启动终端会话")
	}
}

func TestTerminalPrompterRetainsRapidInputAcrossFields(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(master, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prompter := newTerminalPrompter(slave, slave)
	defer prompter.Close()

	type result struct {
		first  string
		second string
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		first, err := prompter.ReadLine(ctx, "first field")
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		second, err := prompter.ReadLine(ctx, "second field")
		resultCh <- result{first: first, second: second, err: err}
	}()

	output := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				output <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	waitPTYText := func(want string) bool {
		t.Helper()
		var text strings.Builder
		for {
			select {
			case chunk := <-output:
				text.WriteString(chunk)
				if strings.Contains(text.String(), want) {
					return true
				}
			case <-ctx.Done():
				t.Logf("等待终端文本 %q 超时，已收到 %q", want, text.String())
				return false
			}
		}
	}

	if !waitPTYText("first field") {
		t.Fatal("第一个字段未显示")
	}
	if _, err := master.Write([]byte("alpha\rbravo\r")); err != nil {
		t.Fatal(err)
	}
	// 已排队的第二字段可在下一帧渲染前完成，验证结果而非要求瞬时界面留屏。
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("快速连续输入失败：%v", got.err)
		}
		if got.first != "alpha" || got.second != "bravo" {
			t.Fatalf("跨字段输入错位：first=%q second=%q", got.first, got.second)
		}
	case <-ctx.Done():
		t.Fatal("第二字段输入未被保留")
	}
}
