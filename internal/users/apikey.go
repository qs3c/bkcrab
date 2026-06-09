package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// APIKey type tiers. Set on the apikey at create time; immutable.
//
//   - APIKeyTypeAdmin: platform-wide authority — only super_admin owners
//     may issue this. Bypasses agent ACLs and unlocks /api/admin/* routes.
//   - APIKeyTypeUser:  scoped to the owning user's resources. Can create
//     agents (which then belong to the owner) and access any agent the
//     owner already owns. Apikey_agents is ignored for this tier.
//   - APIKeyTypeAgent: locked to an explicit list of agents (apikey_agents).
//     Cannot create agents. Use this for "give a downstream app keys to N
//     specific agents" scenarios.
const (
	APIKeyTypeAdmin = "admin"
	APIKeyTypeUser  = "user"
	APIKeyTypeAgent = "agent"
)

// IsAPIKeyType reports whether s is one of the canonical tier strings.
func IsAPIKeyType(s string) bool {
	return s == APIKeyTypeAdmin || s == APIKeyTypeUser || s == APIKeyTypeAgent
}

// APIKey is the public representation of an apikey row. Key holds the
// masked display string ("fc_xxxx****") on list responses, and the freshly
// issued plaintext token on create/rotate. The hash is never returned.
type APIKey struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	Name      string    `json:"name,omitempty"`
	Key       string    `json:"key"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
}

// Resolved is what the auth middleware needs to authorize a request: the
// apikey, its owning user, and the agents this key may operate on. Fetched
// in one shot from LookupByToken so the hot path stays a single round-trip.
//
// For type=user keys, Agents is populated with every agent owned by the
// apikey owner at resolve time (a fresh agent created mid-request won't
// appear until the next request). For type=agent it's the explicit ACL
// list. For type=admin it's empty — the auth gate short-circuits on type.
type Resolved struct {
	APIKey  APIKey
	Account Account
	Agents  []string
}

// APIKeys is the registry for programmatic credentials.
type APIKeys struct {
	store store.Store
}

// NewAPIKeys returns an apikey registry backed by st.
func NewAPIKeys(st store.Store) (*APIKeys, error) {
	if st == nil {
		return nil, errors.New("users.NewAPIKeys: store is required")
	}
	return &APIKeys{store: st}, nil
}

// Create issues a new apikey for userID. The plaintext token is returned
// once and never recoverable.
//
// keyType must be one of APIKeyTypeAdmin/User/Agent. agentIDs is honored
// only for type=agent (ignored otherwise — the user/admin tiers derive
// their scope from the owner's ownership rather than an explicit list).
// For type=agent the list must be non-empty; an agent-tier key with no
// reachable agents would be unusable. Caller is responsible for the
// role-vs-type policy check (handlers_admin.go enforces "only super_admin
// may issue type=admin", etc.).
func (k *APIKeys) Create(ctx context.Context, userID, name, keyType string, agentIDs []string) (*APIKey, string, error) {
	if userID == "" {
		return nil, "", errors.New("users.APIKeys.Create: userID is required")
	}
	if keyType == "" {
		keyType = APIKeyTypeAgent
	}
	if !IsAPIKeyType(keyType) {
		return nil, "", errors.New("users.APIKeys.Create: invalid type (want admin|user|agent)")
	}
	if keyType == APIKeyTypeAgent && len(agentIDs) == 0 {
		return nil, "", errors.New("users.APIKeys.Create: type=agent requires at least one agent")
	}
	id, err := newID("k_")
	if err != nil {
		return nil, "", err
	}
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}
	rec := &store.APIKeyRecord{
		ID:        id,
		UserID:    userID,
		Name:      name,
		KeyHash:   hashToken(token),
		KeyPrefix: keyPrefix(token),
		Type:      keyType,
		CreatedAt: time.Now().UTC(),
	}
	if err := k.store.CreateAPIKey(ctx, rec); err != nil {
		return nil, "", err
	}
	if keyType == APIKeyTypeAgent {
		if err := k.store.SetAPIKeyAgents(ctx, id, agentIDs); err != nil {
			return nil, "", err
		}
	}
	out := toAPIKey(rec)
	out.Key = token
	return out, token, nil
}

// Rotate replaces the apikey's token. Old token stops working immediately.
func (k *APIKeys) Rotate(ctx context.Context, id string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := k.store.RotateAPIKey(ctx, id, hashToken(token), keyPrefix(token)); err != nil {
		return "", err
	}
	return token, nil
}

func (k *APIKeys) Delete(ctx context.Context, id string) error {
	return k.store.DeleteAPIKey(ctx, id)
}

func (k *APIKeys) Get(ctx context.Context, id string) (*APIKey, error) {
	rec, err := k.store.GetAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return toAPIKey(rec), nil
}

// List returns every apikey owned by userID, with masked Key fields.
func (k *APIKeys) List(ctx context.Context, userID string) ([]*APIKey, error) {
	recs, err := k.store.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*APIKey, 0, len(recs))
	for i := range recs {
		out = append(out, toAPIKey(&recs[i]))
	}
	return out, nil
}

// Agents returns the apikey's agent access list.
func (k *APIKeys) Agents(ctx context.Context, apikeyID string) ([]string, error) {
	return k.store.ListAPIKeyAgents(ctx, apikeyID)
}

// SetAgents replaces the apikey's agent access list. Only meaningful for
// type=agent — the admin/user tiers derive scope from the owner, not from
// apikey_agents, so editing the list there would silently no-op at auth
// time. Reject those calls so callers don't mistake "set succeeded" for
// "scope changed".
func (k *APIKeys) SetAgents(ctx context.Context, apikeyID string, agentIDs []string) error {
	rec, err := k.store.GetAPIKey(ctx, apikeyID)
	if err != nil {
		return err
	}
	if rec.Type != "" && rec.Type != APIKeyTypeAgent {
		return errors.New("users.APIKeys.SetAgents: agent list is only editable on type=agent keys")
	}
	if len(agentIDs) == 0 {
		return errors.New("users.APIKeys.SetAgents: at least one agent required")
	}
	return k.store.SetAPIKeyAgents(ctx, apikeyID, agentIDs)
}

// LookupByToken is the auth hot path. SHA256(token) → (apikey, account,
// access list). Returns ErrInvalidCredentials for any failure mode so the
// middleware can't distinguish "unknown" from "disabled".
//
// For type=agent we read the explicit apikey_agents ACL. For type=user
// we substitute the owner's full agent list — newly created agents are
// auto-included on the next request without any ACL maintenance. For
// type=admin we skip the list entirely; the agent gate short-circuits on
// type before consulting it.
func (k *APIKeys) LookupByToken(ctx context.Context, token string) (*Resolved, error) {
	if token == "" {
		return nil, ErrInvalidCredentials
	}
	rec, err := k.store.LookupAPIKeyByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	user, err := k.store.GetUser(ctx, rec.UserID)
	if err != nil {
		// Orphaned apikey (user deleted but apikey lingered). Treat as
		// invalid — the cascade should have caught this.
		return nil, ErrInvalidCredentials
	}
	if user.Status != StatusActive {
		return nil, ErrInvalidCredentials
	}
	var agents []string
	switch rec.Type {
	case APIKeyTypeAdmin:
		// Admin keys bypass the per-agent gate entirely; leave empty.
	case APIKeyTypeUser:
		// All agents owned by the apikey owner. A second list per
		// request is the price of "no ACL maintenance for new agents".
		ags, err := k.store.ListAgents(ctx, rec.UserID)
		if err != nil {
			return nil, err
		}
		agents = make([]string, 0, len(ags))
		for _, a := range ags {
			agents = append(agents, a.ID)
		}
	default:
		// type=agent (and any legacy/unknown value) → explicit ACL.
		agents, err = k.store.ListAPIKeyAgents(ctx, rec.ID)
		if err != nil {
			return nil, err
		}
	}
	return &Resolved{
		APIKey:  *toAPIKey(rec),
		Account: *toAccount(user),
		Agents:  agents,
	}, nil
}

// CanAccessAgent answers "may this apikey operate on agentID?"
func (k *APIKeys) CanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error) {
	return k.store.APIKeyCanAccessAgent(ctx, apikeyID, agentID)
}

func toAPIKey(rec *store.APIKeyRecord) *APIKey {
	if rec == nil {
		return nil
	}
	masked := rec.KeyPrefix
	if masked == "" {
		masked = "fc_********"
	} else {
		masked = masked + "****"
	}
	return &APIKey{
		ID:        rec.ID,
		UserID:    rec.UserID,
		Name:      rec.Name,
		Key:       masked,
		Type:      rec.Type,
		CreatedAt: rec.CreatedAt,
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// keyPrefix keeps a recognizable slice of the plaintext for UI display.
// 10 chars is enough to spot "your" key in a list while staying far below
// brute-force feasibility.
func keyPrefix(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:10]
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "fc_" + hex.EncodeToString(buf[:]), nil
}
