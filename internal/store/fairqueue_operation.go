package store

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The operation journal deliberately lives in store rather than fairqueue. It
// is the MySQL source of truth which survives loss of all Redis coordination
// state, and it must be usable before the transport-independent package is
// introduced.

type FairQueueOperationKind string

const (
	FairQueueOperationRabbitRepair FairQueueOperationKind = "RABBIT_REPAIR"
	FairQueueOperationWriterRebind FairQueueOperationKind = "WRITER_REBIND"
	FairQueueOperationForceRebuild FairQueueOperationKind = "FORCE_REBUILD"
)

type FairQueueOperationPhase string

const (
	FairQueueOperationActive         FairQueueOperationPhase = "ACTIVE"
	FairQueueOperationReadyCommitted FairQueueOperationPhase = "READY_COMMITTED"
	FairQueueOperationCompleted      FairQueueOperationPhase = "COMPLETED"
)

var (
	ErrFairQueueMySQLRequired        = errors.New("store: fair queue safety operations require MySQL")
	ErrFairQueueWriterMismatch       = errors.New("store: fair queue authoritative writer identity mismatch")
	ErrFairQueueUnsafeConnection     = errors.New("store: fair queue MySQL connection state is unsafe")
	ErrFairQueueStartLockUnavailable = errors.New("store: fair queue operation-start lock unavailable")
	ErrFairQueueOperationInvalid     = errors.New("store: invalid fair queue operation record")
	ErrFairQueueOperationConflict    = errors.New("store: fair queue operation compare-and-swap conflict")
)

const (
	fairQueueOperationStartLockTimeout = 5 * time.Second
	fairQueueOperationCleanupTimeout   = 2 * time.Second
)

var (
	fairQueueResourcePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)
	lowerHex32Pattern        = regexp.MustCompile(`^[0-9a-f]{32}$`)
	lowerHex64Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// FairQueueOperationRecord is one CAS version of the current special
// operation for a resource. NORMAL recovery never creates a record.
type FairQueueOperationRecord struct {
	Resource                  string
	OperationID               string
	Kind                      FairQueueOperationKind
	Phase                     FairQueueOperationPhase
	CurrentWriterFingerprint  string
	OriginalWriterFingerprint string
	TargetWriterFingerprint   string
	RepairHighWater           *string
	RepairPassComplete        bool
	ForceNotBefore            *time.Time
	ForceDeletePassComplete   bool
	Version                   int64
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// FairQueueOperationProposal contains immutable start parameters. Version and
// timestamps are assigned by MySQL. Rabbit repair high-water is captured later;
// force not-before is fixed at start and cannot be shortened by a resume.
type FairQueueOperationProposal struct {
	Resource                  string
	OperationID               string
	Kind                      FairQueueOperationKind
	CurrentWriterFingerprint  string
	OriginalWriterFingerprint string
	TargetWriterFingerprint   string
	ForceNotBefore            *time.Time
}

// NewFairQueueOperationID returns an unpredictable 128-bit identifier encoded
// in its canonical lowercase form.
func NewFairQueueOperationID() (string, error) {
	var value [16]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate fair queue operation id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func validFairQueueResource(resource string) bool {
	return resource != "" && len(resource) <= 120 &&
		resource == strings.TrimSpace(resource) && fairQueueResourcePattern.MatchString(resource)
}

func validFairQueueHighWater(highWater string) bool {
	if highWater == "" || len(highWater) > 191 || highWater != strings.TrimSpace(highWater) {
		return false
	}
	for i := 0; i < len(highWater); i++ {
		if highWater[i] < 0x20 || highWater[i] > 0x7e {
			return false
		}
	}
	return true
}

func validFairQueueForceNotBefore(value *time.Time) bool {
	return value != nil && !value.IsZero() && value.Location() == time.UTC &&
		value.Nanosecond()%int(time.Millisecond) == 0
}

func validateFairQueueOperationProposal(proposal FairQueueOperationProposal) error {
	if !validFairQueueResource(proposal.Resource) ||
		!lowerHex32Pattern.MatchString(proposal.OperationID) ||
		!lowerHex64Pattern.MatchString(proposal.CurrentWriterFingerprint) {
		return ErrFairQueueOperationInvalid
	}
	switch proposal.Kind {
	case FairQueueOperationRabbitRepair:
		if proposal.OriginalWriterFingerprint != "" || proposal.TargetWriterFingerprint != "" ||
			proposal.ForceNotBefore != nil {
			return ErrFairQueueOperationInvalid
		}
	case FairQueueOperationWriterRebind:
		if !lowerHex64Pattern.MatchString(proposal.OriginalWriterFingerprint) ||
			!lowerHex64Pattern.MatchString(proposal.TargetWriterFingerprint) ||
			proposal.OriginalWriterFingerprint == proposal.TargetWriterFingerprint ||
			proposal.CurrentWriterFingerprint != proposal.TargetWriterFingerprint ||
			proposal.ForceNotBefore != nil {
			return ErrFairQueueOperationInvalid
		}
	case FairQueueOperationForceRebuild:
		if proposal.OriginalWriterFingerprint != "" || proposal.TargetWriterFingerprint != "" ||
			!validFairQueueForceNotBefore(proposal.ForceNotBefore) {
			return ErrFairQueueOperationInvalid
		}
	default:
		return ErrFairQueueOperationInvalid
	}
	return nil
}

func validateFairQueueOperationRecord(record FairQueueOperationRecord) error {
	proposal := FairQueueOperationProposal{
		Resource: record.Resource, OperationID: record.OperationID, Kind: record.Kind,
		CurrentWriterFingerprint:  record.CurrentWriterFingerprint,
		OriginalWriterFingerprint: record.OriginalWriterFingerprint,
		TargetWriterFingerprint:   record.TargetWriterFingerprint,
		ForceNotBefore:            record.ForceNotBefore,
	}
	if err := validateFairQueueOperationProposal(proposal); err != nil || record.Version <= 0 ||
		record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return ErrFairQueueOperationInvalid
	}
	switch record.Phase {
	case FairQueueOperationActive, FairQueueOperationReadyCommitted, FairQueueOperationCompleted:
	default:
		return ErrFairQueueOperationInvalid
	}
	if record.RepairHighWater != nil && !validFairQueueHighWater(*record.RepairHighWater) {
		return ErrFairQueueOperationInvalid
	}
	switch record.Kind {
	case FairQueueOperationRabbitRepair:
		if record.ForceDeletePassComplete ||
			(record.RepairPassComplete && record.RepairHighWater == nil) {
			return ErrFairQueueOperationInvalid
		}
	case FairQueueOperationWriterRebind:
		if record.RepairHighWater != nil || record.RepairPassComplete || record.ForceDeletePassComplete {
			return ErrFairQueueOperationInvalid
		}
	case FairQueueOperationForceRebuild:
		if record.RepairHighWater != nil || record.RepairPassComplete {
			return ErrFairQueueOperationInvalid
		}
	}
	if record.Phase != FairQueueOperationActive {
		switch record.Kind {
		case FairQueueOperationRabbitRepair:
			if record.RepairHighWater == nil || !record.RepairPassComplete {
				return ErrFairQueueOperationInvalid
			}
		case FairQueueOperationForceRebuild:
			if !record.ForceDeletePassComplete {
				return ErrFairQueueOperationInvalid
			}
		}
	}
	return nil
}

func proposalMatchesRecord(proposal FairQueueOperationProposal, record FairQueueOperationRecord) bool {
	return proposal.Resource == record.Resource && proposal.OperationID == record.OperationID &&
		proposal.Kind == record.Kind &&
		proposal.CurrentWriterFingerprint == record.CurrentWriterFingerprint &&
		proposal.OriginalWriterFingerprint == record.OriginalWriterFingerprint &&
		proposal.TargetWriterFingerprint == record.TargetWriterFingerprint &&
		equalOptionalTime(proposal.ForceNotBefore, record.ForceNotBefore)
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return normalizeFairQueueMySQLTime(*left).Equal(normalizeFairQueueMySQLTime(*right))
}

func normalizeFairQueueMySQLTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func normalizeFairQueueOperationProposal(proposal FairQueueOperationProposal) FairQueueOperationProposal {
	if proposal.ForceNotBefore != nil {
		value := normalizeFairQueueMySQLTime(*proposal.ForceNotBefore)
		proposal.ForceNotBefore = &value
	}
	return proposal
}

func sameFairQueueOperationIdentity(expected, current FairQueueOperationRecord) bool {
	return expected.Resource == current.Resource && expected.OperationID == current.OperationID &&
		expected.Kind == current.Kind &&
		expected.CurrentWriterFingerprint == current.CurrentWriterFingerprint &&
		expected.OriginalWriterFingerprint == current.OriginalWriterFingerprint &&
		expected.TargetWriterFingerprint == current.TargetWriterFingerprint &&
		equalOptionalTime(expected.ForceNotBefore, current.ForceNotBefore)
}

func fairQueueOperationPhaseRank(phase FairQueueOperationPhase) int {
	switch phase {
	case FairQueueOperationActive:
		return 1
	case FairQueueOperationReadyCommitted:
		return 2
	case FairQueueOperationCompleted:
		return 3
	default:
		return 0
	}
}

func fairQueueOperationMonotonicFrom(expected, current FairQueueOperationRecord) bool {
	if !sameFairQueueOperationIdentity(expected, current) || current.Version < expected.Version ||
		fairQueueOperationPhaseRank(current.Phase) < fairQueueOperationPhaseRank(expected.Phase) {
		return false
	}
	if expected.RepairHighWater != nil && !equalOptionalString(expected.RepairHighWater, current.RepairHighWater) {
		return false
	}
	if expected.RepairPassComplete && !current.RepairPassComplete {
		return false
	}
	if expected.ForceDeletePassComplete && !current.ForceDeletePassComplete {
		return false
	}
	return true
}

func fairQueueOperationCASMatches(left, right FairQueueOperationRecord) bool {
	return left.Resource == right.Resource && left.OperationID == right.OperationID &&
		left.Kind == right.Kind && left.Phase == right.Phase &&
		left.CurrentWriterFingerprint == right.CurrentWriterFingerprint &&
		left.OriginalWriterFingerprint == right.OriginalWriterFingerprint &&
		left.TargetWriterFingerprint == right.TargetWriterFingerprint &&
		equalOptionalString(left.RepairHighWater, right.RepairHighWater) &&
		left.RepairPassComplete == right.RepairPassComplete &&
		equalOptionalTime(left.ForceNotBefore, right.ForceNotBefore) &&
		left.ForceDeletePassComplete == right.ForceDeletePassComplete &&
		left.Version == right.Version
}

type fairQueueMySQLIdentity struct {
	serverUUID   string
	database     string
	connectionID uint64
	fingerprint  string
}

type fairQueueIdentityQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readFairQueueMySQLIdentity(ctx context.Context, queryer fairQueueIdentityQuerier) (fairQueueMySQLIdentity, error) {
	var identity fairQueueMySQLIdentity
	if err := queryer.QueryRowContext(ctx,
		`SELECT @@server_uuid, DATABASE(), CONNECTION_ID()`).Scan(
		&identity.serverUUID, &identity.database, &identity.connectionID); err != nil {
		return fairQueueMySQLIdentity{}, fmt.Errorf("read fair queue MySQL identity: %w", err)
	}
	if strings.TrimSpace(identity.serverUUID) == "" || strings.TrimSpace(identity.database) == "" ||
		identity.connectionID == 0 {
		return fairQueueMySQLIdentity{}, ErrFairQueueWriterMismatch
	}
	digest := sha256.Sum256([]byte(identity.serverUUID + "\x00" + identity.database))
	identity.fingerprint = hex.EncodeToString(digest[:])
	return identity, nil
}

func sameFairQueueMySQLSession(left, right fairQueueMySQLIdentity) bool {
	return left.serverUUID == right.serverUUID && left.database == right.database &&
		left.connectionID == right.connectionID && left.fingerprint == right.fingerprint
}

func readFairQueueMySQLIdentityWithLockOwner(
	ctx context.Context,
	queryer fairQueueIdentityQuerier,
	lockName string,
) (fairQueueMySQLIdentity, sql.NullInt64, error) {
	var identity fairQueueMySQLIdentity
	var lockOwner sql.NullInt64
	if err := queryer.QueryRowContext(ctx,
		`SELECT @@server_uuid, DATABASE(), CONNECTION_ID(), IS_USED_LOCK(?)`, lockName).Scan(
		&identity.serverUUID, &identity.database, &identity.connectionID, &lockOwner); err != nil {
		return fairQueueMySQLIdentity{}, sql.NullInt64{}, fmt.Errorf("read fair queue MySQL lock identity: %w", err)
	}
	if strings.TrimSpace(identity.serverUUID) == "" || strings.TrimSpace(identity.database) == "" ||
		identity.connectionID == 0 {
		return fairQueueMySQLIdentity{}, lockOwner, ErrFairQueueWriterMismatch
	}
	digest := sha256.Sum256([]byte(identity.serverUUID + "\x00" + identity.database))
	identity.fingerprint = hex.EncodeToString(digest[:])
	return identity, lockOwner, nil
}

func verifyFairQueueMySQLSession(
	ctx context.Context,
	queryer fairQueueIdentityQuerier,
	expected fairQueueMySQLIdentity,
	lockName string,
) error {
	if !lowerHex64Pattern.MatchString(expected.fingerprint) || expected.connectionID == 0 {
		return ErrFairQueueUnsafeConnection
	}
	if lockName == "" {
		current, err := readFairQueueMySQLIdentity(ctx, queryer)
		if err != nil {
			return errors.Join(ErrFairQueueUnsafeConnection, err)
		}
		if !sameFairQueueMySQLSession(expected, current) {
			return ErrFairQueueWriterMismatch
		}
		return nil
	}
	current, lockOwner, err := readFairQueueMySQLIdentityWithLockOwner(ctx, queryer, lockName)
	if err != nil {
		return errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	if !sameFairQueueMySQLSession(expected, current) {
		return ErrFairQueueWriterMismatch
	}
	if !lockOwner.Valid || lockOwner.Int64 <= 0 || uint64(lockOwner.Int64) != expected.connectionID {
		return errors.Join(ErrFairQueueUnsafeConnection, ErrFairQueueStartLockUnavailable)
	}
	return nil
}

func fairQueueOperationStartLockName(database, resource string) string {
	digest := sha256.Sum256([]byte(database + "\x00" + resource))
	return "bkcrab:fqo:" + hex.EncodeToString(digest[:])[:48]
}

func discardFairQueueSQLConn(conn *sql.Conn) error {
	if conn == nil {
		return nil
	}
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if err == nil || errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	return fmt.Errorf("discard unsafe fair queue MySQL connection: %w", err)
}

func fairQueueConnectionErrorRequiresDiscard(err error) bool {
	return errors.Is(err, ErrFairQueueUnsafeConnection) ||
		errors.Is(err, ErrFairQueueWriterMismatch) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (d *DBStore) withFairQueueExpectedWriterConn(
	ctx context.Context,
	expectedWriter string,
	fn func(*sql.Conn, fairQueueMySQLIdentity) error,
) (err error) {
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return ErrFairQueueMySQLRequired
	}
	if !lowerHex64Pattern.MatchString(expectedWriter) {
		return ErrFairQueueWriterMismatch
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	identity, err := readFairQueueMySQLIdentity(ctx, conn)
	if err != nil || identity.fingerprint != expectedWriter {
		discardErr := discardFairQueueSQLConn(conn)
		if err != nil {
			return errors.Join(ErrFairQueueUnsafeConnection, err, discardErr)
		}
		return errors.Join(ErrFairQueueWriterMismatch, discardErr)
	}
	callbackErr := fn(conn, identity)

	// Always revalidate through an independent bounded context. In particular,
	// callback cancellation or an inner safety error must not bypass the final
	// session check and return an uncertain physical connection to the pool.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
	after, verifyErr := readFairQueueMySQLIdentity(verifyCtx, conn)
	verifyCancel()
	var safetyErr error
	if verifyErr != nil {
		safetyErr = errors.Join(ErrFairQueueUnsafeConnection, verifyErr)
	} else if !sameFairQueueMySQLSession(identity, after) {
		safetyErr = ErrFairQueueWriterMismatch
	}
	if safetyErr != nil || fairQueueConnectionErrorRequiresDiscard(callbackErr) {
		if discardErr := discardFairQueueSQLConn(conn); discardErr != nil {
			safetyErr = errors.Join(safetyErr, ErrFairQueueUnsafeConnection, discardErr)
		}
	}
	return errors.Join(callbackErr, safetyErr)
}

func (d *DBStore) discoverFairQueueWriterIdentity(ctx context.Context) (identity fairQueueMySQLIdentity, err error) {
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return identity, ErrFairQueueMySQLRequired
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return identity, err
	}
	defer func() { _ = conn.Close() }()
	identity, err = readFairQueueMySQLIdentity(ctx, conn)
	if err != nil {
		_ = discardFairQueueSQLConn(conn)
		return fairQueueMySQLIdentity{}, errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	after, verifyErr := readFairQueueMySQLIdentity(ctx, conn)
	if verifyErr != nil || !sameFairQueueMySQLSession(identity, after) {
		_ = discardFairQueueSQLConn(conn)
		if verifyErr != nil {
			return fairQueueMySQLIdentity{}, errors.Join(ErrFairQueueUnsafeConnection, verifyErr)
		}
		return fairQueueMySQLIdentity{}, ErrFairQueueWriterMismatch
	}
	return identity, nil
}

// FairQueueOperationStartSession confines start reads and writes to the same
// physical MySQL connection which owns the operation-start named lock.
type FairQueueOperationStartSession struct {
	store          *DBStore
	conn           *sql.Conn
	resource       string
	expectedWriter string
	identity       fairQueueMySQLIdentity
	lockName       string
}

// WithFairQueueOperationStartFence serializes every NORMAL/special start for a
// resource. The callback is the only place where callers may acquire the Redis
// raw recovery lock. MySQL is always acquired first.
func (d *DBStore) WithFairQueueOperationStartFence(
	ctx context.Context,
	resource, expectedWriter string,
	fn func(*FairQueueOperationStartSession) error,
) (err error) {
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return ErrFairQueueMySQLRequired
	}
	if !validFairQueueResource(resource) || !lowerHex64Pattern.MatchString(expectedWriter) || fn == nil {
		return ErrFairQueueOperationInvalid
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	identity, err := readFairQueueMySQLIdentity(ctx, conn)
	if err != nil || identity.fingerprint != expectedWriter {
		discardErr := discardFairQueueSQLConn(conn)
		if err != nil {
			return errors.Join(ErrFairQueueUnsafeConnection, err, discardErr)
		}
		return errors.Join(ErrFairQueueWriterMismatch, discardErr)
	}
	lockName := fairQueueOperationStartLockName(identity.database, resource)
	timeoutSeconds := fairQueueOperationStartLockTimeout.Seconds()
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline).Seconds()
		if remaining <= 0 {
			discardErr := discardFairQueueSQLConn(conn)
			return errors.Join(context.DeadlineExceeded, discardErr)
		}
		if remaining < timeoutSeconds {
			timeoutSeconds = remaining
		}
	}
	var acquired sql.NullInt64
	if lockErr := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeoutSeconds).Scan(&acquired); lockErr != nil || !acquired.Valid || acquired.Int64 != 1 {
		discardErr := discardFairQueueSQLConn(conn)
		if lockErr != nil {
			return errors.Join(ErrFairQueueStartLockUnavailable, lockErr, discardErr)
		}
		return errors.Join(ErrFairQueueStartLockUnavailable, discardErr)
	}

	// From this point every exit, including a panic, attempts exactly one release
	// with a cleanup context independent of the request context.
	lockHeld := true
	discardAfterRelease := false
	defer func() {
		panicValue := recover()
		if lockHeld {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
			var released sql.NullInt64
			releaseErr := conn.QueryRowContext(cleanupCtx, `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
			cancel()
			lockHeld = false
			if releaseErr != nil || !released.Valid || released.Int64 != 1 {
				discardAfterRelease = true
				unsafeErr := ErrFairQueueUnsafeConnection
				if releaseErr != nil {
					unsafeErr = errors.Join(unsafeErr, releaseErr)
				}
				err = errors.Join(err, unsafeErr)
			}
		}
		if discardAfterRelease {
			err = errors.Join(err, discardFairQueueSQLConn(conn))
		}
		if panicValue != nil {
			panic(panicValue)
		}
	}()

	if err := verifyFairQueueMySQLSession(ctx, conn, identity, lockName); err != nil {
		discardErr := discardFairQueueSQLConn(conn)
		lockHeld = false // physical close releases the session-level lock
		return errors.Join(err, discardErr)
	}
	err = fn(&FairQueueOperationStartSession{
		store: d, conn: conn, resource: resource, expectedWriter: expectedWriter,
		identity: identity, lockName: lockName,
	})
	if fairQueueConnectionErrorRequiresDiscard(err) {
		discardAfterRelease = true
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
	verifyErr := verifyFairQueueMySQLSession(verifyCtx, conn, identity, lockName)
	verifyCancel()
	if verifyErr != nil {
		discardAfterRelease = true
		err = errors.Join(err, verifyErr)
	}
	return err
}

const fairQueueOperationColumns = `resource,operation_id,kind,phase,
	current_writer_fingerprint,original_writer_fingerprint,target_writer_fingerprint,
	repair_high_water,repair_pass_complete,force_not_before,force_delete_pass_complete,
	version,created_at,updated_at`

type fairQueueOperationScanner interface {
	Scan(...any) error
}

func scanFairQueueOperation(scanner fairQueueOperationScanner) (FairQueueOperationRecord, error) {
	var record FairQueueOperationRecord
	var repairHighWater sql.NullString
	var forceNotBefore sql.NullTime
	if err := scanner.Scan(
		&record.Resource, &record.OperationID, &record.Kind, &record.Phase,
		&record.CurrentWriterFingerprint, &record.OriginalWriterFingerprint,
		&record.TargetWriterFingerprint, &repairHighWater, &record.RepairPassComplete,
		&forceNotBefore, &record.ForceDeletePassComplete, &record.Version,
		&record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return FairQueueOperationRecord{}, err
	}
	if repairHighWater.Valid {
		value := repairHighWater.String
		record.RepairHighWater = &value
	}
	if forceNotBefore.Valid {
		value := forceNotBefore.Time
		record.ForceNotBefore = &value
	}
	if err := validateFairQueueOperationRecord(record); err != nil {
		return FairQueueOperationRecord{}, err
	}
	return record, nil
}

type fairQueueOperationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readFairQueueOperation(
	ctx context.Context,
	queryer fairQueueOperationQueryer,
	resource string,
) (FairQueueOperationRecord, bool, error) {
	record, err := scanFairQueueOperation(queryer.QueryRowContext(ctx,
		`SELECT `+fairQueueOperationColumns+` FROM fairqueue_resource_operations WHERE resource=?`,
		resource))
	if errors.Is(err, sql.ErrNoRows) {
		return FairQueueOperationRecord{}, false, nil
	}
	if err != nil {
		return FairQueueOperationRecord{}, false, err
	}
	return record, true, nil
}

func (d *DBStore) ReadFairQueueOperation(
	ctx context.Context,
	resource, expectedWriter string,
) (record FairQueueOperationRecord, found bool, err error) {
	if !validFairQueueResource(resource) || !lowerHex64Pattern.MatchString(expectedWriter) {
		return record, false, ErrFairQueueOperationInvalid
	}
	err = d.withFairQueueExpectedWriterConn(ctx, expectedWriter,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var readErr error
			record, found, readErr = readFairQueueOperation(ctx, conn, resource)
			return readErr
		})
	return record, found, err
}

func (session *FairQueueOperationStartSession) Read(
	ctx context.Context,
) (record FairQueueOperationRecord, found bool, err error) {
	if session == nil || session.conn == nil || session.store == nil {
		return FairQueueOperationRecord{}, false, ErrFairQueueOperationInvalid
	}
	if !validFairQueueResource(session.resource) {
		return FairQueueOperationRecord{}, false, ErrFairQueueOperationInvalid
	}
	if verifyErr := session.verifyExpectedWriter(ctx, session.conn); verifyErr != nil {
		return FairQueueOperationRecord{}, false, session.discardUnsafeConnection(verifyErr)
	}
	defer func() {
		verifyCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
		verifyErr := session.verifyExpectedWriter(verifyCtx, session.conn)
		cancel()
		if verifyErr != nil {
			record = FairQueueOperationRecord{}
			found = false
			err = errors.Join(err, session.discardUnsafeConnection(verifyErr))
		}
	}()
	return readFairQueueOperation(ctx, session.conn, session.resource)
}

// ReadResource is the resource-bound form used by the operation adapter. Read
// remains intentionally unavailable without a resource to prevent a caller
// from using an untrusted row to choose its lock scope.
func (session *FairQueueOperationStartSession) ReadResource(
	ctx context.Context,
	resource string,
) (FairQueueOperationRecord, bool, error) {
	if session == nil || session.conn == nil || resource != session.resource || !validFairQueueResource(resource) {
		return FairQueueOperationRecord{}, false, ErrFairQueueOperationInvalid
	}
	return session.Read(ctx)
}

func optionalStringArgument(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalTimeArgument(value *time.Time) any {
	if value == nil {
		return nil
	}
	return normalizeFairQueueMySQLTime(*value)
}

func fairQueueOperationExpectedArgs(record FairQueueOperationRecord) []any {
	highWater := optionalStringArgument(record.RepairHighWater)
	notBefore := optionalTimeArgument(record.ForceNotBefore)
	return []any{
		record.Resource, record.OperationID, string(record.Kind), string(record.Phase),
		record.CurrentWriterFingerprint, record.OriginalWriterFingerprint,
		record.TargetWriterFingerprint,
		highWater, highWater, record.RepairPassComplete,
		notBefore, notBefore, record.ForceDeletePassComplete, record.Version,
	}
}

const fairQueueOperationExpectedWhere = `resource=? AND operation_id=? AND kind=? AND phase=?
	AND current_writer_fingerprint=? AND original_writer_fingerprint=? AND target_writer_fingerprint=?
	AND (repair_high_water=? OR (repair_high_water IS NULL AND ? IS NULL))
	AND repair_pass_complete=?
	AND (force_not_before=? OR (force_not_before IS NULL AND ? IS NULL))
	AND force_delete_pass_complete=? AND version=?`

func (session *FairQueueOperationStartSession) verifyExpectedWriter(ctx context.Context, queryer fairQueueIdentityQuerier) error {
	if session.store.dialect != mysqlDialect {
		// Unit tests exercise the SQL state machine with SQLite. Public entrypoints
		// reject non-MySQL stores before a session can be constructed.
		return nil
	}
	if session.identity.fingerprint != session.expectedWriter {
		return ErrFairQueueUnsafeConnection
	}
	return verifyFairQueueMySQLSession(ctx, queryer, session.identity, session.lockName)
}

func (session *FairQueueOperationStartSession) discardUnsafeConnection(err error) error {
	if err == nil || session == nil {
		return err
	}
	// A start-fenced session must let its owner attempt exactly one bounded
	// RELEASE_LOCK before discarding. The outer fence observes this typed safety
	// error, performs that release attempt, and then physically evicts the conn.
	if session.lockName != "" {
		return err
	}
	return errors.Join(err, discardFairQueueSQLConn(session.conn))
}

func (session *FairQueueOperationStartSession) insertProposal(
	ctx context.Context,
	proposal FairQueueOperationProposal,
) (FairQueueOperationRecord, error) {
	if err := session.verifyExpectedWriter(ctx, session.conn); err != nil {
		return FairQueueOperationRecord{}, err
	}
	tx, err := session.conn.BeginTx(ctx, nil)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	defer tx.Rollback()
	nowExpr := session.store.ragNowExpr()
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO fairqueue_resource_operations (
		resource,operation_id,kind,phase,current_writer_fingerprint,
		original_writer_fingerprint,target_writer_fingerprint,repair_high_water,
		repair_pass_complete,force_not_before,force_delete_pass_complete,
		version,created_at,updated_at)
		VALUES (?,?,?,'ACTIVE',?,?,?,NULL,FALSE,?,FALSE,1,%s,%s)`, nowExpr, nowExpr),
		proposal.Resource, proposal.OperationID, string(proposal.Kind),
		proposal.CurrentWriterFingerprint, proposal.OriginalWriterFingerprint,
		proposal.TargetWriterFingerprint, optionalTimeArgument(proposal.ForceNotBefore))
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	if err := session.verifyExpectedWriter(ctx, tx); err != nil {
		return FairQueueOperationRecord{}, err
	}
	record, found, err := readFairQueueOperation(ctx, tx, proposal.Resource)
	if err != nil || !found {
		return FairQueueOperationRecord{}, errors.Join(ErrFairQueueOperationConflict, err)
	}
	if err := session.verifyExpectedWriter(ctx, tx); err != nil {
		return FairQueueOperationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return FairQueueOperationRecord{}, errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	return record, nil
}

func (session *FairQueueOperationStartSession) updateExpected(
	ctx context.Context,
	expected FairQueueOperationRecord,
	setClause string,
	setArgs []any,
	desired func(FairQueueOperationRecord) bool,
) (FairQueueOperationRecord, error) {
	if err := validateFairQueueOperationRecord(expected); err != nil ||
		expected.CurrentWriterFingerprint != session.expectedWriter {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	if err := session.verifyExpectedWriter(ctx, session.conn); err != nil {
		return FairQueueOperationRecord{}, err
	}
	current, found, err := readFairQueueOperation(ctx, session.conn, expected.Resource)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	if !found {
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	validOutcome := func(record FairQueueOperationRecord) bool {
		if fairQueueOperationMonotonicFrom(expected, record) {
			return true
		}
		// The sole non-monotonic use of this primitive replaces a COMPLETED
		// resource row with a new operation under the old row's full CAS.
		return expected.Phase == FairQueueOperationCompleted &&
			record.Resource == expected.Resource && record.OperationID != expected.OperationID
	}
	if desired(current) && validOutcome(current) {
		return current, nil
	}
	if !fairQueueOperationCASMatches(expected, current) {
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	tx, err := session.conn.BeginTx(ctx, nil)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	defer tx.Rollback()
	args := append(append([]any{}, setArgs...), fairQueueOperationExpectedArgs(expected)...)
	result, err := tx.ExecContext(ctx, `UPDATE fairqueue_resource_operations SET `+setClause+
		`,version=version+1,updated_at=`+session.store.ragNowExpr()+` WHERE `+
		fairQueueOperationExpectedWhere, args...)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	if rows == 1 {
		if err := session.verifyExpectedWriter(ctx, tx); err != nil {
			return FairQueueOperationRecord{}, err
		}
		record, found, err := readFairQueueOperation(ctx, tx, expected.Resource)
		if err != nil || !found || !desired(record) || !validOutcome(record) {
			return FairQueueOperationRecord{}, errors.Join(ErrFairQueueOperationConflict, err)
		}
		if err := session.verifyExpectedWriter(ctx, tx); err != nil {
			return FairQueueOperationRecord{}, err
		}
		if err := tx.Commit(); err != nil {
			return FairQueueOperationRecord{}, errors.Join(ErrFairQueueUnsafeConnection, err)
		}
		return record, nil
	}
	if rows != 0 {
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	_ = tx.Rollback()
	current, found, readErr := readFairQueueOperation(ctx, session.conn, expected.Resource)
	if readErr == nil && found && desired(current) && validOutcome(current) {
		return current, nil // exact repeated request: idempotent, zero mutation
	}
	return FairQueueOperationRecord{}, errors.Join(ErrFairQueueOperationConflict, readErr)
}

// BeginSpecial creates a new ACTIVE operation, resumes the same immutable
// operation, or replaces a COMPLETED row using its full expected CAS version.
func (session *FairQueueOperationStartSession) BeginSpecial(
	ctx context.Context,
	expected *FairQueueOperationRecord,
	proposal FairQueueOperationProposal,
) (record FairQueueOperationRecord, err error) {
	if session == nil || session.conn == nil || session.store == nil {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	if verifyErr := session.verifyExpectedWriter(ctx, session.conn); verifyErr != nil {
		return FairQueueOperationRecord{}, session.discardUnsafeConnection(verifyErr)
	}
	defer func() {
		verifyCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
		verifyErr := session.verifyExpectedWriter(verifyCtx, session.conn)
		cancel()
		if verifyErr != nil {
			record = FairQueueOperationRecord{}
			err = errors.Join(err, session.discardUnsafeConnection(verifyErr))
		}
	}()
	if err := validateFairQueueOperationProposal(proposal); err != nil ||
		proposal.Resource != session.resource ||
		proposal.CurrentWriterFingerprint != session.expectedWriter {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	proposal = normalizeFairQueueOperationProposal(proposal)
	current, found, err := readFairQueueOperation(ctx, session.conn, proposal.Resource)
	if err != nil {
		return FairQueueOperationRecord{}, err
	}
	if expected == nil {
		if !found {
			return session.insertProposal(ctx, proposal)
		}
		if proposalMatchesRecord(proposal, current) && current.Phase != FairQueueOperationCompleted {
			return current, nil
		}
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	if err := validateFairQueueOperationRecord(*expected); err != nil {
		return FairQueueOperationRecord{}, err
	}
	if !found || !fairQueueOperationCASMatches(current, *expected) {
		if found && proposalMatchesRecord(proposal, current) && current.Phase != FairQueueOperationCompleted {
			return current, nil
		}
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	if current.Phase != FairQueueOperationCompleted {
		if proposalMatchesRecord(proposal, current) {
			return current, nil
		}
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	if proposal.OperationID == current.OperationID {
		return FairQueueOperationRecord{}, ErrFairQueueOperationConflict
	}
	return session.updateExpected(ctx, current,
		`operation_id=?,kind=?,phase='ACTIVE',current_writer_fingerprint=?,
		 original_writer_fingerprint=?,target_writer_fingerprint=?,repair_high_water=NULL,
		 repair_pass_complete=FALSE,force_not_before=?,force_delete_pass_complete=FALSE,
		 created_at=`+session.store.ragNowExpr(),
		[]any{proposal.OperationID, string(proposal.Kind), proposal.CurrentWriterFingerprint,
			proposal.OriginalWriterFingerprint, proposal.TargetWriterFingerprint,
			optionalTimeArgument(proposal.ForceNotBefore)},
		func(record FairQueueOperationRecord) bool {
			return proposalMatchesRecord(proposal, record) && record.Phase == FairQueueOperationActive
		})
}

func (d *DBStore) mutateFairQueueOperation(
	ctx context.Context,
	expected FairQueueOperationRecord,
	fn func(*FairQueueOperationStartSession) (FairQueueOperationRecord, error),
) (record FairQueueOperationRecord, err error) {
	err = d.withFairQueueExpectedWriterConn(ctx, expected.CurrentWriterFingerprint,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			var mutationErr error
			record, mutationErr = fn(&FairQueueOperationStartSession{
				store: d, conn: conn, resource: expected.Resource,
				expectedWriter: expected.CurrentWriterFingerprint, identity: identity,
			})
			return mutationErr
		})
	return record, err
}

func (d *DBStore) SetFairQueueOperationRepairHighWater(
	ctx context.Context,
	expected FairQueueOperationRecord,
	highWater string,
) (FairQueueOperationRecord, error) {
	if expected.Kind != FairQueueOperationRabbitRepair || expected.Phase != FairQueueOperationActive ||
		(expected.RepairHighWater != nil && *expected.RepairHighWater != highWater) ||
		!validFairQueueHighWater(highWater) {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	return d.mutateFairQueueOperation(ctx, expected,
		func(session *FairQueueOperationStartSession) (FairQueueOperationRecord, error) {
			return session.updateExpected(ctx, expected, `repair_high_water=?`, []any{highWater},
				func(record FairQueueOperationRecord) bool {
					return record.OperationID == expected.OperationID &&
						record.RepairHighWater != nil && *record.RepairHighWater == highWater
				})
		})
}

func (d *DBStore) MarkFairQueueOperationRepairPassComplete(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if expected.Kind != FairQueueOperationRabbitRepair || expected.Phase != FairQueueOperationActive ||
		expected.RepairHighWater == nil {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	return d.mutateFairQueueOperation(ctx, expected,
		func(session *FairQueueOperationStartSession) (FairQueueOperationRecord, error) {
			return session.updateExpected(ctx, expected, `repair_pass_complete=TRUE`, nil,
				func(record FairQueueOperationRecord) bool {
					return record.OperationID == expected.OperationID && record.RepairPassComplete
				})
		})
}

func (d *DBStore) MarkFairQueueOperationForceDeletePassComplete(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if expected.Kind != FairQueueOperationForceRebuild || expected.Phase != FairQueueOperationActive ||
		expected.ForceNotBefore == nil {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	return d.mutateFairQueueOperation(ctx, expected,
		func(session *FairQueueOperationStartSession) (FairQueueOperationRecord, error) {
			return session.updateExpected(ctx, expected, `force_delete_pass_complete=TRUE`, nil,
				func(record FairQueueOperationRecord) bool {
					return record.OperationID == expected.OperationID && record.ForceDeletePassComplete
				})
		})
}

func fairQueueOperationReady(record FairQueueOperationRecord) bool {
	if record.Phase != FairQueueOperationActive {
		return false
	}
	switch record.Kind {
	case FairQueueOperationRabbitRepair:
		return record.RepairHighWater != nil && record.RepairPassComplete
	case FairQueueOperationWriterRebind:
		return true
	case FairQueueOperationForceRebuild:
		return record.ForceNotBefore != nil && record.ForceDeletePassComplete
	default:
		return false
	}
}

func (d *DBStore) CommitFairQueueOperationReady(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if expected.Phase == FairQueueOperationActive && !fairQueueOperationReady(expected) {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	if expected.Phase != FairQueueOperationActive &&
		expected.Phase != FairQueueOperationReadyCommitted &&
		expected.Phase != FairQueueOperationCompleted {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	return d.mutateFairQueueOperation(ctx, expected,
		func(session *FairQueueOperationStartSession) (FairQueueOperationRecord, error) {
			return session.updateExpected(ctx, expected, `phase='READY_COMMITTED'`, nil,
				func(record FairQueueOperationRecord) bool {
					return record.OperationID == expected.OperationID &&
						fairQueueOperationPhaseRank(record.Phase) >=
							fairQueueOperationPhaseRank(FairQueueOperationReadyCommitted)
				})
		})
}

func (d *DBStore) CompleteFairQueueOperation(
	ctx context.Context,
	expected FairQueueOperationRecord,
) (FairQueueOperationRecord, error) {
	if expected.Phase != FairQueueOperationReadyCommitted && expected.Phase != FairQueueOperationCompleted {
		return FairQueueOperationRecord{}, ErrFairQueueOperationInvalid
	}
	return d.mutateFairQueueOperation(ctx, expected,
		func(session *FairQueueOperationStartSession) (FairQueueOperationRecord, error) {
			return session.updateExpected(ctx, expected, `phase='COMPLETED'`, nil,
				func(record FairQueueOperationRecord) bool {
					return record.OperationID == expected.OperationID &&
						record.Phase == FairQueueOperationCompleted
				})
		})
}
