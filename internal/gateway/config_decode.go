package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/shichao-wang/cpa-tui/internal/config"
)

// decodeModels 填充公开 key 类型的模型字段，不导入未被 SDK 导出的内部模型类型。
func decodeModels(entry any, models []config.Model) error {
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entries = append(entries, map[string]any{"name": m.ID, "alias": m.ID, "display-name": m.Name, "force-mapping": true})
	}
	data, err := json.Marshal(map[string]any{"models": entries})
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, entry); err != nil {
		return fmt.Errorf("编译上游模型配置失败")
	}
	return nil
}
func processStartTime(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("无法读取进程身份")
	}
	result := strings.TrimSpace(string(data))
	if result == "" {
		return "", fmt.Errorf("进程身份为空")
	}
	return result, nil
}
