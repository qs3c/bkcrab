package sandbox

import (
	"strings"
	"testing"
)

func TestCommandOutputBounded(t *testing.T) {
	var b commandOutput
	payload := []byte(strings.Repeat("x", 2<<20))
	n, err := b.Write(payload)
	if err != nil || n != len(payload) || b.Len() != 1<<20 || !strings.Contains(b.String(), "truncated") {
		t.Fatal("output not bounded")
	}
	b.Write(payload)
	if b.Len() != 1<<20 {
		t.Fatal("grew after limit")
	}
}
