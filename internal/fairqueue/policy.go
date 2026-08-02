package fairqueue

import "errors"

type CapacitySnapshot struct {
	GlobalInflight int
	TenantInflight int
	ActiveTenants  int
}

type ReservationDecision string

const (
	ReservationRegular           ReservationDecision = "regular"
	ReservationBorrowed          ReservationDecision = "borrowed"
	ReservationDeniedGlobalFull  ReservationDecision = "denied-global-full"
	ReservationDeniedTenantBurst ReservationDecision = "denied-tenant-burst"
	ReservationDeniedCompetition ReservationDecision = "denied-competing-active-tenant"
	ReservationDeniedBorrowOff   ReservationDecision = "denied-borrow-disabled"
)

func DecideReservation(limits CapacityLimits, snapshot CapacitySnapshot) (ReservationDecision, error) {
	if err := limits.Validate(); err != nil {
		return "", err
	}
	if snapshot.GlobalInflight < 0 || snapshot.TenantInflight < 0 {
		return "", errors.New("fairqueue: inflight counts must be non-negative")
	}
	if snapshot.TenantInflight > snapshot.GlobalInflight {
		return "", errors.New("fairqueue: tenant inflight exceeds global inflight")
	}
	if snapshot.ActiveTenants <= 0 {
		return "", errors.New("fairqueue: active tenant count must include the current tenant")
	}
	if snapshot.GlobalInflight >= limits.GlobalConcurrency {
		return ReservationDeniedGlobalFull, nil
	}
	if snapshot.TenantInflight >= limits.PerUserBurstConcurrency {
		return ReservationDeniedTenantBurst, nil
	}
	if snapshot.TenantInflight < limits.PerUserBaseConcurrency {
		return ReservationRegular, nil
	}
	if !limits.BorrowEnabled {
		return ReservationDeniedBorrowOff, nil
	}
	if snapshot.ActiveTenants == 1 {
		return ReservationBorrowed, nil
	}
	return ReservationDeniedCompetition, nil
}
