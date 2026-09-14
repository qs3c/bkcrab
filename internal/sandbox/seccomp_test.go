package sandbox

import (
	"encoding/json"
	"testing"
)

func TestQuotaSeccompBlocksNativeCompatAndHighBits(t *testing.T) {
	var profile struct {
		Syscalls []struct {
			Names  []string
			Action string
			Args   []struct {
				Index           int
				Value, ValueTwo uint64
				Op              string
			}
		}
	}
	if err := json.Unmarshal(quotaSeccomp, &profile); err != nil {
		t.Fatal(err)
	}
	allowed := func(request uint64) bool {
		for _, r := range profile.Syscalls {
			for _, name := range r.Names {
				if name == "ioctl" && r.Action == "SCMP_ACT_ALLOW" {
					if len(r.Args) == 0 {
						t.Fatal("unconditional ioctl allow defeats quota")
					}
					match := true
					for _, a := range r.Args {
						if a.Index != 1 || a.Op != "SCMP_CMP_MASKED_EQ" {
							t.Fatal("unexpected ioctl rule")
						}
						match = match && (request&a.Value == a.ValueTwo)
					}
					if match {
						return true
					}
				}
			}
		}
		return false
	}
	for _, v := range []uint64{0x401c5820, 0x40086602, 0x40046602} {
		if allowed(v) || allowed(v|(1<<32)) {
			t.Fatalf("quota bypass ioctl allowed: %x", v)
		}
	}
	for _, v := range []uint64{0x5413, 0x5414, 0x5401, 0x80086601, 0x801c581f, 0, 0xffffffff} {
		if !allowed(v) {
			t.Fatalf("unrelated ioctl blocked: %x", v)
		}
	}
}
