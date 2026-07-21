package users

import (
	"errors"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func TestMapLastActiveSuperAdminError(t *testing.T) {
	tests := []struct {
		operation, message string
	}{
		{"Delete", "users.Delete: refusing to remove the last active super_admin"},
		{"Update", "users.Update: refusing to remove the last active super_admin"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			err := mapLastActiveSuperAdminError(test.operation, store.ErrLastActiveSuperAdmin)
			if err.Error() != test.message {
				t.Fatalf("mapped error = %q, want %q", err, test.message)
			}
			if !errors.Is(err, store.ErrLastActiveSuperAdmin) {
				t.Fatalf("mapped error lost store sentinel: %v", err)
			}
		})
	}

	original := errors.New("other")
	if got := mapLastActiveSuperAdminError("Delete", original); got != original {
		t.Fatalf("unrelated error was replaced: got %v, want %v", got, original)
	}
}
