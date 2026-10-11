package config

import (
	"os"
	"testing"
)

func TestCallerContextIsPreservedByDefault(t *testing.T) {
	if os.Getenv("ZCODE_PRESERVE_CLIENT_CONTEXT") == "" && !PreserveClientContext {
		t.Fatal("未覆盖配置时应保留调用者的上下文")
	}
}
