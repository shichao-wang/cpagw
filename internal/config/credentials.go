package config

import (
	"fmt"
	"strings"
)

// ValidateKey 防止将多行输入或控制字符作为认证头发送给上游。
func ValidateKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("API key 不能为空")
	}
	for _, r := range key {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("API key 不能包含空白或控制字符")
		}
	}
	return nil
}
