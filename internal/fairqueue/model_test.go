package fairqueue

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testOperationID = "0123456789abcdef0123456789abcdef"
	testOwnerToken  = "abcdef0123456789abcdef0123456789"
	testWriterA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testWriterB     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func validTestMessage() Message {
	return Message{
		Version:  MessageVersion1,
		Resource: "rag.index",
		TenantID: "tenant-a",
		TaskType: "document-index",
		TaskID:   "42",
		DispatchToken: DispatchToken{
			Resource:   "rag.index",
			TaskID:     "42",
			Generation: 7,
		},
	}
}

func validHeaderFacts(token DispatchToken) StableHeaderFacts {
	version := int32(MessageVersion1)
	resource := token.Resource
	taskID := token.TaskID
	generation := int64(token.Generation)
	return StableHeaderFacts{
		ProtocolVersion: &version, Resource: &resource, TaskID: &taskID,
		DispatchGeneration: &generation,
	}
}

func TestMessageV1StrictJSONRoundTrip(t *testing.T) {
	want := validTestMessage()
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const canonical = `{"version":1,"resource":"rag.index","tenant_id":"tenant-a","task_type":"document-index","task_id":"42","dispatch_token":{"resource":"rag.index","task_id":"42","generation":7}}`
	if string(raw) != canonical {
		t.Fatalf("Marshal() = %s, want stable %s", raw, canonical)
	}

	got, err := StrictDecodeMessage(raw)
	if err != nil {
		t.Fatalf("StrictDecodeMessage() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}

	var viaJSON Message
	if err := json.Unmarshal(raw, &viaJSON); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(viaJSON, want) {
		t.Fatalf("json.Unmarshal() = %#v, want %#v", viaJSON, want)
	}
}

func TestMessageRejectsInvalidIdentityAndToken(t *testing.T) {
	valid := validTestMessage()
	tests := map[string]func(*Message){
		"version":           func(m *Message) { m.Version = 2 },
		"resource":          func(m *Message) { m.Resource = "" },
		"resource syntax":   func(m *Message) { m.Resource = "RAG Index" },
		"tenant":            func(m *Message) { m.TenantID = "" },
		"task type":         func(m *Message) { m.TaskType = "" },
		"task id":           func(m *Message) { m.TaskID = "" },
		"token resource":    func(m *Message) { m.DispatchToken.Resource = "" },
		"token task":        func(m *Message) { m.DispatchToken.TaskID = "" },
		"zero generation":   func(m *Message) { m.DispatchToken.Generation = 0 },
		"huge generation":   func(m *Message) { m.DispatchToken.Generation = uint64(math.MaxInt64) + 1 },
		"resource mismatch": func(m *Message) { m.DispatchToken.Resource = "image.generate" },
		"task mismatch":     func(m *Message) { m.DispatchToken.TaskID = "43" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			message := valid
			mutate(&message)
			if err := message.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
			if _, err := json.Marshal(message); err == nil {
				t.Fatal("Marshal() error = nil")
			}
		})
	}

	// The common model deliberately does not assume numeric task IDs.
	valid.TaskID = "imgt_abcd1234abcd1234"
	valid.DispatchToken.TaskID = valid.TaskID
	if err := valid.Validate(); err != nil {
		t.Fatalf("opaque task ID rejected: %v", err)
	}
}

func TestMessageStrictDecoderRejectsUnknownDuplicateTrailingAndOversize(t *testing.T) {
	validRaw, err := json.Marshal(validTestMessage())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"unknown":           []byte(`{"version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","generation":1},"extra":true}`),
		"case alias":        []byte(`{"Version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","generation":1}}`),
		"case duplicate":    []byte(`{"version":1,"Version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","generation":1}}`),
		"nested unknown":    []byte(`{"version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","generation":1,"extra":true}}`),
		"nested case alias": []byte(`{"version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"Resource":"rag.index","task_id":"1","generation":1}}`),
		"duplicate":         []byte(`{"version":1,"resource":"rag.index","resource":"image.generate","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","generation":1}}`),
		"nested duplicate":  []byte(`{"version":1,"resource":"rag.index","tenant_id":"t","task_type":"x","task_id":"1","dispatch_token":{"resource":"rag.index","task_id":"1","task_id":"2","generation":1}}`),
		"trailing":          append(append([]byte{}, validRaw...), []byte(` {}`)...),
		"empty":             nil,
		"oversize":          []byte(`{"version":1,"resource":"rag.index","tenant_id":"` + strings.Repeat("x", MaxMessageBytes) + `"}`),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := StrictDecodeMessage(raw); err == nil {
				t.Fatal("StrictDecodeMessage() error = nil")
			}
		})
	}
}

func TestDispatchCandidatePreservesOpaqueTokenAndGuard(t *testing.T) {
	candidate := DispatchCandidate{Message: validTestMessage(), Guard: `opaque:\u0000:not-json:{}`}
	if err := candidate.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := candidate.Message.DispatchToken; got != validTestMessage().DispatchToken {
		t.Fatalf("token changed: %#v", got)
	}
	if candidate.Guard != `opaque:\u0000:not-json:{}` {
		t.Fatalf("guard changed: %q", candidate.Guard)
	}

	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "opaque") || strings.Contains(string(raw), "guard") {
		t.Fatalf("opaque database guard leaked into wire JSON: %s", raw)
	}
}

type testPreparedTask struct{}

func (testPreparedTask) Run(context.Context) error { return nil }

func TestPrepareResultMatrix(t *testing.T) {
	prepared := testPreparedTask{}
	claim := &ClaimRef{TenantID: "tenant-a", TaskID: "42", ClaimGeneration: 7}
	tests := []struct {
		name        string
		result      PrepareResult
		prepared    PreparedTask
		wantInvalid bool
	}{
		{"claimed", PrepareResult{Disposition: PrepareClaimed, DeliveryAction: DeliveryPromoteThenAckRun, CanonicalEffect: CanonicalClaimCommitted, Claim: claim}, prepared, false},
		{"capacity", PrepareResult{Disposition: PrepareCapacityDeferred, DeliveryAction: DeliveryNackRequeue, CanonicalEffect: CanonicalNone}, nil, false},
		{"transient", PrepareResult{Disposition: PrepareTransientInfrastructure, DeliveryAction: DeliveryNackRequeue, CanonicalEffect: CanonicalNone}, nil, false},
		{"duplicate", PrepareResult{Disposition: PrepareDuplicateStaleTerminal, DeliveryAction: DeliveryAckRelease, CanonicalEffect: CanonicalNone}, nil, false},
		{"terminal repair", PrepareResult{Disposition: PrepareCanonicalRepairedTerminal, DeliveryAction: DeliveryAckRelease, CanonicalEffect: CanonicalTerminalRepairCommitted}, nil, false},
		{"retry repair", PrepareResult{Disposition: PrepareCanonicalRepairedRetry, DeliveryAction: DeliveryAckRelease, CanonicalEffect: CanonicalRetryRepairCommitted}, nil, false},
		{"poison", PrepareResult{Disposition: PreparePoisonPermanentInvalidMessage, DeliveryAction: DeliveryConfirmDLQThenAck, CanonicalEffect: CanonicalPoisonRepairSettled}, nil, false},
		{"unlocatable poison", PrepareResult{Disposition: PreparePoisonPermanentInvalidMessage, DeliveryAction: DeliveryConfirmDLQThenAck, CanonicalEffect: CanonicalNone}, nil, false},
		{"claimed missing task", PrepareResult{Disposition: PrepareClaimed, DeliveryAction: DeliveryPromoteThenAckRun, CanonicalEffect: CanonicalClaimCommitted, Claim: claim}, nil, true},
		{"claimed missing ref", PrepareResult{Disposition: PrepareClaimed, DeliveryAction: DeliveryPromoteThenAckRun, CanonicalEffect: CanonicalClaimCommitted}, prepared, true},
		{"claimed bad generation", PrepareResult{Disposition: PrepareClaimed, DeliveryAction: DeliveryPromoteThenAckRun, CanonicalEffect: CanonicalClaimCommitted, Claim: &ClaimRef{TenantID: "tenant-a", TaskID: "42"}}, prepared, true},
		{"non-claimed has task", PrepareResult{Disposition: PrepareCapacityDeferred, DeliveryAction: DeliveryNackRequeue, CanonicalEffect: CanonicalNone}, prepared, true},
		{"wrong action", PrepareResult{Disposition: PrepareCanonicalRepairedRetry, DeliveryAction: DeliveryNackRequeue, CanonicalEffect: CanonicalRetryRepairCommitted}, nil, true},
		{"wrong effect", PrepareResult{Disposition: PrepareDuplicateStaleTerminal, DeliveryAction: DeliveryAckRelease, CanonicalEffect: CanonicalClaimCommitted}, nil, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.result.Validate(test.prepared)
			if (err != nil) != test.wantInvalid {
				t.Fatalf("Validate() error = %v, wantInvalid=%v", err, test.wantInvalid)
			}
		})
	}
}

func TestPrepareResultValidateForBindsClaimToRequest(t *testing.T) {
	message := validTestMessage()
	body := message
	header := message.DispatchToken
	hash, err := TenantHash(message.Resource, message.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareRequest{
		Message:            &message,
		BodyCandidate:      &body,
		HeaderToken:        &header,
		HeaderFacts:        validHeaderFacts(header),
		RegisteredResource: message.Resource,
		QueueTenantHash:    hash,
		PublishAttemptID:   testOperationID,
		RawBody:            rawBody,
	}
	prepared := testPreparedTask{}
	result := PrepareResult{
		Disposition:     PrepareClaimed,
		DeliveryAction:  DeliveryPromoteThenAckRun,
		CanonicalEffect: CanonicalClaimCommitted,
		Claim: &ClaimRef{
			TenantID:        message.TenantID,
			TaskID:          message.TaskID,
			ClaimGeneration: message.DispatchToken.Generation,
		},
	}
	if err := result.ValidateFor(request, prepared); err != nil {
		t.Fatalf("ValidateFor() error = %v", err)
	}

	mutations := map[string]func(*PrepareRequest, *PrepareResult){
		"missing executable message": func(request *PrepareRequest, _ *PrepareResult) {
			request.Message = nil
		},
		"tenant mismatch": func(_ *PrepareRequest, result *PrepareResult) {
			result.Claim.TenantID = "tenant-b"
		},
		"task mismatch": func(_ *PrepareRequest, result *PrepareResult) {
			result.Claim.TaskID = "43"
		},
		"generation mismatch": func(_ *PrepareRequest, result *PrepareResult) {
			result.Claim.ClaimGeneration++
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidateRequest := request
			candidateResult := result
			claim := *result.Claim
			candidateResult.Claim = &claim
			mutate(&candidateRequest, &candidateResult)
			if err := candidateResult.ValidateFor(candidateRequest, prepared); err == nil {
				t.Fatal("ValidateFor() error = nil")
			}
		})
	}

	repairOnly := request
	repairOnly.Message = nil
	repairOnly.BodyCandidate = nil
	repairOnly.RawBody = []byte(`{}`)
	repairOnly.DecodeErrorCode = "invalid-body"
	poison := PrepareResult{
		Disposition:     PreparePoisonPermanentInvalidMessage,
		DeliveryAction:  DeliveryConfirmDLQThenAck,
		CanonicalEffect: CanonicalPoisonRepairSettled,
	}
	if err := poison.ValidateFor(repairOnly, nil); err != nil {
		t.Fatalf("repair-only poison ValidateFor() error = %v", err)
	}
	nonDLQ := PrepareResult{
		Disposition:     PrepareDuplicateStaleTerminal,
		DeliveryAction:  DeliveryAckRelease,
		CanonicalEffect: CanonicalNone,
	}
	if err := nonDLQ.ValidateFor(repairOnly, nil); !errors.Is(err, ErrInvalidPrepareResult) {
		t.Fatalf("repair-only non-DLQ ValidateFor() error = %v, want ErrInvalidPrepareResult", err)
	}
}

func TestPrepareRequestExecutableGateAndSafeJSON(t *testing.T) {
	message := validTestMessage()
	body := message
	header := message.DispatchToken
	hash, err := TenantHash(message.Resource, message.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareRequest{
		Message:            &message,
		BodyCandidate:      &body,
		HeaderToken:        &header,
		HeaderFacts:        validHeaderFacts(header),
		RegisteredResource: message.Resource,
		QueueTenantHash:    hash,
		PublishAttemptID:   testOperationID,
		RawBody:            rawBody,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	safe, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{message.TenantID, "task_id", `"raw_body":`, "body_candidate", "header_token"} {
		if strings.Contains(string(safe), secret) {
			t.Fatalf("PrepareRequest JSON leaked %q: %s", secret, safe)
		}
	}

	missingHeader := request
	missingHeader.HeaderToken = nil
	if err := missingHeader.Validate(); err == nil {
		t.Fatal("executable request without header accepted")
	}

	repairOnly := request
	repairOnly.Message = nil
	repairOnly.BodyCandidate = nil
	repairOnly.RawBody = []byte(`{}`)
	repairOnly.DecodeErrorCode = "invalid-body"
	if err := repairOnly.Validate(); err != nil {
		t.Fatalf("repair-only request rejected: %v", err)
	}
	silentlyDowngraded := request
	silentlyDowngraded.Message = nil
	if err := silentlyDowngraded.Validate(); err == nil {
		t.Fatal("canonical transport facts without executable message accepted")
	}
	mismatchToken := DispatchToken{Resource: header.Resource, TaskID: "43", Generation: header.Generation}
	crossFactMismatch := request
	crossFactMismatch.Message = nil
	crossFactMismatch.HeaderToken = &mismatchToken
	crossFactMismatch.HeaderFacts = validHeaderFacts(mismatchToken)
	if err := crossFactMismatch.Validate(); err != nil {
		t.Fatalf("repair-only body/header mismatch rejected: %v", err)
	}
	queueMismatch := request
	queueMismatch.Message = nil
	queueMismatch.QueueTenantHash = strings.Repeat("0", 64)
	if err := queueMismatch.Validate(); err != nil {
		t.Fatalf("repair-only queue mismatch rejected: %v", err)
	}

	wrongHash := request
	wrongHash.QueueTenantHash = strings.Repeat("0", 64)
	if err := wrongHash.Validate(); err == nil {
		t.Fatal("executable request with wrong queue tenant accepted")
	}
	oversizedClaim := request
	oversizedClaim.RawBody = make([]byte, MaxMessageBytes+1)
	if err := oversizedClaim.Validate(); err == nil {
		t.Fatal("executable request with oversized protocol body accepted")
	}
	corruptClaim := request
	corruptClaim.RawBody = []byte(`{}`)
	if err := corruptClaim.Validate(); err == nil {
		t.Fatal("executable request whose raw body mismatches its candidate accepted")
	}
	decodeFailedClaim := request
	decodeFailedClaim.DecodeErrorCode = "invalid-json"
	if err := decodeFailedClaim.Validate(); err == nil {
		t.Fatal("executable request with a decode error accepted")
	}
	missingAttemptClaim := request
	missingAttemptClaim.PublishAttemptID = ""
	if err := missingAttemptClaim.Validate(); err == nil {
		t.Fatal("executable request without a canonical publish attempt accepted")
	}
	mismatchedHeaderFacts := request
	mismatchedHeaderFacts.HeaderFacts = validHeaderFacts(DispatchToken{
		Resource: header.Resource, TaskID: "43", Generation: header.Generation,
	})
	if err := mismatchedHeaderFacts.Validate(); err == nil {
		t.Fatal("header token/facts mismatch accepted")
	}
	missingRepairHeaderToken := request
	missingRepairHeaderToken.Message = nil
	missingRepairHeaderToken.HeaderToken = nil
	if err := missingRepairHeaderToken.Validate(); err == nil {
		t.Fatal("missing registry-validated header token without an error accepted")
	}
	missingRepairHeaderToken.HeaderErrorCode = "unregistered-header-resource"
	if err := missingRepairHeaderToken.Validate(); err != nil {
		t.Fatalf("registry-rejected generic header facts were not preserved safely: %v", err)
	}
	registryRejectedBody := request
	registryRejectedBody.Message = nil
	registryRejectedBody.BodyCandidate = nil
	registryRejectedBody.DecodeErrorCode = "unregistered-body-resource"
	if err := registryRejectedBody.Validate(); err != nil {
		t.Fatalf("registry-rejected generic body was not preserved safely: %v", err)
	}
	invalidAttempt := request
	invalidAttempt.Message = nil
	invalidAttempt.PublishAttemptID = "attacker supplied text"
	invalidAttempt.PropertyErrorCode = "invalid-properties"
	if err := invalidAttempt.Validate(); err == nil {
		t.Fatal("non-canonical publish attempt retained")
	}
	incompleteHeadersWithoutError := request
	incompleteHeadersWithoutError.Message = nil
	incompleteHeadersWithoutError.HeaderFacts.ProtocolVersion = nil
	if err := incompleteHeadersWithoutError.Validate(); err == nil {
		t.Fatal("incomplete headers without an error code accepted")
	}
	incompleteHeadersWithoutError.HeaderErrorCode = "invalid-headers"
	if err := incompleteHeadersWithoutError.Validate(); err != nil {
		t.Fatalf("repair locator with an independent header error rejected: %v", err)
	}

	oversizedProtocolBody := repairOnly
	oversizedProtocolBody.BodyCandidate = nil
	oversizedProtocolBody.RawBody = make([]byte, MaxMessageBytes+1)
	oversizedProtocolBody.DecodeErrorCode = "body-too-large"
	if err := oversizedProtocolBody.Validate(); err != nil {
		t.Fatalf("bounded poison body rejected: %v", err)
	}
	poison := PrepareResult{
		Disposition:     PreparePoisonPermanentInvalidMessage,
		DeliveryAction:  DeliveryConfirmDLQThenAck,
		CanonicalEffect: CanonicalNone,
	}
	if err := poison.ValidateFor(oversizedProtocolBody, nil); err != nil {
		t.Fatalf("oversized protocol body could not reach poison/DLQ: %v", err)
	}
	forgedRepairLocator := request
	forgedRepairLocator.Message = nil
	forgedRepairLocator.RawBody = []byte(`{}`)
	forgedRepairLocator.DecodeErrorCode = ""
	if err := forgedRepairLocator.Validate(); err == nil {
		t.Fatal("repair-only body candidate detached from raw body accepted")
	}

	oversizedTransportBody := repairOnly
	oversizedTransportBody.BodyCandidate = nil
	oversizedTransportBody.RawBody = make([]byte, MaxRawDeliveryBytes+1)
	if err := oversizedTransportBody.Validate(); err == nil {
		t.Fatal("raw body beyond transport retention limit accepted")
	}
	omittedTransportBody := request
	omittedTransportBody.Message = nil
	omittedTransportBody.BodyCandidate = nil
	omittedTransportBody.RawBody = nil
	omittedTransportBody.RawBodyOmitted = true
	omittedTransportBody.RawBodySize = MaxRawDeliveryBytes + 1
	omittedTransportBody.RawBodySHA256 = strings.Repeat("a", 64)
	omittedTransportBody.DecodeErrorCode = "raw-body-too-large"
	if err := omittedTransportBody.Validate(); err != nil {
		t.Fatalf("bounded oversized-delivery metadata rejected: %v", err)
	}
	deadLetter := DeadLetterRequest{Delivery: omittedTransportBody, ReasonCode: "body-too-large"}
	if err := deadLetter.Validate(); err != nil {
		t.Fatalf("DeadLetterRequest.Validate() error = %v", err)
	}
	deadLetter.ReasonCode = "unsafe reason text"
	if err := deadLetter.Validate(); err == nil {
		t.Fatal("unsafe dead-letter reason accepted")
	}
}

func TestStableHeaderFactsPreserveRepairLocator(t *testing.T) {
	token := validTestMessage().DispatchToken
	facts := validHeaderFacts(token)
	if err := facts.Validate(); err != nil {
		t.Fatalf("StableHeaderFacts.Validate() error = %v", err)
	}
	if got, ok := facts.CompleteV1Token(); !ok || got != token {
		t.Fatalf("CompleteV1Token() = %#v, %v", got, ok)
	}
	facts.ProtocolVersion = nil
	if got, ok := facts.Token(); !ok || got != token {
		t.Fatalf("repair Token() = %#v, %v", got, ok)
	}
	if _, ok := facts.CompleteV1Token(); ok {
		t.Fatal("incomplete protocol facts authorized execution")
	}
}

func TestRecoveryPageCursorContractAndJSONRoundTrip(t *testing.T) {
	page := RecoveryPage[TenantRef]{
		Items:      []TenantRef{{TenantID: "tenant-a"}},
		NextCursor: "opaque:tenant-page:2",
		Done:       false,
	}
	if err := page.Validate("opaque:tenant-page:1", 2); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var got RecoveryPage[TenantRef]
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, page) {
		t.Fatalf("round trip = %#v, want %#v", got, page)
	}

	validEmptyIntermediate := RecoveryPage[TenantRef]{NextCursor: "opaque-next"}
	if err := validEmptyIntermediate.Validate("opaque-before", 1); err != nil {
		t.Fatalf("empty intermediate page rejected: %v", err)
	}
	tests := []struct {
		name  string
		page  RecoveryPage[TenantRef]
		after string
		limit int
	}{
		{"zero limit", page, "before", 0},
		{"excessive limit", page, "before", MaxRecoveryPageLimit + 1},
		{"too many items", page, "before", 0},
		{"unfinished empty cursor", RecoveryPage[TenantRef]{}, "before", 1},
		{"cursor did not advance", RecoveryPage[TenantRef]{NextCursor: "same"}, "same", 1},
		{"done cursor retained", RecoveryPage[TenantRef]{NextCursor: "next", Done: true}, "before", 1},
		{"control cursor", RecoveryPage[TenantRef]{NextCursor: "bad\nnext"}, "before", 1},
		{"invalid item", RecoveryPage[TenantRef]{Items: []TenantRef{{}}, Done: true}, "before", 1},
	}
	tests[2].page.Items = []TenantRef{{TenantID: "a"}, {TenantID: "b"}}
	tests[2].limit = 1
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.page.Validate(test.after, test.limit); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func assertRecoveryPageJSONRoundTrip[T any](t *testing.T, item T) {
	t.Helper()
	page := RecoveryPage[T]{Items: []T{item}, Done: true}
	if err := page.Validate("before", 1); err != nil {
		t.Fatalf("RecoveryPage[%T].Validate() error = %v", item, err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("json.Marshal(RecoveryPage[%T]) error = %v", item, err)
	}
	var got RecoveryPage[T]
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal(RecoveryPage[%T]) error = %v", item, err)
	}
	if !reflect.DeepEqual(got, page) {
		t.Fatalf("RecoveryPage[%T] round trip = %#v, want %#v", item, got, page)
	}
	if err := got.Validate("before", 1); err != nil {
		t.Fatalf("round-tripped RecoveryPage[%T].Validate() error = %v", item, err)
	}
}

func TestRecoveryPageReferenceDTOJSONRoundTrips(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 123000000, time.UTC)
	assertRecoveryPageJSONRoundTrip(t, TenantRef{TenantID: "tenant-a"})
	assertRecoveryPageJSONRoundTrip(t, DispatchedRef{TenantID: "tenant-a", Token: validTestMessage().DispatchToken})
	assertRecoveryPageJSONRoundTrip(t, RunningLease{
		TenantID: "tenant-a", TaskID: "42", ClaimGeneration: 7,
		LeaseUntil: now.Add(time.Minute), ObservedDBNow: now,
	})
	assertRecoveryPageJSONRoundTrip(t, ReservationRef{
		TenantID: "tenant-a", StableToken: "r:" + strings.Repeat("a", 64),
		ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli(),
	})
	assertRecoveryPageJSONRoundTrip(t, RedisKeyRef{Key: "bkcrab:fair:{rag.index}:known"})
}

func TestRecoveryReferenceDTOValidationAndRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 123000, time.UTC)
	values := []interface {
		Validate() error
	}{
		TenantRef{TenantID: "tenant-a"},
		DispatchedRef{TenantID: "tenant-a", Token: validTestMessage().DispatchToken},
		RunningLease{TenantID: "tenant-a", TaskID: "42", ClaimGeneration: 7, LeaseUntil: now.Add(time.Minute), ObservedDBNow: now},
		ReservationRef{TenantID: "tenant-a", StableToken: "r:" + strings.Repeat("a", 64), ExpiresAtUnixMS: now.Add(time.Minute).UnixMilli()},
		RedisKeyRef{Key: "bkcrab:fair:{rag.index}:known"},
	}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			t.Fatalf("%T.Validate() error = %v", value, err)
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw) == 0 {
			t.Fatalf("json.Marshal(%T) = %s, %v", value, raw, err)
		}
	}

	invalid := []interface{ Validate() error }{
		TenantRef{},
		DispatchedRef{TenantID: "tenant-a"},
		RunningLease{TenantID: "tenant-a", TaskID: "42", ClaimGeneration: 7, LeaseUntil: now, ObservedDBNow: now},
		ReservationRef{TenantID: "tenant-a", StableToken: "unstable", ExpiresAtUnixMS: 1},
		RedisKeyRef{},
	}
	for _, value := range invalid {
		if err := value.Validate(); err == nil {
			t.Fatalf("%T.Validate() error = nil", value)
		}
	}
}

func TestWriterAndRecoveryProgressValidation(t *testing.T) {
	writer := WriterIdentity{Fingerprint: testWriterA}
	if err := writer.Validate(); err != nil {
		t.Fatalf("WriterIdentity.Validate() error = %v", err)
	}
	readiness := WriterReadinessReport{Writer: writer, SchemaReady: true}
	if err := readiness.Validate(); err != nil || !readiness.Ready() {
		t.Fatalf("readiness = %#v, error = %v", readiness, err)
	}
	readiness.GenerationViolationCount = -1
	if err := readiness.Validate(); err == nil {
		t.Fatal("negative readiness aggregate accepted")
	}

	base := RecoveryProgress{
		Kind:         RecoveryNormal,
		HighWater:    "opaque-high-water",
		KnownTenants: RecoveryPassProgress{Cycle: 1, Complete: true},
		Dispatched:   RecoveryPassProgress{Cycle: 1, Complete: true},
		Running:      RecoveryPassProgress{Cycle: 2, Complete: true},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("normal progress rejected: %v", err)
	}

	rabbit := base
	rabbit.Kind = RecoveryRabbitRepair
	rabbit.OperationID = testOperationID
	rabbit.RabbitRepair = &RabbitRepairProgress{RepairHighWater: "repair-water", RepairPassComplete: true}
	if err := rabbit.Validate(); err != nil {
		t.Fatalf("rabbit progress rejected: %v", err)
	}

	writerProgress := base
	writerProgress.Kind = RecoveryWriterRebind
	writerProgress.OperationID = testOperationID
	writerProgress.WriterRebind = &WriterRebindProgress{OriginalWriterFingerprint: testWriterA, TargetWriterFingerprint: testWriterB}
	if err := writerProgress.Validate(); err != nil {
		t.Fatalf("writer progress rejected: %v", err)
	}

	force := base
	force.Kind = RecoveryForceRebuild
	force.OperationID = testOperationID
	force.ForceRebuild = &ForceRebuildProgress{NotBeforeUnixMS: time.Now().UnixMilli(), DeletePassComplete: true}
	if err := force.Validate(); err != nil {
		t.Fatalf("force progress rejected: %v", err)
	}

	invalid := []RecoveryProgress{
		{},
		{Kind: RecoveryNormal, OperationID: testOperationID},
		{Kind: RecoveryRabbitRepair, OperationID: testOperationID},
		{Kind: RecoveryWriterRebind, OperationID: testOperationID, WriterRebind: &WriterRebindProgress{OriginalWriterFingerprint: testWriterA, TargetWriterFingerprint: testWriterA}},
		{Kind: RecoveryForceRebuild, OperationID: testOperationID, ForceRebuild: &ForceRebuildProgress{}},
	}
	for i, progress := range invalid {
		if err := progress.Validate(); err == nil {
			t.Fatalf("invalid progress[%d] accepted: %#v", i, progress)
		}
	}
}

func TestRecoveryFenceAndControlSnapshotValidation(t *testing.T) {
	ready := RecoveryControlSnapshot{
		Present:           true,
		State:             ResourceReady,
		Epoch:             testOwnerToken,
		ProtocolVersion:   MessageVersion1,
		WriterFingerprint: testWriterA,
		Kind:              RecoveryNone,
	}
	if err := ready.Validate(); err != nil {
		t.Fatalf("ready snapshot rejected: %v", err)
	}
	completedReady := ready
	completedReady.LastCompletedOperationID = testOperationID
	if err := completedReady.Validate(); err == nil {
		t.Fatal("READY completion without last-completed kind accepted")
	}
	completedReady.LastCompletedOperationKind = RecoveryRabbitRepair
	if err := completedReady.Validate(); err != nil {
		t.Fatalf("READY completion with exact kind rejected: %v", err)
	}

	progress := RecoveryProgress{Kind: RecoveryNormal}
	recovering := RecoveryControlSnapshot{
		Present:           true,
		State:             ResourceRecovering,
		Epoch:             testOwnerToken,
		ProtocolVersion:   MessageVersion1,
		WriterFingerprint: testWriterA,
		Kind:              RecoveryNormal,
		Progress:          &progress,
	}
	if err := recovering.Validate(); err != nil {
		t.Fatalf("recovering snapshot rejected: %v", err)
	}

	if err := (ResourceFence{Epoch: testOwnerToken, WriterFingerprint: testWriterA}).Validate(); err != nil {
		t.Fatalf("resource fence rejected: %v", err)
	}
	if err := (RecoveryLock{OwnerToken: testOwnerToken}).Validate(); err != nil {
		t.Fatalf("recovery lock rejected: %v", err)
	}
	if err := (RecoveryFence{
		ResourceFence: ResourceFence{Epoch: testOwnerToken, WriterFingerprint: testWriterA},
		OwnerToken:    testOwnerToken,
		Kind:          RecoveryRabbitRepair,
		OperationID:   testOperationID,
	}).Validate(); err != nil {
		t.Fatalf("recovery fence rejected: %v", err)
	}

	invalidReady := ready
	invalidReady.Kind = RecoveryRabbitRepair
	if err := invalidReady.Validate(); err == nil {
		t.Fatal("READY with special kind accepted")
	}
	invalidRecovering := recovering
	invalidRecovering.Progress = nil
	if err := invalidRecovering.Validate(); err == nil {
		t.Fatal("RECOVERING without progress accepted")
	}
	missing := RecoveryControlSnapshot{}
	if err := missing.Validate(); err != nil {
		t.Fatalf("canonical missing snapshot rejected: %v", err)
	}
}

func TestRedisInspectionAndForceDeadlineContracts(t *testing.T) {
	standalone := RedisTopology{Mode: RedisDeploymentStandalone, WritablePrimary: true}
	if err := standalone.Validate(); err != nil || !standalone.SupportsFairQueue() {
		t.Fatalf("standalone primary = %#v, error = %v", standalone, err)
	}
	for _, topology := range []RedisTopology{
		{Mode: RedisDeploymentStandalone},
		{Mode: RedisDeploymentCluster, WritablePrimary: true},
	} {
		if err := topology.Validate(); err != nil || topology.SupportsFairQueue() {
			t.Fatalf("unsupported topology = %#v, error = %v", topology, err)
		}
	}
	if err := (RedisTopology{Mode: "sentinel"}).Validate(); err == nil {
		t.Fatal("unknown Redis topology accepted")
	}

	observed := time.Date(2026, 8, 2, 10, 0, 0, 123456000, time.UTC)
	minimumDelay := 1500 * time.Microsecond
	deadline, err := NewForceRebuildDeadline(observed, minimumDelay)
	if err != nil {
		t.Fatalf("NewForceRebuildDeadline() error = %v", err)
	}
	if deadline.NotBefore.Nanosecond() != 125000000 ||
		deadline.NotBefore.Before(observed.Add(minimumDelay)) {
		t.Fatalf("deadline shortened or not rounded up: %#v", deadline)
	}
	if err := deadline.Validate(minimumDelay); err != nil {
		t.Fatalf("ForceRebuildDeadline.Validate() error = %v", err)
	}
	shortened := deadline
	shortened.NotBefore = shortened.NotBefore.Add(-time.Millisecond)
	if err := shortened.Validate(minimumDelay); err == nil {
		t.Fatal("shortened force rebuild deadline accepted")
	}
	if _, err := NewForceRebuildDeadline(observed.Add(time.Nanosecond), minimumDelay); err == nil {
		t.Fatal("non-Redis-time precision accepted")
	}
}

func TestBoundaryErrorCategoriesSupportErrorsIs(t *testing.T) {
	categories := []error{
		ErrDependencyUnavailable,
		ErrUnsupportedTopology,
		ErrResourceNotReady,
		ErrFenceMismatch,
		ErrAuthoritativeWriterMismatch,
		ErrAuthoritativeStateCorrupt,
		ErrRecoveryOwnerStale,
		ErrCoordinationCorrupt,
		ErrPublishUnroutable,
		ErrPublishUnconfirmed,
	}
	for index, category := range categories {
		wrapped := errors.Join(errors.New("adapter detail"), category)
		if !errors.Is(wrapped, category) {
			t.Fatalf("wrapped category %v is not discoverable", category)
		}
		for otherIndex, other := range categories {
			if index != otherIndex && errors.Is(wrapped, other) {
				t.Fatalf("category %v aliases %v", category, other)
			}
		}
	}
}

func validOperationRecord(kind RecoveryKind) RecoveryOperationRecord {
	now := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	record := RecoveryOperationRecord{
		Resource:                 "rag.index",
		OperationID:              testOperationID,
		Kind:                     kind,
		Phase:                    OperationActive,
		CurrentWriterFingerprint: testWriterA,
		Version:                  1,
		CreatedAt:                now,
		UpdatedAt:                now,
	}
	switch kind {
	case RecoveryWriterRebind:
		record.CurrentWriterFingerprint = testWriterB
		record.OriginalWriterFingerprint = testWriterA
		record.TargetWriterFingerprint = testWriterB
	case RecoveryForceRebuild:
		notBefore := now.Add(time.Minute)
		record.ForceNotBefore = &notBefore
	}
	return record
}

func TestRecoveryOperationRecordProposalAndPersistedValidation(t *testing.T) {
	for _, kind := range []RecoveryKind{RecoveryRabbitRepair, RecoveryWriterRebind, RecoveryForceRebuild} {
		t.Run(string(kind), func(t *testing.T) {
			record := validOperationRecord(kind)
			if err := record.ValidatePersisted(); err != nil {
				t.Fatalf("ValidatePersisted() error = %v", err)
			}
			proposal := record
			proposal.Phase = ""
			proposal.Version = 0
			proposal.CreatedAt = time.Time{}
			proposal.UpdatedAt = time.Time{}
			if err := proposal.ValidateProposal(); err != nil {
				t.Fatalf("ValidateProposal() error = %v", err)
			}
		})
	}

	rabbitReady := validOperationRecord(RecoveryRabbitRepair)
	highWater := "opaque-repair-water"
	rabbitReady.RepairHighWater = &highWater
	rabbitReady.RepairPassComplete = true
	rabbitReady.Phase = OperationReadyCommitted
	if err := rabbitReady.ValidatePersisted(); err != nil {
		t.Fatalf("ready rabbit record rejected: %v", err)
	}

	forceReady := validOperationRecord(RecoveryForceRebuild)
	forceReady.ForceDeletePassComplete = true
	forceReady.Phase = OperationCompleted
	if err := forceReady.ValidatePersisted(); err != nil {
		t.Fatalf("completed force record rejected: %v", err)
	}

	invalid := []RecoveryOperationRecord{
		validOperationRecord(RecoveryNormal),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryRabbitRepair)
			r.Version = 0
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryRabbitRepair)
			r.Phase = OperationReadyCommitted
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryWriterRebind)
			r.TargetWriterFingerprint = testWriterA
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryForceRebuild)
			r.ForceNotBefore = nil
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryForceRebuild)
			value := r.ForceNotBefore.Add(time.Nanosecond)
			r.ForceNotBefore = &value
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryForceRebuild)
			value := r.ForceNotBefore.In(time.FixedZone("non-utc", int(time.Hour/time.Second)))
			r.ForceNotBefore = &value
			return r
		}(),
		func() RecoveryOperationRecord {
			r := validOperationRecord(RecoveryRabbitRepair)
			r.UpdatedAt = r.CreatedAt.Add(-time.Microsecond)
			return r
		}(),
	}
	for i, record := range invalid {
		if err := record.ValidatePersisted(); err == nil {
			t.Fatalf("invalid record[%d] accepted: %#v", i, record)
		}
	}
}

func TestOperatorAttestationsAndAggregateReports(t *testing.T) {
	attestations := []interface{ Validate() error }{
		RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true},
		WriterRebindAttestation{OldWriterFenced: true, ResourceRuntimesStopped: true, NewWriterAuthoritative: true},
		ForceRebuildAttestation{DiscardRedisCoordinationState: true},
	}
	for _, attestation := range attestations {
		if err := attestation.Validate(); err != nil {
			t.Fatalf("%T.Validate() error = %v", attestation, err)
		}
	}
	invalid := []interface{ Validate() error }{
		RabbitRepairAttestation{OldBrokerIsolated: true},
		WriterRebindAttestation{OldWriterFenced: true, ResourceRuntimesStopped: true},
		ForceRebuildAttestation{},
	}
	for _, attestation := range invalid {
		if err := attestation.Validate(); err == nil {
			t.Fatalf("%T missing confirmation accepted", attestation)
		}
	}

	reports := []any{
		RabbitRepairReport{Resource: "rag.index", Writer: WriterIdentity{Fingerprint: testWriterA}, CandidateCount: 12, PagesScanned: 2, Operation: OperationSummary{Present: true, Kind: RecoveryRabbitRepair, Phase: OperationActive}},
		WriterRebindReport{Resource: "rag.index", ExpectedOldWriterFingerprint: testWriterA, TargetWriter: WriterIdentity{Fingerprint: testWriterB}, Readiness: WriterReadinessReport{Writer: WriterIdentity{Fingerprint: testWriterB}, SchemaReady: true}, ValidRunningCount: 0},
		ForceRebuildReport{Resource: "rag.index", Writer: WriterIdentity{Fingerprint: testWriterA}, ControlPresent: true, ControlState: ResourceRecovering, ControlKind: RecoveryNormal, StandaloneRedis: true, CurrentWriterVerified: true, RabbitTruthSourceVerified: true, RebuildableKeyCount: 9, PagesScanned: 1},
	}
	for _, report := range reports {
		validator, ok := report.(interface{ Validate() error })
		if !ok {
			t.Fatalf("%T has no validation contract", report)
		}
		if err := validator.Validate(); err != nil {
			t.Fatalf("%T.Validate() error = %v", report, err)
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("json.Marshal(%T) error = %v", report, err)
		}
		for _, forbidden := range []string{"operation_id", "high_water", "cursor", "task_id", "tenant_id", "epoch", "owner_token"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%T leaked %q: %s", report, forbidden, raw)
			}
		}
	}

	invalidReports := []interface{ Validate() error }{
		RabbitRepairReport{Resource: "rag.index", Writer: WriterIdentity{Fingerprint: testWriterA}, CandidateCount: -1},
		WriterRebindReport{Resource: "rag.index", ExpectedOldWriterFingerprint: testWriterA, TargetWriter: WriterIdentity{Fingerprint: testWriterB}, Readiness: WriterReadinessReport{Writer: WriterIdentity{Fingerprint: testWriterA}}},
		ForceRebuildReport{Resource: "rag.index", Writer: WriterIdentity{Fingerprint: testWriterA}, ControlState: ResourceReady},
	}
	for _, report := range invalidReports {
		if err := report.Validate(); err == nil {
			t.Fatalf("invalid %T accepted", report)
		}
	}
}

type fakeDispatchSource struct{}

func (fakeDispatchSource) ListDispatchCandidates(context.Context, string, int) ([]DispatchCandidate, string, error) {
	return nil, "", nil
}
func (fakeDispatchSource) GetDispatchableByID(context.Context, string) (DispatchCandidate, bool, error) {
	return DispatchCandidate{}, false, nil
}
func (fakeDispatchSource) MarkDispatched(context.Context, DispatchCandidate) (bool, error) {
	return false, nil
}

type fakeExpiredRearmSource struct{}

func (fakeExpiredRearmSource) RearmExpiredPage(context.Context, string, int) ([]DispatchCandidate, string, error) {
	return nil, "", nil
}

type fakeTaskPreparer struct{}

func (fakeTaskPreparer) Prepare(context.Context, PrepareRequest) (PreparedTask, PrepareResult, error) {
	return nil, PrepareResult{}, nil
}

type fakeRecoverySource struct{}

func (fakeRecoverySource) CaptureHighWater(context.Context) (string, error) { return "", nil }
func (fakeRecoverySource) ListKnownTenants(context.Context, string, string, int) (RecoveryPage[TenantRef], error) {
	return RecoveryPage[TenantRef]{}, nil
}
func (fakeRecoverySource) ListDispatched(context.Context, string, string, int) (RecoveryPage[DispatchedRef], error) {
	return RecoveryPage[DispatchedRef]{}, nil
}
func (fakeRecoverySource) ListValidRunning(context.Context, string, string, int) (RecoveryPage[RunningLease], error) {
	return RecoveryPage[RunningLease]{}, nil
}

type fakeWriterRebindSource struct{ fakeRecoverySource }

func (fakeWriterRebindSource) ReadWriterIdentity(context.Context) (WriterIdentity, error) {
	return WriterIdentity{}, nil
}
func (fakeWriterRebindSource) CheckSchemaAndInvariants(context.Context) (WriterReadinessReport, error) {
	return WriterReadinessReport{}, nil
}
func (fakeWriterRebindSource) CountValidRunning(context.Context) (int64, error) { return 0, nil }

type fakeBrokerRepairSource struct{}

func (fakeBrokerRepairSource) CaptureRepairHighWater(context.Context) (string, error) { return "", nil }
func (fakeBrokerRepairSource) ListBrokerBackedCandidates(context.Context, string, string, int) (RecoveryPage[DispatchCandidate], error) {
	return RecoveryPage[DispatchCandidate]{}, nil
}
func (fakeBrokerRepairSource) RearmAfterBrokerLoss(context.Context, DispatchCandidate) (DispatchCandidate, bool, error) {
	return DispatchCandidate{}, false, nil
}

type fakeStartSession struct{}

func (fakeStartSession) Read(context.Context) (RecoveryOperationRecord, bool, error) {
	return RecoveryOperationRecord{}, false, nil
}
func (fakeStartSession) BeginSpecial(context.Context, *RecoveryOperationRecord, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}

type fakeOperationJournal struct{}

func (fakeOperationJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	return RecoveryOperationRecord{}, false, nil
}
func (fakeOperationJournal) WithStartFence(context.Context, string, string, func(OperationStartSession) error) error {
	return nil
}
func (fakeOperationJournal) SetRepairHighWater(context.Context, RecoveryOperationRecord, string) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}
func (fakeOperationJournal) MarkRepairPassComplete(context.Context, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}
func (fakeOperationJournal) MarkForceDeletePassComplete(context.Context, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}
func (fakeOperationJournal) CommitReady(context.Context, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}
func (fakeOperationJournal) Complete(context.Context, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, nil
}

type fakeDelivery struct{}

func (fakeDelivery) Request() PrepareRequest          { return PrepareRequest{} }
func (fakeDelivery) Ack(context.Context) error        { return nil }
func (fakeDelivery) Nack(context.Context, bool) error { return nil }

type fakeRabbitClient struct{}

func (fakeRabbitClient) EnsureTenantTopology(context.Context, string, string) error { return nil }
func (fakeRabbitClient) PublishMandatoryConfirmed(context.Context, Message) (PublishReceipt, error) {
	return PublishReceipt{}, nil
}
func (fakeRabbitClient) GetOne(context.Context, string, string) (Delivery, bool, error) {
	return nil, false, nil
}
func (fakeRabbitClient) ReadyDepth(context.Context, string, string) (int64, error) { return 0, nil }
func (fakeRabbitClient) PublishDeadLetterConfirmed(context.Context, DeadLetterRequest) (PublishReceipt, error) {
	return PublishReceipt{}, nil
}
func (fakeRabbitClient) Close() error { return nil }

type fakeRedisInspector struct{}

func (fakeRedisInspector) InspectRedisTopology(context.Context) (RedisTopology, error) {
	return RedisTopology{}, nil
}

type fakeCoordinator struct{}

func (fakeCoordinator) ObserveReadyFence(context.Context, string, string) (ResourceFence, error) {
	return ResourceFence{}, nil
}
func (fakeCoordinator) CheckReadyFence(context.Context, string, ResourceFence) error  { return nil }
func (fakeCoordinator) Activate(context.Context, string, ResourceFence, string) error { return nil }
func (fakeCoordinator) EnsureKnownTenant(context.Context, string, ResourceFence, string) error {
	return nil
}
func (fakeCoordinator) EnsureActive(context.Context, string, ResourceFence, string) error { return nil }
func (fakeCoordinator) NextTurn(context.Context, string, ResourceFence, ProcessingTurnToken, time.Duration) (ProcessingTurn, bool, error) {
	return ProcessingTurn{}, false, nil
}
func (fakeCoordinator) RotateOrDeactivate(context.Context, string, ResourceFence, ProcessingTurnToken, uint64, bool) error {
	return nil
}
func (fakeCoordinator) AcquireProvisional(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
	return "", nil
}
func (fakeCoordinator) BindReservation(context.Context, string, ResourceFence, string, string, string, time.Duration) error {
	return nil
}
func (fakeCoordinator) RenewStable(context.Context, string, ResourceFence, string, string, time.Duration) error {
	return nil
}
func (fakeCoordinator) Release(context.Context, string, ResourceFence, string, string) error {
	return nil
}
func (fakeCoordinator) ListReadyStableInflight(context.Context, string, ResourceFence, string, int) (RecoveryPage[ReservationRef], error) {
	return RecoveryPage[ReservationRef]{}, nil
}
func (fakeCoordinator) EnsureReadyStableInflight(context.Context, string, ResourceFence, string, string, time.Duration) error {
	return nil
}
func (fakeCoordinator) ReapExpiredTurnsAndProvisionals(context.Context, string, ResourceFence, int) (RecoveryCleanupResult, error) {
	return RecoveryCleanupResult{}, nil
}
func (fakeCoordinator) AcquireRecoveryLock(context.Context, string, string, time.Duration) (RecoveryLock, error) {
	return RecoveryLock{}, nil
}
func (fakeCoordinator) CheckRecoveryLock(context.Context, string, RecoveryLock) error { return nil }
func (fakeCoordinator) RenewRecoveryLock(context.Context, string, RecoveryLock, time.Duration) error {
	return nil
}
func (fakeCoordinator) InspectRecoveryStart(context.Context, string, RecoveryLock) (RecoveryControlSnapshot, error) {
	return RecoveryControlSnapshot{}, nil
}
func (fakeCoordinator) ComputeForceRebuildDeadlineWithLock(context.Context, string, RecoveryLock, time.Duration) (ForceRebuildDeadline, error) {
	return ForceRebuildDeadline{}, nil
}
func (fakeCoordinator) ReleaseRecoveryLock(context.Context, string, RecoveryLock) error { return nil }
func (fakeCoordinator) BeginRecoveryWithLock(context.Context, string, string, RecoveryLock, time.Duration) (RecoveryFence, error) {
	return RecoveryFence{}, nil
}
func (fakeCoordinator) BeginRabbitRepairWithLock(context.Context, string, string, string, RecoveryLock, time.Duration) (RecoveryFence, error) {
	return RecoveryFence{}, nil
}
func (fakeCoordinator) BeginWriterRebindWithLock(context.Context, string, string, string, string, RecoveryLock, time.Duration) (RecoveryFence, error) {
	return RecoveryFence{}, nil
}
func (fakeCoordinator) BeginForceRebuildWithLock(context.Context, string, string, string, int64, RecoveryLock, time.Duration) (RecoveryFence, error) {
	return RecoveryFence{}, nil
}
func (fakeCoordinator) RenewRecovery(context.Context, string, RecoveryFence, time.Duration) error {
	return nil
}
func (fakeCoordinator) RecoveryReapExpired(context.Context, string, RecoveryFence, int) (RecoveryCleanupResult, error) {
	return RecoveryCleanupResult{}, nil
}
func (fakeCoordinator) ResetResource(context.Context, string, RecoveryFence) error { return nil }
func (fakeCoordinator) SetRecoveryHighWater(context.Context, string, RecoveryFence, string) error {
	return nil
}
func (fakeCoordinator) SetRabbitRepairHighWater(context.Context, string, RecoveryFence, string) error {
	return nil
}
func (fakeCoordinator) MarkRabbitRepairPassComplete(context.Context, string, RecoveryFence) error {
	return nil
}
func (fakeCoordinator) MarkForceDeletePassComplete(context.Context, string, RecoveryFence) error {
	return nil
}
func (fakeCoordinator) RestoreKnownTenant(context.Context, string, RecoveryFence, string) error {
	return nil
}
func (fakeCoordinator) RestoreActiveTenant(context.Context, string, RecoveryFence, string) error {
	return nil
}
func (fakeCoordinator) RestoreInflight(context.Context, string, RecoveryFence, string, string, time.Duration) error {
	return nil
}
func (fakeCoordinator) ListRecoveryStableInflight(context.Context, string, RecoveryFence, string, int) (RecoveryPage[ReservationRef], error) {
	return RecoveryPage[ReservationRef]{}, nil
}
func (fakeCoordinator) DeleteRecoveryStableInflight(context.Context, string, RecoveryFence, ReservationRef) error {
	return nil
}
func (fakeCoordinator) MarkRecoveryPass(context.Context, string, RecoveryFence, RecoveryPass, uint64, bool, int64) error {
	return nil
}
func (fakeCoordinator) ListOwnedResourceKeys(context.Context, string, RecoveryFence, string, int) (RecoveryPage[RedisKeyRef], error) {
	return RecoveryPage[RedisKeyRef]{}, nil
}
func (fakeCoordinator) DeleteOwnedResourceKeys(context.Context, string, RecoveryFence, []RedisKeyRef) error {
	return nil
}
func (fakeCoordinator) FinishRecovery(context.Context, string, RecoveryFence) error { return nil }

var (
	_ DispatchSource        = fakeDispatchSource{}
	_ ExpiredRearmSource    = fakeExpiredRearmSource{}
	_ TaskPreparer          = fakeTaskPreparer{}
	_ PreparedTask          = testPreparedTask{}
	_ RecoverySource        = fakeRecoverySource{}
	_ BrokerRepairSource    = fakeBrokerRepairSource{}
	_ WriterRebindSource    = fakeWriterRebindSource{}
	_ OperationStartSession = fakeStartSession{}
	_ OperationJournal      = fakeOperationJournal{}
	_ Delivery              = fakeDelivery{}
	_ RabbitClient          = fakeRabbitClient{}
	_ RedisInspector        = fakeRedisInspector{}
	_ Coordinator           = fakeCoordinator{}
)
