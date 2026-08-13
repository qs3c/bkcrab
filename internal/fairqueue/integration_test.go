package fairqueue

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	redislib "github.com/redis/go-redis/v9"
)

func TestIntegrationFairnessBorrowReturnAndNoStarvation(t *testing.T) {
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	running := map[string]int{"A": 0}
	global := 0
	for i := 0; i < 4; i++ {
		decision, err := DecideReservation(limits, CapacitySnapshot{GlobalInflight: global, TenantInflight: running["A"], ActiveTenants: 1})
		if err != nil || (decision != ReservationRegular && decision != ReservationBorrowed) {
			t.Fatalf("A borrow %d: decision=%s err=%v", i, decision, err)
		}
		running["A"]++
		global++
	}
	if running["A"] != 4 || global != 4 {
		t.Fatalf("exclusive borrower state=%v global=%d", running, global)
	}

	// B arrives while A owns borrowed slots. Releasing one A makes B's base
	// reservation eligible while A is denied further borrowing under competition.
	running["A"]--
	global--
	if decision, _ := DecideReservation(limits, CapacitySnapshot{global, running["A"], 2}); decision != ReservationDeniedCompetition {
		t.Fatalf("competing A decision=%s, want denied competition", decision)
	}
	if decision, _ := DecideReservation(limits, CapacitySnapshot{global, 0, 2}); decision != ReservationRegular {
		t.Fatalf("new B decision=%s, want regular", decision)
	}
	running["B"] = 1
	global++
	running["A"]--
	global--
	running["B"]++
	global++
	if running["A"] != 2 || running["B"] != 2 || global != 4 {
		t.Fatalf("competitive convergence=%v global=%d, want 2/2 and 4", running, global)
	}

	// With three continuously active tenants, round-robin base grants make
	// progress for every tenant and never cross the global fence.
	running = map[string]int{"A": 0, "B": 0, "C": 0}
	global = 0
	started := map[string]int{}
	order := []string{"A", "B", "C", "A", "B", "C", "A", "B", "C", "A", "B", "C"}
	var inFlight []string
	for _, tenant := range order {
		if global == limits.GlobalConcurrency {
			finished := inFlight[0]
			inFlight = inFlight[1:]
			running[finished]--
			global--
		}
		decision, err := DecideReservation(limits, CapacitySnapshot{global, running[tenant], 3})
		if err != nil || decision != ReservationRegular {
			t.Fatalf("tenant %s: decision=%s err=%v state=%v", tenant, decision, err, running)
		}
		running[tenant]++
		started[tenant]++
		global++
		inFlight = append(inFlight, tenant)
		if global > limits.GlobalConcurrency {
			t.Fatalf("global=%d exceeds %d", global, limits.GlobalConcurrency)
		}
	}
	for _, tenant := range []string{"A", "B", "C"} {
		if started[tenant] == 0 {
			t.Fatalf("tenant %s starved: %v", tenant, started)
		}
	}
}

func TestIntegrationDependencies(t *testing.T) {
	mysqlDSN := strings.TrimSpace(os.Getenv("BKCRAB_TEST_MYSQL_DSN"))
	redisAddr := strings.TrimSpace(os.Getenv("BKCRAB_TEST_REDIS_ADDR"))
	rabbitURL := strings.TrimSpace(os.Getenv("BKCRAB_TEST_RABBITMQ_URL"))
	if mysqlDSN == "" || redisAddr == "" || rabbitURL == "" {
		t.Skip("set BKCRAB_TEST_MYSQL_DSN, BKCRAB_TEST_REDIS_ADDR, and BKCRAB_TEST_RABBITMQ_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	namespace := fmt.Sprintf("bkcrab.integration.%d", time.Now().UnixNano())

	db, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	defer db.Close()
	var serverUUID, database string
	if err := db.QueryRowContext(ctx, "SELECT @@server_uuid, DATABASE()").Scan(&serverUUID, &database); err != nil {
		t.Fatalf("read MySQL writer identity: %v", err)
	}
	if strings.TrimSpace(serverUUID) == "" || strings.TrimSpace(database) == "" {
		t.Fatalf("empty MySQL writer identity uuid=%q database=%q", serverUUID, database)
	}

	redisClient := redislib.NewClient(&redislib.Options{Addr: redisAddr, Password: os.Getenv("BKCRAB_TEST_REDIS_PASSWORD"), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = redisClient.Close() })
	redisKey := namespace + ":probe"
	if err := redisClient.Set(ctx, redisKey, "1", time.Minute).Err(); err != nil {
		t.Fatalf("write Redis namespace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = redisClient.Del(cleanupCtx, redisKey).Err()
	})

	connection, err := amqp.DialConfig(rabbitURL, amqp.Config{Dial: amqp.DefaultDial(10 * time.Second)})
	if err != nil {
		t.Fatalf("dial RabbitMQ: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("open RabbitMQ channel: %v", err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	exchange := namespace + ".exchange"
	queue := namespace + ".queue"
	if err := channel.ExchangeDeclare(exchange, "direct", true, false, false, false, nil); err != nil {
		t.Fatalf("declare RabbitMQ exchange: %v", err)
	}
	if _, err := channel.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare RabbitMQ queue: %v", err)
	}
	if err := channel.QueueBind(queue, "probe", exchange, false, nil); err != nil {
		t.Fatalf("bind RabbitMQ queue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = channel.QueueDelete(queue, false, false, false)
		_ = channel.ExchangeDelete(exchange, false, false)
	})
}
