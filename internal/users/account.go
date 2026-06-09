// Package users owns the platform's identity layer: real user accounts
// (Account) and the programmatic tokens they issue (APIKey). Both types are
// thin facades over store.Store so a single SQL backend remains the source
// of truth across pods.
//
// The legacy "apikey == user" model is gone. An account is what owns
// agents/sessions/cron jobs; an apikey is just a scoped credential pointing
// at one account, with an explicit list of agents it may operate on.
package users

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Roles. super_admin can manage every user/agent/provider on the platform;
// user can only touch their own resources. app_user is provisioned by an
// api_key on behalf of a downstream application — these accounts have no
// password and cannot log in via dashboard or password endpoints; they
// exist purely to give external end-users a stable fastclaw user_id so
// sessions / agent_files / scope=user configs partition cleanly per
// end-user. There is intentionally no fine-grained scheme — anything
// more complex lives in the apikey ACL layer.
const (
	RoleSuperAdmin = "super_admin"
	RoleUser       = "user"
	RoleAppUser    = "app_user"
)

// Statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// ErrInvalidCredentials masks "no such user" and "wrong password" so the
// login handler can't be used as an email-existence oracle.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Account is the public representation of a user row. PasswordHash never
// leaves the package — we read it during Authenticate and zero it out
// before returning to callers.
type Account struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName,omitempty"`
	Role        string    `json:"role"`
	Status      string    `json:"status"`
	APIKeyID    string    `json:"apikeyId,omitempty"`
	ExternalID  string    `json:"externalId,omitempty"`
	AvatarURL   string    `json:"avatarUrl,omitempty"`
	AgentQuota  int64     `json:"agentQuota"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Accounts is the registry for user accounts.
type Accounts struct {
	store store.Store
}

// NewAccounts returns an account registry backed by st. Refuses nil — the
// platform has no in-memory mode.
func NewAccounts(st store.Store) (*Accounts, error) {
	if st == nil {
		return nil, errors.New("users.NewAccounts: store is required")
	}
	return &Accounts{store: st}, nil
}

// Count returns the number of users on the platform. Onboarding gates on
// `Count(ctx) == 0` to decide whether to show the wizard.
func (a *Accounts) Count(ctx context.Context) (int, error) {
	return a.store.CountUsers(ctx)
}

// CreateInput is the bag of fields Create writes onto a new user row.
// Required: Username, Email, Password. Role defaults to RoleUser.
//
// AgentQuota:
//   - nil           — unlimited (platform default for self-registered users)
//   - *value < 0    — unlimited
//   - *value = 0    — caller cannot self-create agents (admin provisions only)
//   - *value > 0    — caller can hold up to N owned agents
//
// APIKeyID + ExternalID are the upstream-provisioning idempotency pair.
// Set APIKeyID to the apikey that's minting this row (handler reads it
// from auth.Identity, never from the request body) so the row is
// auditable back to the provisioning key. Set ExternalID to the calling
// app's own user identifier; the partial UNIQUE index on (apikey_id,
// external_id) — see migrateUsersAppUserCols — means the same pair
// always resolves to the same fastclaw user_id, so retries are safe.
//
// AvatarURL must be empty or a `data:image/*` URL ≤256KB; the handler
// caller is responsible for that validation.
type CreateInput struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
	Role        string
	AgentQuota  *int64
	AvatarURL   string
	APIKeyID    string
	ExternalID  string
}

// Create writes a new account. Password is hashed with bcrypt; plaintext
// is never persisted. ID is always auto-generated.
//
// Idempotent on (APIKeyID, ExternalID): when both are non-empty, a repeat
// call returns the already-provisioned row instead of erroring on the
// partial UNIQUE index. Upstream apps can re-issue the same provisioning
// call without tracking whether they've called us before. username/email
// UNIQUE collisions across *different* identities still surface as errors —
// silently returning a stranger's row would hide a real conflict.
func (a *Accounts) Create(ctx context.Context, in CreateInput) (*Account, error) {
	apikeyID := strings.TrimSpace(in.APIKeyID)
	externalID := strings.TrimSpace(in.ExternalID)
	// Fast path — already provisioned for this (apikey, external_id) pair.
	if apikeyID != "" && externalID != "" {
		if rec, err := a.store.GetUserByExternal(ctx, apikeyID, externalID); err == nil {
			return toAccount(rec), nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	username := strings.TrimSpace(in.Username)
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if username == "" || email == "" || in.Password == "" {
		return nil, errors.New("users.Create: username, email, password are required")
	}
	role := in.Role
	if role == "" {
		role = RoleUser
	}
	if role != RoleSuperAdmin && role != RoleUser {
		return nil, errors.New("users.Create: invalid role")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	quota := int64(-1)
	if in.AgentQuota != nil {
		quota = *in.AgentQuota
	}
	rec := &store.UserRecord{
		ID:           id,
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  in.DisplayName,
		Role:         role,
		Status:       StatusActive,
		APIKeyID:     apikeyID,
		ExternalID:   externalID,
		AvatarURL:    in.AvatarURL,
		AgentQuota:   quota,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		// Race: another concurrent request minted the same
		// (apikey_id, external_id) pair between our fast-path miss
		// above and the INSERT. Re-read and return that row so the
		// caller sees the same idempotent contract regardless of
		// timing. username/email collisions across different
		// identities still bubble — see EnsureAppUser for the same
		// pattern.
		if apikeyID != "" && externalID != "" {
			if again, qerr := a.store.GetUserByExternal(ctx, apikeyID, externalID); qerr == nil {
				return toAccount(again), nil
			}
		}
		return nil, err
	}
	return toAccount(rec), nil
}

// Authenticate validates a username-or-email + password pair. Returns the
// account on success, ErrInvalidCredentials on every failure mode (missing
// user, wrong password, disabled account) so callers can't distinguish.
func (a *Accounts) Authenticate(ctx context.Context, login, password string) (*Account, error) {
	login = strings.TrimSpace(login)
	if login == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if strings.Contains(login, "@") {
		login = strings.ToLower(login)
	}
	rec, err := a.store.GetUserByLogin(ctx, login)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if rec.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	// app_user accounts (and any other row provisioned without a real
	// password) carry an empty hash. bcrypt.CompareHashAndPassword
	// would still fail-closed, but checking explicitly keeps the
	// failure mode unambiguous and avoids burning bcrypt cycles on
	// every probe.
	if rec.PasswordHash == "" || rec.Role == RoleAppUser {
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}
	return toAccount(rec), nil
}

// Get returns the account for id, or store.ErrNotFound.
func (a *Accounts) Get(ctx context.Context, id string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// List returns all accounts. Super-admin endpoints only.
func (a *Accounts) List(ctx context.Context) ([]*Account, error) {
	recs, err := a.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(recs))
	for i := range recs {
		out = append(out, toAccount(&recs[i]))
	}
	return out, nil
}

// Update applies non-credential changes (display name, role, status). Use
// SetPassword for password rotation.
func (a *Accounts) Update(ctx context.Context, id, displayName, role, status string, agentQuota *int64) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	if displayName != "" {
		rec.DisplayName = displayName
	}
	if role != "" {
		if role != RoleSuperAdmin && role != RoleUser {
			return nil, errors.New("users.Update: invalid role")
		}
		rec.Role = role
	}
	if status != "" {
		if status != StatusActive && status != StatusDisabled {
			return nil, errors.New("users.Update: invalid status")
		}
		rec.Status = status
	}
	if agentQuota != nil {
		rec.AgentQuota = *agentQuota
	}
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// UpdateProfile applies self-service edits — display name and avatar
// only. Role/status changes go through Update (admin-only). avatarURL
// is stored verbatim; the handler is responsible for shape and size
// validation. Pass an explicit empty string to clear the avatar.
func (a *Accounts) UpdateProfile(ctx context.Context, id, displayName, avatarURL string) (*Account, error) {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return nil, err
	}
	rec.DisplayName = displayName
	rec.AvatarURL = avatarURL
	if err := a.store.UpdateUser(ctx, rec); err != nil {
		return nil, err
	}
	return toAccount(rec), nil
}

// VerifyPassword checks a plaintext password against the stored hash for
// id. Returns ErrInvalidCredentials on mismatch (or for accounts with no
// password, e.g. app_user). Used by /api/me/password to gate self-service
// password change behind the current password.
func (a *Accounts) VerifyPassword(ctx context.Context, id, password string) error {
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return ErrInvalidCredentials
	}
	if rec.PasswordHash == "" {
		return ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// SetPassword rotates an account's password. Caller is responsible for
// permission checks (self vs. super_admin).
func (a *Accounts) SetPassword(ctx context.Context, id, newPassword string) error {
	if newPassword == "" {
		return errors.New("users.SetPassword: empty password")
	}
	rec, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	rec.PasswordHash = string(hash)
	return a.store.UpdateUser(ctx, rec)
}

// EnsureAppUser returns the fastclaw user representing (apikeyID, externalID),
// creating one with role=app_user the first time it's seen. Idempotent:
// later calls with the same pair return the existing row. The caller is
// expected to be the api_key owner — Mint does not authenticate, that's
// the auth middleware's job. Username/email are synthesized from the
// pair and namespaced ("ext:<apikeyID>:<externalID>") so they don't
// collide with real human signups but still satisfy the UNIQUE
// constraints on those columns.
func (a *Accounts) EnsureAppUser(ctx context.Context, apikeyID, externalID, displayName string) (*Account, error) {
	apikeyID = strings.TrimSpace(apikeyID)
	externalID = strings.TrimSpace(externalID)
	if apikeyID == "" || externalID == "" {
		return nil, errors.New("users.EnsureAppUser: apikeyID and externalID are required")
	}
	// Fast path — already provisioned.
	if rec, err := a.store.GetUserByExternal(ctx, apikeyID, externalID); err == nil {
		return toAccount(rec), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	id, err := newID("u_")
	if err != nil {
		return nil, err
	}
	// Synthesize unique username/email tokens. The downstream app
	// is the source of truth for the human-readable identity; we
	// only need *something* unique to satisfy the schema.
	syn := apikeyID + ":" + externalID
	rec := &store.UserRecord{
		ID:           id,
		Username:     "ext:" + syn,
		Email:        syn + "@external.fastclaw.local",
		PasswordHash: "",
		DisplayName:  displayName,
		Role:         RoleAppUser,
		Status:       StatusActive,
		APIKeyID:     apikeyID,
		ExternalID:   externalID,
		AgentQuota:   -1,
	}
	if err := a.store.CreateUser(ctx, rec); err != nil {
		// Race: another concurrent request minted the same pair
		// between our GetUserByExternal and CreateUser. Re-read
		// and return that row instead of bubbling the unique
		// violation up to the caller.
		if again, qerr := a.store.GetUserByExternal(ctx, apikeyID, externalID); qerr == nil {
			return toAccount(again), nil
		}
		return nil, err
	}
	return toAccount(rec), nil
}

// Delete removes an account and its owned rows (cascade implemented in the
// store). Refuses to drop the last super_admin so the install doesn't lock
// itself out.
func (a *Accounts) Delete(ctx context.Context, id string) error {
	target, err := a.store.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if target.Role == RoleSuperAdmin {
		all, err := a.store.ListUsers(ctx)
		if err != nil {
			return err
		}
		admins := 0
		for _, u := range all {
			if u.Role == RoleSuperAdmin && u.Status == StatusActive {
				admins++
			}
		}
		if admins <= 1 {
			return errors.New("users.Delete: refusing to remove the last active super_admin")
		}
	}
	return a.store.DeleteUser(ctx, id)
}

func toAccount(r *store.UserRecord) *Account {
	if r == nil {
		return nil
	}
	return &Account{
		ID:          r.ID,
		Username:    r.Username,
		Email:       r.Email,
		DisplayName: r.DisplayName,
		Role:        r.Role,
		Status:      r.Status,
		APIKeyID:    r.APIKeyID,
		ExternalID:  r.ExternalID,
		AvatarURL:   r.AvatarURL,
		AgentQuota:  r.AgentQuota,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// newID returns a short unique id with the given prefix. ~80 bits of
// entropy — collisions vanishingly unlikely at platform scale.
func newID(prefix string) (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}
