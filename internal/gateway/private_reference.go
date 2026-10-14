package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// 会话/trace 只使用本进程限时 HMAC。密钥不落盘、不发送给上游；跨进程/周期不可直接比对。
// 不复用明文 SHA，避免低熵调用者 ID 被离线枚举，也不修改业务重放键的既有语义。
var referenceKeys struct {
	sync.Mutex
	key     [32]byte
	expires time.Time
}

const referenceLifetime = time.Hour

func privateReference(value string, now time.Time) string {
	if value == "" {
		return ""
	}
	referenceKeys.Lock()
	if referenceKeys.expires.IsZero() || !now.Before(referenceKeys.expires) {
		_, _ = rand.Read(referenceKeys.key[:])
		referenceKeys.expires = now.Add(referenceLifetime)
	}
	key := referenceKeys.key
	referenceKeys.Unlock()
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
