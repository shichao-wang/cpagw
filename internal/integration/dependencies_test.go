package integration

// 将 CLI 及其依赖纳入测试缓存指纹，避免 binary 源码变化后复用旧验收结果。
import _ "github.com/shichao-wang/cpagw/internal/cli"
