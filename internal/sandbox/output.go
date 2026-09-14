package sandbox

import "bytes"

// Keep an untrusted command from exhausting the gateway heap via stdout/stderr.
// Return the full write length to keep draining the pipe without blocking it.
type commandOutput struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *commandOutput) Len() int { return b.buffer.Len() }
func (b *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, err := b.buffer.Write(p)
	return n, err
}
func (b *commandOutput) String() string {
	if b.truncated {
		return b.buffer.String() + "\n[Output truncated at 1 MiB; write large output to a workspace file.]"
	}
	return b.buffer.String()
}
