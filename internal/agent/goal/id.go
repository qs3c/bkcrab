package goal

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewID 返回目标行的新的不透明标识符。随机字节
// 而不是时间前缀格式，因为每次训练的目标很少见
// （一次一个）和时间前缀会泄漏创建时刻。
func NewID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 加密/兰特故障非常奇特，以至于令人恐慌
		// 诚实的回应——非唯一的后备会更糟糕。
		panic(fmt.Sprintf("goal: crypto/rand failed: %v", err))
	}
	return "g-" + hex.EncodeToString(buf[:])
}
