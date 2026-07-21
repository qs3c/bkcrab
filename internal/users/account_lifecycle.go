package users

import (
	"errors"

	"github.com/qs3c/bkcrab/internal/store"
)

type lastActiveSuperAdminError struct {
	operation string
	cause     error
}

func (e *lastActiveSuperAdminError) Error() string {
	return "users." + e.operation + ": refusing to remove the last active super_admin"
}

func (e *lastActiveSuperAdminError) Unwrap() error { return e.cause }

// mapLastActiveSuperAdminError keeps the accounts layer's existing
// user-readable wording while retaining the store sentinel for errors.Is and
// logging/HTTP policy decisions.
func mapLastActiveSuperAdminError(operation string, err error) error {
	if !errors.Is(err, store.ErrLastActiveSuperAdmin) {
		return err
	}
	return &lastActiveSuperAdminError{operation: operation, cause: err}
}
