package integration

import (
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmptyBinaryStartsFailClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过子进程验收")
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	dir := filepath.Join(tmp, "state")
	call := func(args ...string) (string, error) {
		out, err := exec.Command(binary, append([]string{"--state-dir", dir}, args...)...).CombinedOutput()
		return string(out), err
	}
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := socket.Addr().String()
	socket.Close()
	if out, err := call("server", "start", "--listen", address); err != nil {
		t.Fatalf("空配置必须允许启动：%v %s", err, out)
	}
	t.Cleanup(func() { _, _ = call("server", "stop") })
	if _, err := call("server", "start"); err == nil {
		t.Fatal("重复启动必须拒绝")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	for path, want := range map[string]int{"/v1/models": 401, "/__cpagw/ready": 404, "/v8/management/config": 404} {
		req, err := http.NewRequest("GET", "http://"+address+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", "not-a-profile-key")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("%s = %d，预期 %d", path, response.StatusCode, want)
		}
	}
	if out, err := call("server", "stop"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := call("server", "status"); err != nil || !strings.Contains(out, `"state":"stopped"`) {
		t.Fatalf("%v %s", err, out)
	}
}
