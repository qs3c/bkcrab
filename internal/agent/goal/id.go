package goal

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewID returns a fresh opaque identifier for a goal row. Random bytes
// rather than a time-prefixed format because goals are rare per session
// (one at a time) and a time prefix would leak the creation moment.
func NewID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failures are exotic enough that panicking is the
		// honest response — a non-unique fallback would be worse.
		panic(fmt.Sprintf("goal: crypto/rand failed: %v", err))
	}
	return "g-" + hex.EncodeToString(buf[:])
}
