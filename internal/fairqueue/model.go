// Package fairqueue defines domain-neutral scheduling contracts. Domain
// adapters retain ownership of canonical task state and opaque CAS guards.
package fairqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ProtocolVersion = 1
	MessageVersion1 = ProtocolVersion
	MaxMessageBytes = 64 << 10
	// MaxRawDeliveryBytes is deliberately larger than MaxMessageBytes so a
	// bounded, permanently invalid delivery can still be confirmed to the DLQ.
	MaxRawDeliveryBytes  = 1 << 20
	MaxRecoveryPageLimit = maxRecoveryPageSize
	maxOpaqueGuardBytes  = 64 << 10
	maxDecodeErrorBytes  = 128
	maxRedisKeyBytes     = 4096
)

var (
	ErrInvalidMessage         = errors.New("fairqueue: invalid message")
	ErrInvalidModel           = errors.New("fairqueue: invalid model")
	ErrInvalidPrepareResult   = errors.New("fairqueue: invalid prepare result")
	ErrInvalidRecoveryPage    = errors.New("fairqueue: invalid recovery page")
	ErrInvalidRecoveryState   = errors.New("fairqueue: invalid recovery state")
	ErrInvalidOperationRecord = errors.New("fairqueue: invalid recovery operation record")
	ErrOperatorConfirmation   = errors.New("fairqueue: required operator confirmation is missing")

	// Boundary implementations wrap these stable categories so runtimes can
	// fail closed without parsing transport, Redis, or Lua error strings.
	ErrDependencyUnavailable       = errors.New("fairqueue: dependency unavailable")
	ErrUnsupportedTopology         = errors.New("fairqueue: unsupported dependency topology")
	ErrResourceNotReady            = errors.New("fairqueue: resource is not ready")
	ErrFenceMismatch               = errors.New("fairqueue: resource fence mismatch")
	ErrAuthoritativeWriterMismatch = errors.New("fairqueue: authoritative writer identity mismatch")
	ErrAuthoritativeStateCorrupt   = errors.New("fairqueue: authoritative state is corrupt")
	ErrRecoveryOwnerStale          = errors.New("fairqueue: recovery owner is stale")
	ErrCoordinationCorrupt         = errors.New("fairqueue: corrupt coordination state")
	ErrPublishUnroutable           = errors.New("fairqueue: publish was unroutable")
	ErrPublishUnconfirmed          = errors.New("fairqueue: publish was not confirmed")

	lowerHex32Pattern        = regexp.MustCompile(`^[0-9a-f]{32}$`)
	lowerHex64Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stableReservationPattern = regexp.MustCompile(`^r:[0-9a-f]{64}$`)
	reasonCodePattern        = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
)

// DispatchToken is the durable logical delivery epoch. TaskID remains opaque
// here; registered resources apply their own canonical task-ID validator.
type DispatchToken struct {
	Resource   string `json:"resource"`
	TaskID     string `json:"task_id"`
	Generation uint64 `json:"generation"`
}

func (t DispatchToken) Validate() error {
	if err := ValidateResource(t.Resource); err != nil {
		return fmt.Errorf("%w: token resource: %v", ErrInvalidMessage, err)
	}
	if err := ValidateTaskID(t.TaskID); err != nil {
		return fmt.Errorf("%w: token task ID: %v", ErrInvalidMessage, err)
	}
	if t.Generation == 0 || t.Generation > math.MaxInt64 {
		return fmt.Errorf("%w: token generation must be in 1..MaxInt64", ErrInvalidMessage)
	}
	return nil
}

// Message is the complete v1 wire identity. It intentionally contains no
// domain payload or database guard.
type Message struct {
	Version       int           `json:"version"`
	Resource      string        `json:"resource"`
	TenantID      string        `json:"tenant_id"`
	TaskType      string        `json:"task_type"`
	TaskID        string        `json:"task_id"`
	DispatchToken DispatchToken `json:"dispatch_token"`
}

func (m Message) Validate() error {
	if m.Version != MessageVersion1 {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidMessage, m.Version)
	}
	if err := ValidateResource(m.Resource); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := ValidateTenantID(m.TenantID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := ValidateTaskType(m.TaskType); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := ValidateTaskID(m.TaskID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := m.DispatchToken.Validate(); err != nil {
		return err
	}
	if m.DispatchToken.Resource != m.Resource || m.DispatchToken.TaskID != m.TaskID {
		return fmt.Errorf("%w: dispatch token does not match message identity", ErrInvalidMessage)
	}
	return nil
}

type messageWire Message

func (m Message) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(messageWire(m))
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	if m == nil {
		return fmt.Errorf("%w: nil destination", ErrInvalidMessage)
	}
	decoded, err := StrictDecodeMessage(raw)
	if err != nil {
		return err
	}
	*m = decoded
	return nil
}

// StrictDecodeMessage rejects unknown and duplicate fields, trailing JSON, and
// oversized bodies before accepting a v1 identity.
func StrictDecodeMessage(raw []byte) (Message, error) {
	if len(raw) == 0 || len(raw) > MaxMessageBytes {
		return Message{}, fmt.Errorf("%w: body size must be in 1..%d bytes", ErrInvalidMessage, MaxMessageBytes)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := validateExactMessageJSONFields(raw); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire messageWire
	if err := decoder.Decode(&wire); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	message := Message(wire)
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}

func validateExactMessageJSONFields(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	rootFields := map[string]struct{}{
		"version": {}, "resource": {}, "tenant_id": {}, "task_type": {},
		"task_id": {}, "dispatch_token": {},
	}
	for field := range root {
		if _, ok := rootFields[field]; !ok {
			return fmt.Errorf("unknown exact JSON field %q", field)
		}
	}
	tokenRaw, ok := root["dispatch_token"]
	if !ok {
		return nil
	}
	var token map[string]json.RawMessage
	if err := json.Unmarshal(tokenRaw, &token); err != nil {
		return err
	}
	tokenFields := map[string]struct{}{
		"resource": {}, "task_id": {}, "generation": {},
	}
	for field := range token {
		if _, ok := tokenFields[field]; !ok {
			return fmt.Errorf("unknown exact dispatch token JSON field %q", field)
		}
	}
	return nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkUniqueJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func walkUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := walkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := walkUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// DispatchCandidate binds a wire message to an opaque domain CAS snapshot.
// Guard never crosses the Rabbit boundary.
type DispatchCandidate struct {
	Message Message `json:"message"`
	Guard   string  `json:"-"`
}

// StableHeaderFacts preserves each independently type-checked AMQP header.
// A repair-only token can remain usable when the protocol-version header is
// missing or invalid; only a complete v1 set can authorize execution.
type StableHeaderFacts struct {
	ProtocolVersion    *int32  `json:"protocol_version,omitempty"`
	Resource           *string `json:"resource,omitempty"`
	TaskID             *string `json:"task_id,omitempty"`
	DispatchGeneration *int64  `json:"dispatch_generation,omitempty"`
}

func (f StableHeaderFacts) Validate() error {
	if f.Resource != nil {
		if err := validateBoundedText("header resource", *f.Resource, maxResourceBytes, false); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidModel, err)
		}
	}
	if f.TaskID != nil {
		if err := validateBoundedText("header task ID", *f.TaskID, maxTaskIDBytes, false); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidModel, err)
		}
	}
	return nil
}

func (f StableHeaderFacts) Token() (DispatchToken, bool) {
	if f.Resource == nil || f.TaskID == nil || f.DispatchGeneration == nil || *f.DispatchGeneration <= 0 {
		return DispatchToken{}, false
	}
	token := DispatchToken{
		Resource: *f.Resource, TaskID: *f.TaskID, Generation: uint64(*f.DispatchGeneration),
	}
	return token, token.Validate() == nil
}

func (f StableHeaderFacts) MatchesToken(token DispatchToken) bool {
	parsed, ok := f.Token()
	return ok && parsed == token
}

func (f StableHeaderFacts) CompleteV1Token() (DispatchToken, bool) {
	if f.ProtocolVersion == nil || *f.ProtocolVersion != int32(MessageVersion1) {
		return DispatchToken{}, false
	}
	return f.Token()
}

func (c DispatchCandidate) Validate() error {
	if err := c.Message.Validate(); err != nil {
		return err
	}
	if c.Guard == "" || len(c.Guard) > maxOpaqueGuardBytes || !utf8.ValidString(c.Guard) {
		return fmt.Errorf("%w: opaque guard must be non-empty bounded UTF-8", ErrInvalidModel)
	}
	return nil
}

// PrepareRequest preserves independently validated transport facts. Message is
// non-nil only when body, headers, registered resource, and queue context agree.
type PrepareRequest struct {
	Message            *Message          `json:"-"`
	BodyCandidate      *Message          `json:"-"`
	HeaderToken        *DispatchToken    `json:"-"`
	HeaderFacts        StableHeaderFacts `json:"-"`
	RegisteredResource string            `json:"registered_resource"`
	QueueTenantHash    string            `json:"queue_tenant_hash"`
	PublishAttemptID   string            `json:"publish_attempt_id"`
	RawBody            []byte            `json:"-"`
	RawBodyOmitted     bool              `json:"raw_body_omitted"`
	RawBodySize        int64             `json:"raw_body_size"`
	RawBodySHA256      string            `json:"raw_body_sha256"`
	DecodeErrorCode    string            `json:"decode_error_code"`
	HeaderErrorCode    string            `json:"header_error_code"`
	PropertyErrorCode  string            `json:"property_error_code"`
}

func (r PrepareRequest) Validate() error {
	if err := ValidateResource(r.RegisteredResource); err != nil {
		return fmt.Errorf("%w: registered resource: %v", ErrInvalidModel, err)
	}
	if !lowerHex64Pattern.MatchString(r.QueueTenantHash) {
		return fmt.Errorf("%w: queue tenant hash must be 64 lowercase hex characters", ErrInvalidModel)
	}
	if len(r.RawBody) > MaxRawDeliveryBytes {
		return fmt.Errorf("%w: raw body exceeds transport limit", ErrInvalidModel)
	}
	if r.RawBodyOmitted {
		if len(r.RawBody) != 0 || r.RawBodySize <= MaxRawDeliveryBytes ||
			!lowerHex64Pattern.MatchString(r.RawBodySHA256) || r.Message != nil || r.BodyCandidate != nil ||
			r.DecodeErrorCode == "" {
			return fmt.Errorf("%w: invalid omitted raw body metadata", ErrInvalidModel)
		}
	} else if r.RawBodySize != 0 || r.RawBodySHA256 != "" {
		return fmt.Errorf("%w: retained raw body carries omission metadata", ErrInvalidModel)
	}
	if r.PublishAttemptID != "" && !lowerHex32Pattern.MatchString(r.PublishAttemptID) {
		return fmt.Errorf("%w: publish attempt ID must be empty or 128-bit lowercase hex", ErrInvalidModel)
	}
	if err := validateOptionalReasonCode("decode error code", r.DecodeErrorCode); err != nil {
		return err
	}
	if err := validateOptionalReasonCode("header error code", r.HeaderErrorCode); err != nil {
		return err
	}
	if err := validateOptionalReasonCode("property error code", r.PropertyErrorCode); err != nil {
		return err
	}
	if err := r.HeaderFacts.Validate(); err != nil {
		return err
	}
	if r.BodyCandidate != nil {
		if err := r.BodyCandidate.Validate(); err != nil {
			return err
		}
		if r.RawBodyOmitted || len(r.RawBody) == 0 || len(r.RawBody) > MaxMessageBytes || r.DecodeErrorCode != "" {
			return fmt.Errorf("%w: body candidate requires a valid protocol-sized raw body", ErrInvalidModel)
		}
		decodedBody, err := StrictDecodeMessage(r.RawBody)
		if err != nil || !reflectMessageIdentity(decodedBody, *r.BodyCandidate) {
			return fmt.Errorf("%w: raw body does not match body candidate", ErrInvalidModel)
		}
	} else if r.DecodeErrorCode == "" {
		return fmt.Errorf("%w: missing body candidate requires a body error code", ErrInvalidModel)
	}
	headerToken, hasHeaderToken := r.HeaderFacts.Token()
	if r.HeaderToken != nil {
		if err := r.HeaderToken.Validate(); err != nil {
			return err
		}
		if !hasHeaderToken || headerToken != *r.HeaderToken {
			return fmt.Errorf("%w: stable header facts do not match token", ErrInvalidModel)
		}
		_, completeHeaders := r.HeaderFacts.CompleteV1Token()
		if completeHeaders != (r.HeaderErrorCode == "") {
			return fmt.Errorf("%w: header error code does not match parsed header facts", ErrInvalidModel)
		}
	} else if r.HeaderErrorCode == "" {
		return fmt.Errorf("%w: missing header token requires a header error code", ErrInvalidModel)
	}
	if (r.PublishAttemptID != "") != (r.PropertyErrorCode == "") {
		return fmt.Errorf("%w: property error code does not match publish attempt facts", ErrInvalidModel)
	}
	completeHeaderToken, completeHeaders := r.HeaderFacts.CompleteV1Token()
	canExecute := r.BodyCandidate != nil && r.HeaderToken != nil && completeHeaders &&
		completeHeaderToken == *r.HeaderToken && lowerHex32Pattern.MatchString(r.PublishAttemptID) &&
		r.DecodeErrorCode == "" && r.HeaderErrorCode == "" && r.PropertyErrorCode == "" &&
		r.BodyCandidate.DispatchToken == *r.HeaderToken && r.BodyCandidate.Resource == r.RegisteredResource
	if canExecute {
		tenantHash, err := TenantHash(r.BodyCandidate.Resource, r.BodyCandidate.TenantID)
		canExecute = err == nil && tenantHash == r.QueueTenantHash
	}
	if r.Message == nil {
		if canExecute {
			return fmt.Errorf("%w: canonical transport facts require an executable message", ErrInvalidModel)
		}
		return nil
	}
	if !canExecute {
		return fmt.Errorf("%w: executable message requires canonical transport facts", ErrInvalidModel)
	}
	decodedBody, err := StrictDecodeMessage(r.RawBody)
	if err != nil || !reflectMessageIdentity(decodedBody, *r.BodyCandidate) {
		return fmt.Errorf("%w: executable raw body does not match body candidate", ErrInvalidModel)
	}
	if err := r.Message.Validate(); err != nil {
		return err
	}
	if !reflectMessageIdentity(*r.Message, *r.BodyCandidate) || r.Message.DispatchToken != *r.HeaderToken ||
		r.Message.Resource != r.RegisteredResource {
		return fmt.Errorf("%w: executable transport identities do not agree", ErrInvalidModel)
	}
	return nil
}

func reflectMessageIdentity(left, right Message) bool {
	return left == right
}

func validateOptionalReasonCode(name, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxDecodeErrorBytes || !reasonCodePattern.MatchString(value) {
		return fmt.Errorf("%w: %s must be canonical bounded ASCII", ErrInvalidModel, name)
	}
	return nil
}

type PrepareDisposition string

const (
	PrepareClaimed                       PrepareDisposition = "claimed"
	PrepareCapacityDeferred              PrepareDisposition = "capacity-deferred"
	PrepareTransientInfrastructure       PrepareDisposition = "transient-infrastructure"
	PrepareDuplicateStaleTerminal        PrepareDisposition = "duplicate-stale-terminal"
	PrepareCanonicalRepairedTerminal     PrepareDisposition = "canonical-repaired-terminal"
	PrepareCanonicalRepairedRetry        PrepareDisposition = "canonical-repaired-retry"
	PreparePoisonPermanentInvalidMessage PrepareDisposition = "poison-permanent-invalid-message"
)

type DeliveryAction string

const (
	DeliveryPromoteThenAckRun DeliveryAction = "promote-then-ack-run"
	DeliveryAckRelease        DeliveryAction = "ack-release"
	DeliveryNackRequeue       DeliveryAction = "nack-requeue"
	DeliveryConfirmDLQThenAck DeliveryAction = "confirm-dlq-then-ack"
)

type CanonicalEffect string

const (
	CanonicalNone                    CanonicalEffect = "none"
	CanonicalClaimCommitted          CanonicalEffect = "claim-committed"
	CanonicalTerminalRepairCommitted CanonicalEffect = "terminal-repair-committed"
	CanonicalRetryRepairCommitted    CanonicalEffect = "retry-repair-committed"
	CanonicalPoisonRepairSettled     CanonicalEffect = "poison-repair-settled"
)

type ClaimRef struct {
	TenantID        string `json:"tenant_id"`
	TaskID          string `json:"task_id"`
	ClaimGeneration uint64 `json:"claim_generation"`
}

func (r ClaimRef) Validate() error {
	if err := ValidateTenantID(r.TenantID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if r.ClaimGeneration == 0 || r.ClaimGeneration > math.MaxInt64 {
		return fmt.Errorf("%w: claim generation must be in 1..MaxInt64", ErrInvalidModel)
	}
	return nil
}

// PrepareResult separates the delivery action from the canonical database
// outcome. A claimed result is the only result that may carry runnable work.
type PrepareResult struct {
	Disposition     PrepareDisposition `json:"disposition"`
	DeliveryAction  DeliveryAction     `json:"delivery_action"`
	CanonicalEffect CanonicalEffect    `json:"canonical_effect"`
	Claim           *ClaimRef          `json:"claim"`
}

func (r PrepareResult) Validate(prepared PreparedTask) error {
	wantAction, wantEffect, known := prepareResultMatrix(r.Disposition)
	if !known || r.DeliveryAction != wantAction {
		return ErrInvalidPrepareResult
	}
	if r.Disposition == PreparePoisonPermanentInvalidMessage {
		if r.CanonicalEffect != CanonicalNone && r.CanonicalEffect != CanonicalPoisonRepairSettled {
			return ErrInvalidPrepareResult
		}
	} else if r.CanonicalEffect != wantEffect {
		return ErrInvalidPrepareResult
	}
	if r.Disposition == PrepareClaimed {
		if prepared == nil || r.Claim == nil || r.Claim.Validate() != nil {
			return ErrInvalidPrepareResult
		}
		return nil
	}
	if prepared != nil || r.Claim != nil {
		return ErrInvalidPrepareResult
	}
	return nil
}

// ValidateFor binds a claimed result to the independently validated delivery
// request. Runtime code must use this form before promoting runnable work.
func (r PrepareResult) ValidateFor(request PrepareRequest, prepared PreparedTask) error {
	if err := request.Validate(); err != nil {
		return fmt.Errorf("%w: invalid prepare request: %v", ErrInvalidPrepareResult, err)
	}
	if err := r.Validate(prepared); err != nil {
		return err
	}
	// A repair-only transport envelope can never be silently ACKed or merely
	// requeued via an executable-message disposition. Its domain adapter must
	// first classify/repair every bounded locator and then route the original
	// delivery through the explicit confirmed-DLQ action.
	if request.Message == nil && r.Disposition != PreparePoisonPermanentInvalidMessage {
		return ErrInvalidPrepareResult
	}
	if r.Disposition != PrepareClaimed {
		return nil
	}
	if request.Message == nil || r.Claim == nil ||
		r.Claim.TenantID != request.Message.TenantID ||
		r.Claim.TaskID != request.Message.TaskID ||
		r.Claim.ClaimGeneration != request.Message.DispatchToken.Generation {
		return ErrInvalidPrepareResult
	}
	return nil
}

func prepareResultMatrix(disposition PrepareDisposition) (DeliveryAction, CanonicalEffect, bool) {
	switch disposition {
	case PrepareClaimed:
		return DeliveryPromoteThenAckRun, CanonicalClaimCommitted, true
	case PrepareCapacityDeferred, PrepareTransientInfrastructure:
		return DeliveryNackRequeue, CanonicalNone, true
	case PrepareDuplicateStaleTerminal:
		return DeliveryAckRelease, CanonicalNone, true
	case PrepareCanonicalRepairedTerminal:
		return DeliveryAckRelease, CanonicalTerminalRepairCommitted, true
	case PrepareCanonicalRepairedRetry:
		return DeliveryAckRelease, CanonicalRetryRepairCommitted, true
	case PreparePoisonPermanentInvalidMessage:
		return DeliveryConfirmDLQThenAck, CanonicalPoisonRepairSettled, true
	default:
		return "", "", false
	}
}

type RecoveryPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
	Done       bool   `json:"done"`
}

// Validate enforces bounded opaque keyset pagination. Empty intermediate pages
// are valid as long as their cursor advances.
func (p RecoveryPage[T]) Validate(after string, limit int) error {
	if err := ValidatePageLimit(limit); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecoveryPage, err)
	}
	if err := ValidateCursor(after); err != nil {
		return fmt.Errorf("%w: after cursor: %v", ErrInvalidRecoveryPage, err)
	}
	if err := ValidateCursor(p.NextCursor); err != nil {
		return fmt.Errorf("%w: next cursor: %v", ErrInvalidRecoveryPage, err)
	}
	if len(p.Items) > limit {
		return fmt.Errorf("%w: page contains %d items for limit %d", ErrInvalidRecoveryPage, len(p.Items), limit)
	}
	for index, item := range p.Items {
		validator, ok := any(item).(interface{ Validate() error })
		if !ok {
			return fmt.Errorf("%w: item %d has no validation contract", ErrInvalidRecoveryPage, index)
		}
		if err := validator.Validate(); err != nil {
			return fmt.Errorf("%w: item %d: %v", ErrInvalidRecoveryPage, index, err)
		}
	}
	if p.Done {
		if p.NextCursor != "" {
			return fmt.Errorf("%w: completed page retains a cursor", ErrInvalidRecoveryPage)
		}
		return nil
	}
	if p.NextCursor == "" || p.NextCursor == after {
		return fmt.Errorf("%w: unfinished page cursor did not advance", ErrInvalidRecoveryPage)
	}
	return nil
}

type TenantRef struct {
	TenantID string `json:"tenant_id"`
}

func (r TenantRef) Validate() error { return ValidateTenantID(r.TenantID) }

type DispatchedRef struct {
	TenantID string        `json:"tenant_id"`
	Token    DispatchToken `json:"dispatch_token"`
}

func (r DispatchedRef) Validate() error {
	if err := ValidateTenantID(r.TenantID); err != nil {
		return err
	}
	return r.Token.Validate()
}

type RunningLease struct {
	TenantID        string    `json:"tenant_id"`
	TaskID          string    `json:"task_id"`
	ClaimGeneration uint64    `json:"claim_generation"`
	LeaseUntil      time.Time `json:"lease_until"`
	ObservedDBNow   time.Time `json:"observed_db_now"`
}

func (r RunningLease) Validate() error {
	if err := ValidateTenantID(r.TenantID); err != nil {
		return err
	}
	if err := ValidateTaskID(r.TaskID); err != nil {
		return err
	}
	if r.ClaimGeneration == 0 || r.ClaimGeneration > math.MaxInt64 || r.LeaseUntil.IsZero() ||
		r.ObservedDBNow.IsZero() || !r.LeaseUntil.After(r.ObservedDBNow) {
		return fmt.Errorf("%w: invalid running lease", ErrInvalidModel)
	}
	return nil
}

type ReservationRef struct {
	TenantID        string `json:"tenant_id"`
	StableToken     string `json:"stable_token"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
}

func (r ReservationRef) Validate() error {
	if err := ValidateTenantID(r.TenantID); err != nil {
		return err
	}
	if !stableReservationPattern.MatchString(r.StableToken) || r.ExpiresAtUnixMS <= 0 {
		return fmt.Errorf("%w: invalid reservation reference", ErrInvalidModel)
	}
	return nil
}

type RedisKeyRef struct {
	Key string `json:"key"`
}

func (r RedisKeyRef) Validate() error {
	if r.Key == "" || len(r.Key) > maxRedisKeyBytes || !utf8.ValidString(r.Key) {
		return fmt.Errorf("%w: invalid Redis key reference", ErrInvalidModel)
	}
	for _, character := range r.Key {
		if character == 0 || unicode.IsControl(character) {
			return fmt.Errorf("%w: Redis key contains a control character", ErrInvalidModel)
		}
	}
	return nil
}

type WriterIdentity struct {
	Fingerprint string `json:"fingerprint"`
}

func (w WriterIdentity) Validate() error {
	if !lowerHex64Pattern.MatchString(w.Fingerprint) {
		return fmt.Errorf("%w: writer fingerprint must be 64 lowercase hex characters", ErrInvalidModel)
	}
	return nil
}

type WriterReadinessReport struct {
	Writer                       WriterIdentity `json:"writer"`
	SchemaReady                  bool           `json:"schema_ready"`
	OwnerInvariantViolationCount int64          `json:"owner_invariant_violation_count"`
	GenerationViolationCount     int64          `json:"generation_violation_count"`
}

func (r WriterReadinessReport) Validate() error {
	if err := r.Writer.Validate(); err != nil {
		return err
	}
	if r.OwnerInvariantViolationCount < 0 || r.GenerationViolationCount < 0 {
		return fmt.Errorf("%w: readiness aggregates must be non-negative", ErrInvalidModel)
	}
	return nil
}

func (r WriterReadinessReport) Ready() bool {
	return r.Validate() == nil && r.SchemaReady && r.OwnerInvariantViolationCount == 0 &&
		r.GenerationViolationCount == 0
}

type ResourceState string

const (
	ResourceReady      ResourceState = "READY"
	ResourceRecovering ResourceState = "RECOVERING"
)

type RecoveryKind string

const (
	RecoveryNone         RecoveryKind = "NONE"
	RecoveryNormal       RecoveryKind = "NORMAL"
	RecoveryRabbitRepair RecoveryKind = "RABBIT_REPAIR"
	RecoveryWriterRebind RecoveryKind = "WRITER_REBIND"
	RecoveryForceRebuild RecoveryKind = "FORCE_REBUILD"
)

func (k RecoveryKind) special() bool {
	return k == RecoveryRabbitRepair || k == RecoveryWriterRebind || k == RecoveryForceRebuild
}

type RecoveryPass string

const (
	RecoveryPassKnownTenants RecoveryPass = "KNOWN_TENANTS"
	RecoveryPassDispatched   RecoveryPass = "DISPATCHED"
	RecoveryPassRunning      RecoveryPass = "RUNNING"
)

func (p RecoveryPass) Validate() error {
	switch p {
	case RecoveryPassKnownTenants, RecoveryPassDispatched, RecoveryPassRunning:
		return nil
	default:
		return fmt.Errorf("%w: unknown recovery pass %q", ErrInvalidRecoveryState, p)
	}
}

type RecoveryPassProgress struct {
	Cycle     uint64 `json:"cycle"`
	Complete  bool   `json:"complete"`
	DiffCount int64  `json:"diff_count"`
}

func (p RecoveryPassProgress) Validate() error {
	if p.DiffCount < 0 || (p.Complete && p.Cycle == 0) {
		return fmt.Errorf("%w: invalid recovery pass progress", ErrInvalidRecoveryState)
	}
	return nil
}

type RabbitRepairProgress struct {
	RepairHighWater    string `json:"repair_high_water"`
	RepairPassComplete bool   `json:"repair_pass_complete"`
}

func (p RabbitRepairProgress) Validate() error {
	if p.RepairHighWater != "" {
		if err := ValidateHighWater(p.RepairHighWater); err != nil {
			return err
		}
	}
	if p.RepairPassComplete && p.RepairHighWater == "" {
		return fmt.Errorf("%w: rabbit repair pass lacks high water", ErrInvalidRecoveryState)
	}
	return nil
}

type WriterRebindProgress struct {
	OriginalWriterFingerprint string `json:"original_writer_fingerprint"`
	TargetWriterFingerprint   string `json:"target_writer_fingerprint"`
}

func (p WriterRebindProgress) Validate() error {
	if !lowerHex64Pattern.MatchString(p.OriginalWriterFingerprint) ||
		!lowerHex64Pattern.MatchString(p.TargetWriterFingerprint) ||
		p.OriginalWriterFingerprint == p.TargetWriterFingerprint {
		return fmt.Errorf("%w: invalid writer rebind progress", ErrInvalidRecoveryState)
	}
	return nil
}

type ForceRebuildProgress struct {
	NotBeforeUnixMS    int64 `json:"not_before_unix_ms"`
	DeletePassComplete bool  `json:"delete_pass_complete"`
}

func (p ForceRebuildProgress) Validate() error {
	if p.NotBeforeUnixMS <= 0 {
		return fmt.Errorf("%w: force rebuild not-before must be positive", ErrInvalidRecoveryState)
	}
	return nil
}

type RecoveryProgress struct {
	Kind         RecoveryKind          `json:"kind"`
	OperationID  string                `json:"operation_id"`
	HighWater    string                `json:"high_water"`
	KnownTenants RecoveryPassProgress  `json:"known_tenants"`
	Dispatched   RecoveryPassProgress  `json:"dispatched"`
	Running      RecoveryPassProgress  `json:"running"`
	RabbitRepair *RabbitRepairProgress `json:"rabbit_repair,omitempty"`
	WriterRebind *WriterRebindProgress `json:"writer_rebind,omitempty"`
	ForceRebuild *ForceRebuildProgress `json:"force_rebuild,omitempty"`
}

func (p RecoveryProgress) Validate() error {
	if p.Kind != RecoveryNormal && !p.Kind.special() {
		return fmt.Errorf("%w: invalid recovery progress kind", ErrInvalidRecoveryState)
	}
	if p.Kind == RecoveryNormal {
		if p.OperationID != "" {
			return fmt.Errorf("%w: normal recovery has an operation ID", ErrInvalidRecoveryState)
		}
	} else if !lowerHex32Pattern.MatchString(p.OperationID) {
		return fmt.Errorf("%w: special recovery operation ID is invalid", ErrInvalidRecoveryState)
	}
	if p.HighWater != "" {
		if err := ValidateHighWater(p.HighWater); err != nil {
			return err
		}
	}
	if err := p.KnownTenants.Validate(); err != nil {
		return err
	}
	if err := p.Dispatched.Validate(); err != nil {
		return err
	}
	if err := p.Running.Validate(); err != nil {
		return err
	}
	if p.HighWater == "" && (p.KnownTenants.Complete || p.Dispatched.Complete || p.Running.Complete) {
		return fmt.Errorf("%w: completed common pass lacks high water", ErrInvalidRecoveryState)
	}
	switch p.Kind {
	case RecoveryNormal:
		if p.RabbitRepair != nil || p.WriterRebind != nil || p.ForceRebuild != nil {
			return fmt.Errorf("%w: normal recovery carries special progress", ErrInvalidRecoveryState)
		}
	case RecoveryRabbitRepair:
		if p.RabbitRepair == nil || p.WriterRebind != nil || p.ForceRebuild != nil {
			return fmt.Errorf("%w: rabbit recovery progress shape mismatch", ErrInvalidRecoveryState)
		}
		return p.RabbitRepair.Validate()
	case RecoveryWriterRebind:
		if p.WriterRebind == nil || p.RabbitRepair != nil || p.ForceRebuild != nil {
			return fmt.Errorf("%w: writer recovery progress shape mismatch", ErrInvalidRecoveryState)
		}
		return p.WriterRebind.Validate()
	case RecoveryForceRebuild:
		if p.ForceRebuild == nil || p.RabbitRepair != nil || p.WriterRebind != nil {
			return fmt.Errorf("%w: force recovery progress shape mismatch", ErrInvalidRecoveryState)
		}
		return p.ForceRebuild.Validate()
	}
	return nil
}

type ResourceFence struct {
	Epoch             string `json:"epoch"`
	WriterFingerprint string `json:"writer_fingerprint"`
}

func (f ResourceFence) Validate() error {
	if !lowerHex32Pattern.MatchString(f.Epoch) || !lowerHex64Pattern.MatchString(f.WriterFingerprint) {
		return fmt.Errorf("%w: invalid resource fence", ErrInvalidRecoveryState)
	}
	return nil
}

type RecoveryLock struct {
	OwnerToken string `json:"owner_token"`
}

func (l RecoveryLock) Validate() error {
	if !lowerHex32Pattern.MatchString(l.OwnerToken) {
		return fmt.Errorf("%w: invalid recovery lock", ErrInvalidRecoveryState)
	}
	return nil
}

type RedisDeploymentMode string

const (
	RedisDeploymentStandalone RedisDeploymentMode = "standalone"
	RedisDeploymentCluster    RedisDeploymentMode = "cluster"
)

// RedisTopology is a side-effect-free inspection result used before enabling
// multi-key Lua and before destructive standalone-primary recovery scans.
type RedisTopology struct {
	Mode            RedisDeploymentMode `json:"mode"`
	WritablePrimary bool                `json:"writable_primary"`
}

func (t RedisTopology) Validate() error {
	switch t.Mode {
	case RedisDeploymentStandalone, RedisDeploymentCluster:
		return nil
	default:
		return fmt.Errorf("%w: unknown Redis deployment mode", ErrInvalidModel)
	}
}

func (t RedisTopology) SupportsFairQueue() bool {
	return t.Mode == RedisDeploymentStandalone && t.WritablePrimary
}

// ForceRebuildDeadline proves the not-before value was derived from Redis
// server time while the raw recovery lock was still held.
type ForceRebuildDeadline struct {
	ObservedRedisTime time.Time `json:"observed_redis_time"`
	NotBefore         time.Time `json:"not_before"`
}

func NewForceRebuildDeadline(observedRedisTime time.Time, minimumDelay time.Duration) (ForceRebuildDeadline, error) {
	observedRedisTime = observedRedisTime.UTC()
	if observedRedisTime.IsZero() || observedRedisTime.Nanosecond()%int(time.Microsecond) != 0 ||
		minimumDelay <= 0 || minimumDelay > maxResourceDuration {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: invalid force rebuild deadline inputs", ErrInvalidModel)
	}
	notBefore := observedRedisTime.Add(minimumDelay)
	if remainder := notBefore.Nanosecond() % int(time.Millisecond); remainder != 0 {
		notBefore = notBefore.Add(time.Millisecond - time.Duration(remainder))
	}
	notBefore = notBefore.UTC().Truncate(time.Millisecond)
	return ForceRebuildDeadline{ObservedRedisTime: observedRedisTime, NotBefore: notBefore}, nil
}

func (d ForceRebuildDeadline) Validate(minimumDelay time.Duration) error {
	want, err := NewForceRebuildDeadline(d.ObservedRedisTime, minimumDelay)
	if err != nil || d.ObservedRedisTime.Location() != time.UTC || d.NotBefore.Location() != time.UTC ||
		!d.ObservedRedisTime.Equal(want.ObservedRedisTime) || !d.NotBefore.Equal(want.NotBefore) {
		return fmt.Errorf("%w: invalid force rebuild deadline", ErrInvalidModel)
	}
	return nil
}

type RecoveryFence struct {
	ResourceFence
	OwnerToken  string       `json:"owner_token"`
	Kind        RecoveryKind `json:"kind"`
	OperationID string       `json:"operation_id"`
}

func (f RecoveryFence) Validate() error {
	if err := f.ResourceFence.Validate(); err != nil {
		return err
	}
	if !lowerHex32Pattern.MatchString(f.OwnerToken) {
		return fmt.Errorf("%w: invalid recovery owner", ErrInvalidRecoveryState)
	}
	if f.Kind == RecoveryNormal {
		if f.OperationID != "" {
			return fmt.Errorf("%w: normal recovery fence has operation ID", ErrInvalidRecoveryState)
		}
		return nil
	}
	if !f.Kind.special() || !lowerHex32Pattern.MatchString(f.OperationID) {
		return fmt.Errorf("%w: invalid special recovery fence", ErrInvalidRecoveryState)
	}
	return nil
}

// RecoveryControlSnapshot is a read-only preflight view. Present=false has one
// canonical zero representation so missing and corrupt control are distinct.
type RecoveryControlSnapshot struct {
	Present                    bool              `json:"present"`
	State                      ResourceState     `json:"state"`
	Epoch                      string            `json:"epoch"`
	ProtocolVersion            int               `json:"protocol_version"`
	WriterFingerprint          string            `json:"writer_fingerprint"`
	Kind                       RecoveryKind      `json:"kind"`
	OperationID                string            `json:"operation_id"`
	LastCompletedOperationID   string            `json:"last_completed_operation_id"`
	LastCompletedOperationKind RecoveryKind      `json:"last_completed_operation_kind"`
	Progress                   *RecoveryProgress `json:"progress,omitempty"`
}

func (s RecoveryControlSnapshot) Validate() error {
	if !s.Present {
		zero := RecoveryControlSnapshot{}
		if s != zero {
			return fmt.Errorf("%w: missing control has non-zero fields", ErrInvalidRecoveryState)
		}
		return nil
	}
	if !lowerHex32Pattern.MatchString(s.Epoch) || !lowerHex64Pattern.MatchString(s.WriterFingerprint) ||
		s.ProtocolVersion != MessageVersion1 {
		return fmt.Errorf("%w: invalid control identity", ErrInvalidRecoveryState)
	}
	if s.LastCompletedOperationID == "" {
		if s.LastCompletedOperationKind != "" && s.LastCompletedOperationKind != RecoveryNone {
			return fmt.Errorf("%w: last completed operation kind has no ID", ErrInvalidRecoveryState)
		}
	} else if !lowerHex32Pattern.MatchString(s.LastCompletedOperationID) ||
		!s.LastCompletedOperationKind.special() {
		return fmt.Errorf("%w: invalid last completed operation identity", ErrInvalidRecoveryState)
	}
	switch s.State {
	case ResourceReady:
		if s.Kind != RecoveryNone || s.OperationID != "" || s.Progress != nil {
			return fmt.Errorf("%w: READY control retains recovery state", ErrInvalidRecoveryState)
		}
	case ResourceRecovering:
		if s.Kind == RecoveryNone || s.Progress == nil || s.Progress.Kind != s.Kind ||
			s.Progress.OperationID != s.OperationID {
			return fmt.Errorf("%w: RECOVERING control/progress mismatch", ErrInvalidRecoveryState)
		}
		if err := s.Progress.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown resource state", ErrInvalidRecoveryState)
	}
	return nil
}

type OperationPhase string

const (
	OperationActive         OperationPhase = "ACTIVE"
	OperationReadyCommitted OperationPhase = "READY_COMMITTED"
	OperationCompleted      OperationPhase = "COMPLETED"
)

// RecoveryOperationRecord is a lossless domain-neutral journal record. NORMAL
// recovery never creates one.
type RecoveryOperationRecord struct {
	Resource                  string         `json:"resource"`
	OperationID               string         `json:"operation_id"`
	Kind                      RecoveryKind   `json:"kind"`
	Phase                     OperationPhase `json:"phase"`
	CurrentWriterFingerprint  string         `json:"current_writer_fingerprint"`
	OriginalWriterFingerprint string         `json:"original_writer_fingerprint"`
	TargetWriterFingerprint   string         `json:"target_writer_fingerprint"`
	RepairHighWater           *string        `json:"repair_high_water"`
	RepairPassComplete        bool           `json:"repair_pass_complete"`
	ForceNotBefore            *time.Time     `json:"force_not_before"`
	ForceDeletePassComplete   bool           `json:"force_delete_pass_complete"`
	Version                   int64          `json:"version"`
	CreatedAt                 time.Time      `json:"created_at"`
	UpdatedAt                 time.Time      `json:"updated_at"`
}

func validCanonicalForceNotBefore(value *time.Time) bool {
	return value != nil && !value.IsZero() && value.Location() == time.UTC &&
		value.Nanosecond()%int(time.Millisecond) == 0
}

func (r RecoveryOperationRecord) validateImmutable() error {
	if err := ValidateResource(r.Resource); err != nil || !lowerHex32Pattern.MatchString(r.OperationID) ||
		!lowerHex64Pattern.MatchString(r.CurrentWriterFingerprint) || !r.Kind.special() {
		return ErrInvalidOperationRecord
	}
	switch r.Kind {
	case RecoveryRabbitRepair:
		if r.OriginalWriterFingerprint != "" || r.TargetWriterFingerprint != "" || r.ForceNotBefore != nil {
			return ErrInvalidOperationRecord
		}
	case RecoveryWriterRebind:
		if !lowerHex64Pattern.MatchString(r.OriginalWriterFingerprint) ||
			!lowerHex64Pattern.MatchString(r.TargetWriterFingerprint) ||
			r.OriginalWriterFingerprint == r.TargetWriterFingerprint ||
			r.CurrentWriterFingerprint != r.TargetWriterFingerprint || r.ForceNotBefore != nil {
			return ErrInvalidOperationRecord
		}
	case RecoveryForceRebuild:
		if r.OriginalWriterFingerprint != "" || r.TargetWriterFingerprint != "" ||
			!validCanonicalForceNotBefore(r.ForceNotBefore) {
			return ErrInvalidOperationRecord
		}
	}
	return nil
}

// ValidateProposal validates the immutable pre-insert shape. Store-generated
// phase, version, timestamps, and progress must still be zero.
func (r RecoveryOperationRecord) ValidateProposal() error {
	if err := r.validateImmutable(); err != nil {
		return err
	}
	if r.Phase != "" || r.Version != 0 || !r.CreatedAt.IsZero() || !r.UpdatedAt.IsZero() ||
		r.RepairHighWater != nil || r.RepairPassComplete || r.ForceDeletePassComplete {
		return ErrInvalidOperationRecord
	}
	return nil
}

func (r RecoveryOperationRecord) ValidatePersisted() error {
	if err := r.validateImmutable(); err != nil {
		return err
	}
	if r.Version <= 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return ErrInvalidOperationRecord
	}
	switch r.Phase {
	case OperationActive, OperationReadyCommitted, OperationCompleted:
	default:
		return ErrInvalidOperationRecord
	}
	if r.RepairHighWater != nil {
		if err := ValidateHighWater(*r.RepairHighWater); err != nil {
			return ErrInvalidOperationRecord
		}
	}
	switch r.Kind {
	case RecoveryRabbitRepair:
		if r.ForceDeletePassComplete || (r.RepairPassComplete && r.RepairHighWater == nil) {
			return ErrInvalidOperationRecord
		}
	case RecoveryWriterRebind:
		if r.RepairHighWater != nil || r.RepairPassComplete || r.ForceDeletePassComplete {
			return ErrInvalidOperationRecord
		}
	case RecoveryForceRebuild:
		if r.RepairHighWater != nil || r.RepairPassComplete {
			return ErrInvalidOperationRecord
		}
	}
	if r.Phase != OperationActive {
		if r.Kind == RecoveryRabbitRepair && (r.RepairHighWater == nil || !r.RepairPassComplete) {
			return ErrInvalidOperationRecord
		}
		if r.Kind == RecoveryForceRebuild && !r.ForceDeletePassComplete {
			return ErrInvalidOperationRecord
		}
	}
	return nil
}

type RabbitRepairAttestation struct {
	OldBrokerIsolated bool `json:"old_broker_isolated"`
	PublishersPaused  bool `json:"publishers_paused"`
}

func (a RabbitRepairAttestation) Validate() error {
	if !a.OldBrokerIsolated || !a.PublishersPaused {
		return ErrOperatorConfirmation
	}
	return nil
}

type WriterRebindAttestation struct {
	OldWriterFenced         bool `json:"old_writer_fenced"`
	ResourceRuntimesStopped bool `json:"resource_runtimes_stopped"`
	NewWriterAuthoritative  bool `json:"new_writer_authoritative"`
}

func (a WriterRebindAttestation) Validate() error {
	if !a.OldWriterFenced || !a.ResourceRuntimesStopped || !a.NewWriterAuthoritative {
		return ErrOperatorConfirmation
	}
	return nil
}

type ForceRebuildAttestation struct {
	DiscardRedisCoordinationState bool `json:"discard_redis_coordination_state"`
}

func (a ForceRebuildAttestation) Validate() error {
	if !a.DiscardRedisCoordinationState {
		return ErrOperatorConfirmation
	}
	return nil
}

// Operator reports contain aggregate review facts only. They deliberately omit
// task/tenant identities, cursors, high-water marks, epochs, and operation IDs.
type OperationSummary struct {
	Present bool           `json:"present"`
	Kind    RecoveryKind   `json:"kind"`
	Phase   OperationPhase `json:"phase"`
}

func (s OperationSummary) Validate() error {
	if !s.Present {
		if s.Kind != "" || s.Phase != "" {
			return fmt.Errorf("%w: absent operation summary has state", ErrInvalidModel)
		}
		return nil
	}
	if !s.Kind.special() {
		return fmt.Errorf("%w: operation summary kind is not special", ErrInvalidModel)
	}
	switch s.Phase {
	case OperationActive, OperationReadyCommitted, OperationCompleted:
		return nil
	default:
		return fmt.Errorf("%w: operation summary phase is invalid", ErrInvalidModel)
	}
}

type RabbitRepairReport struct {
	Resource       string           `json:"resource"`
	Writer         WriterIdentity   `json:"writer"`
	CandidateCount int64            `json:"candidate_count"`
	PagesScanned   int64            `json:"pages_scanned"`
	Operation      OperationSummary `json:"operation"`
}

func (r RabbitRepairReport) Validate() error {
	if err := ValidateResource(r.Resource); err != nil {
		return err
	}
	if err := r.Writer.Validate(); err != nil {
		return err
	}
	if r.CandidateCount < 0 || r.PagesScanned < 0 {
		return fmt.Errorf("%w: Rabbit repair aggregates must be non-negative", ErrInvalidModel)
	}
	return r.Operation.Validate()
}

type WriterRebindReport struct {
	Resource                     string                `json:"resource"`
	ExpectedOldWriterFingerprint string                `json:"expected_old_writer_fingerprint"`
	TargetWriter                 WriterIdentity        `json:"target_writer"`
	Readiness                    WriterReadinessReport `json:"readiness"`
	ValidRunningCount            int64                 `json:"valid_running_count"`
	Operation                    OperationSummary      `json:"operation"`
}

func (r WriterRebindReport) Validate() error {
	if err := ValidateResource(r.Resource); err != nil {
		return err
	}
	if !lowerHex64Pattern.MatchString(r.ExpectedOldWriterFingerprint) {
		return fmt.Errorf("%w: expected old writer is invalid", ErrInvalidModel)
	}
	if err := r.TargetWriter.Validate(); err != nil {
		return err
	}
	if r.ExpectedOldWriterFingerprint == r.TargetWriter.Fingerprint {
		return fmt.Errorf("%w: writer rebind target equals old writer", ErrInvalidModel)
	}
	if err := r.Readiness.Validate(); err != nil {
		return err
	}
	if r.Readiness.Writer != r.TargetWriter || r.ValidRunningCount < 0 {
		return fmt.Errorf("%w: writer rebind aggregates are inconsistent", ErrInvalidModel)
	}
	return r.Operation.Validate()
}

type ForceRebuildReport struct {
	Resource                  string           `json:"resource"`
	Writer                    WriterIdentity   `json:"writer"`
	ControlPresent            bool             `json:"control_present"`
	ControlState              ResourceState    `json:"control_state"`
	ControlKind               RecoveryKind     `json:"control_kind"`
	StandaloneRedis           bool             `json:"standalone_redis"`
	CurrentWriterVerified     bool             `json:"current_writer_verified"`
	RabbitTruthSourceVerified bool             `json:"rabbit_truth_source_verified"`
	RebuildableKeyCount       int64            `json:"rebuildable_key_count"`
	PagesScanned              int64            `json:"pages_scanned"`
	Operation                 OperationSummary `json:"operation"`
}

func (r ForceRebuildReport) Validate() error {
	if err := ValidateResource(r.Resource); err != nil {
		return err
	}
	if err := r.Writer.Validate(); err != nil {
		return err
	}
	if r.RebuildableKeyCount < 0 || r.PagesScanned < 0 {
		return fmt.Errorf("%w: force rebuild aggregates must be non-negative", ErrInvalidModel)
	}
	if !r.ControlPresent {
		if r.ControlState != "" || r.ControlKind != "" {
			return fmt.Errorf("%w: absent control has state", ErrInvalidModel)
		}
	} else {
		switch r.ControlState {
		case ResourceReady:
			if r.ControlKind != RecoveryNone {
				return fmt.Errorf("%w: READY control has recovery kind", ErrInvalidModel)
			}
		case ResourceRecovering:
			if r.ControlKind != RecoveryNormal && !r.ControlKind.special() {
				return fmt.Errorf("%w: RECOVERING control kind is invalid", ErrInvalidModel)
			}
		default:
			return fmt.Errorf("%w: control state is invalid", ErrInvalidModel)
		}
	}
	return r.Operation.Validate()
}

type ProcessingTurnToken string

func (t ProcessingTurnToken) Validate() error {
	if !lowerHex32Pattern.MatchString(string(t)) {
		return fmt.Errorf("%w: invalid processing turn token", ErrInvalidModel)
	}
	return nil
}

type ProcessingTurn struct {
	Token                        ProcessingTurnToken `json:"token"`
	TenantID                     string              `json:"tenant_id"`
	ObservedActivationGeneration uint64              `json:"observed_activation_generation"`
}

func (t ProcessingTurn) Validate() error {
	if t.Token.Validate() != nil || ValidateTenantID(t.TenantID) != nil ||
		t.ObservedActivationGeneration == 0 {
		return fmt.Errorf("%w: invalid processing turn", ErrInvalidModel)
	}
	return nil
}

type RecoveryCleanupResult struct {
	RemovedProvisionals   int64 `json:"removed_provisionals"`
	RemovedTurns          int64 `json:"removed_turns"`
	RemainingProvisionals int64 `json:"remaining_provisionals"`
	RemainingTurns        int64 `json:"remaining_turns"`
}

func (r RecoveryCleanupResult) Validate() error {
	if r.RemovedProvisionals < 0 || r.RemovedTurns < 0 || r.RemainingProvisionals < 0 || r.RemainingTurns < 0 {
		return fmt.Errorf("%w: recovery cleanup aggregates must be non-negative", ErrInvalidModel)
	}
	return nil
}

type PublishReceipt struct {
	AttemptID string `json:"attempt_id"`
}

func (r PublishReceipt) Validate() error {
	if !lowerHex32Pattern.MatchString(r.AttemptID) {
		return fmt.Errorf("%w: publish attempt ID must be 128-bit lowercase hex", ErrInvalidModel)
	}
	return nil
}

// DeadLetterRequest separates the trusted consumer context and bounded reason
// from untrusted delivery facts. Rabbit topology must use Delivery's registered
// resource and never body/header resource values.
type DeadLetterRequest struct {
	Delivery   PrepareRequest `json:"delivery"`
	ReasonCode string         `json:"reason_code"`
}

func (r DeadLetterRequest) Validate() error {
	if err := r.Delivery.Validate(); err != nil {
		return err
	}
	if !reasonCodePattern.MatchString(r.ReasonCode) {
		return fmt.Errorf("%w: dead-letter reason must be canonical bounded ASCII", ErrInvalidModel)
	}
	return nil
}

// Delivery is the transport boundary exposed to runtimes and tests, avoiding
// any dependency on amqp.Delivery in domain code.
type Delivery interface {
	Request() PrepareRequest
	Ack(ctx context.Context) error
	Nack(ctx context.Context, requeue bool) error
}

type RabbitClient interface {
	EnsureTenantTopology(ctx context.Context, resource, tenant string) error
	// Publish methods return a non-zero receipt on every outcome after an
	// attempt ID has been allocated, including return, NACK, and timeout.
	PublishMandatoryConfirmed(ctx context.Context, message Message) (PublishReceipt, error)
	GetOne(ctx context.Context, resource, tenant string) (Delivery, bool, error)
	ReadyDepth(ctx context.Context, resource, tenant string) (int64, error)
	PublishDeadLetterConfirmed(ctx context.Context, request DeadLetterRequest) (PublishReceipt, error)
	Close() error
}

type RedisInspector interface {
	InspectRedisTopology(ctx context.Context) (RedisTopology, error)
}

type DispatchSource interface {
	ListDispatchCandidates(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error)
	GetDispatchableByID(ctx context.Context, taskID string) (DispatchCandidate, bool, error)
	MarkDispatched(ctx context.Context, candidate DispatchCandidate) (bool, error)
}

type ExpiredRearmSource interface {
	RearmExpiredPage(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error)
}

// TaskPreparer owns the canonical claim transaction and all domain-specific
// classification. If a commit outcome is uncertain, Prepare must perform a
// fresh bounded canonical read using its own claim identity before returning;
// a non-nil error tells the runtime only to NACK/no-run, never to guess that a
// claim committed. Repair-only requests must return the explicit poison/DLQ
// disposition enforced by PrepareResult.ValidateFor.
type TaskPreparer interface {
	Prepare(ctx context.Context, request PrepareRequest) (PreparedTask, PrepareResult, error)
}

type PreparedTask interface {
	Run(ctx context.Context) error
}

type RecoverySource interface {
	CaptureHighWater(ctx context.Context) (string, error)
	ListKnownTenants(ctx context.Context, highWater, after string, limit int) (RecoveryPage[TenantRef], error)
	ListDispatched(ctx context.Context, highWater, after string, limit int) (RecoveryPage[DispatchedRef], error)
	ListValidRunning(ctx context.Context, highWater, after string, limit int) (RecoveryPage[RunningLease], error)
}

type BrokerRepairSource interface {
	CaptureRepairHighWater(ctx context.Context) (string, error)
	ListBrokerBackedCandidates(ctx context.Context, highWater, after string, limit int) (RecoveryPage[DispatchCandidate], error)
	RearmAfterBrokerLoss(ctx context.Context, candidate DispatchCandidate) (DispatchCandidate, bool, error)
}

type WriterRebindSource interface {
	RecoverySource
	ReadWriterIdentity(ctx context.Context) (WriterIdentity, error)
	CheckSchemaAndInvariants(ctx context.Context) (WriterReadinessReport, error)
	CountValidRunning(ctx context.Context) (int64, error)
}

type OperationStartSession interface {
	Read(ctx context.Context) (RecoveryOperationRecord, bool, error)
	BeginSpecial(ctx context.Context, expected *RecoveryOperationRecord, proposal RecoveryOperationRecord) (RecoveryOperationRecord, error)
}

type OperationJournal interface {
	Read(ctx context.Context, resource, expectedWriter string) (RecoveryOperationRecord, bool, error)
	WithStartFence(ctx context.Context, resource, expectedWriter string, fn func(OperationStartSession) error) error
	SetRepairHighWater(ctx context.Context, expected RecoveryOperationRecord, highWater string) (RecoveryOperationRecord, error)
	MarkRepairPassComplete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
	MarkForceDeletePassComplete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
	CommitReady(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
	Complete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
}

// Coordinator owns only rebuildable Redis scheduling state. Every method is
// fenced by the supplied READY or recovery identity.
type Coordinator interface {
	ObserveReadyFence(ctx context.Context, resource, expectedWriter string) (ResourceFence, error)
	CheckReadyFence(ctx context.Context, resource string, fence ResourceFence) error
	Activate(ctx context.Context, resource string, fence ResourceFence, tenant string) error
	EnsureKnownTenant(ctx context.Context, resource string, fence ResourceFence, tenant string) error
	EnsureActive(ctx context.Context, resource string, fence ResourceFence, tenant string) error
	NextTurn(ctx context.Context, resource string, fence ResourceFence, turnToken ProcessingTurnToken, ttl time.Duration) (ProcessingTurn, bool, error)
	RotateOrDeactivate(ctx context.Context, resource string, fence ResourceFence, turnToken ProcessingTurnToken, observedGeneration uint64, hasReady bool) error
	AcquireProvisional(ctx context.Context, resource string, fence ResourceFence, tenant, attemptID string, limits CapacityLimits, ttl time.Duration) (ReservationDecision, error)
	BindReservation(ctx context.Context, resource string, fence ResourceFence, tenant, attemptID, stableToken string, ttl time.Duration) error
	RenewStable(ctx context.Context, resource string, fence ResourceFence, tenant, stableToken string, ttl time.Duration) error
	// Release accepts either the provisional attempt ID allocated before claim
	// or the stable reservation token produced after a successful bind.
	Release(ctx context.Context, resource string, fence ResourceFence, tenant, token string) error
	ListReadyStableInflight(ctx context.Context, resource string, fence ResourceFence, cursor string, limit int) (RecoveryPage[ReservationRef], error)
	EnsureReadyStableInflight(ctx context.Context, resource string, fence ResourceFence, tenant, stableToken string, ttl time.Duration) error
	ReapExpiredTurnsAndProvisionals(ctx context.Context, resource string, fence ResourceFence, limit int) (RecoveryCleanupResult, error)

	AcquireRecoveryLock(ctx context.Context, resource, owner string, ttl time.Duration) (RecoveryLock, error)
	CheckRecoveryLock(ctx context.Context, resource string, lock RecoveryLock) error
	RenewRecoveryLock(ctx context.Context, resource string, lock RecoveryLock, ttl time.Duration) error
	InspectRecoveryStart(ctx context.Context, resource string, lock RecoveryLock) (RecoveryControlSnapshot, error)
	ComputeForceRebuildDeadlineWithLock(ctx context.Context, resource string, lock RecoveryLock, minimumDelay time.Duration) (ForceRebuildDeadline, error)
	ReleaseRecoveryLock(ctx context.Context, resource string, lock RecoveryLock) error
	BeginRecoveryWithLock(ctx context.Context, resource, writer string, lock RecoveryLock, ttl time.Duration) (RecoveryFence, error)
	BeginRabbitRepairWithLock(ctx context.Context, resource, writer, operationID string, lock RecoveryLock, ttl time.Duration) (RecoveryFence, error)
	BeginWriterRebindWithLock(ctx context.Context, resource, originalWriter, targetWriter, operationID string, lock RecoveryLock, ttl time.Duration) (RecoveryFence, error)
	BeginForceRebuildWithLock(ctx context.Context, resource, writer, operationID string, notBeforeUnixMS int64, lock RecoveryLock, ttl time.Duration) (RecoveryFence, error)
	RenewRecovery(ctx context.Context, resource string, fence RecoveryFence, ttl time.Duration) error
	RecoveryReapExpired(ctx context.Context, resource string, fence RecoveryFence, limit int) (RecoveryCleanupResult, error)
	ResetResource(ctx context.Context, resource string, fence RecoveryFence) error
	SetRecoveryHighWater(ctx context.Context, resource string, fence RecoveryFence, highWater string) error
	SetRabbitRepairHighWater(ctx context.Context, resource string, fence RecoveryFence, highWater string) error
	MarkRabbitRepairPassComplete(ctx context.Context, resource string, fence RecoveryFence) error
	MarkForceDeletePassComplete(ctx context.Context, resource string, fence RecoveryFence) error
	RestoreKnownTenant(ctx context.Context, resource string, fence RecoveryFence, tenant string) error
	RestoreActiveTenant(ctx context.Context, resource string, fence RecoveryFence, tenant string) error
	RestoreInflight(ctx context.Context, resource string, fence RecoveryFence, tenant, stableToken string, ttl time.Duration) error
	ListRecoveryStableInflight(ctx context.Context, resource string, fence RecoveryFence, cursor string, limit int) (RecoveryPage[ReservationRef], error)
	DeleteRecoveryStableInflight(ctx context.Context, resource string, fence RecoveryFence, ref ReservationRef) error
	MarkRecoveryPass(ctx context.Context, resource string, fence RecoveryFence, pass RecoveryPass, cycle uint64, complete bool, diffCount int64) error
	ListOwnedResourceKeys(ctx context.Context, resource string, fence RecoveryFence, cursor string, limit int) (RecoveryPage[RedisKeyRef], error)
	DeleteOwnedResourceKeys(ctx context.Context, resource string, fence RecoveryFence, keys []RedisKeyRef) error
	FinishRecovery(ctx context.Context, resource string, fence RecoveryFence) error
}
