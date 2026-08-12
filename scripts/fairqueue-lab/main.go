// Command fairqueue-lab creates isolated users and RAG documents through the
// public HTTP API, samples the fair queue while they run, and emits a JSON
// report. It is an operator tool: credentials are accepted only through
// environment variables and are never written to the report.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type labConfig struct {
	BaseURL          string
	AdminLogin       string
	Users            int
	DocumentsPerUser int
	DocumentBytes    int
	PollInterval     time.Duration
	Timeout          time.Duration
	Keep             bool
	Output           string
	ExpectedGlobal   int64
	ExpectedPerUser  int64
	RedisAddr        string
	RedisDB          int
	RedisPrefix      string
	RabbitURL        string
	RabbitVHost      string
}

func main() {
	cfg := parseFlags()
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	report, err := runLab(ctx, cfg, os.Getenv("BKCRAB_LAB_ADMIN_PASSWORD"), os.Getenv("BKCRAB_LAB_ADMIN_TOKEN"))
	if report != nil {
		if writeErr := writeReport(cfg.Output, report); writeErr != nil {
			log.Printf("write report: %v", writeErr)
		} else {
			log.Printf("report: %s", cfg.Output)
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	if len(report.Errors) > 0 {
		log.Fatalf("fairqueue verification recorded %d error(s); inspect the report", len(report.Errors))
	}
	if report.Verdict != verdictPass {
		log.Fatalf("fairqueue verification verdict: %s", report.Verdict)
	}
	log.Printf("fairqueue verification passed: users=%d documents=%d samples=%d", cfg.Users, cfg.Users*cfg.DocumentsPerUser, len(report.Samples))
}

func parseFlags() labConfig {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	cfg := labConfig{}
	flag.StringVar(&cfg.BaseURL, "base-url", "http://127.0.0.1", "BkCrab base URL")
	flag.StringVar(&cfg.AdminLogin, "admin-login", "admin", "super-admin username or email")
	flag.IntVar(&cfg.Users, "users", 3, "number of temporary users")
	flag.IntVar(&cfg.DocumentsPerUser, "documents", 3, "documents uploaded by each user")
	flag.IntVar(&cfg.DocumentBytes, "document-bytes", 4*1024, "approximate generated Markdown size")
	flag.DurationVar(&cfg.PollInterval, "poll", 500*time.Millisecond, "observation interval")
	flag.DurationVar(&cfg.Timeout, "timeout", 20*time.Minute, "whole-run timeout")
	flag.BoolVar(&cfg.Keep, "keep", false, "keep temporary users and knowledge bases")
	flag.StringVar(&cfg.Output, "output", "fairqueue-lab-report-"+stamp+".json", "JSON report path")
	flag.Int64Var(&cfg.ExpectedGlobal, "expected-global", 4, "expected global concurrency ceiling")
	flag.Int64Var(&cfg.ExpectedPerUser, "expected-per-user", 4, "expected per-user burst ceiling")
	flag.StringVar(&cfg.RedisAddr, "redis-addr", "", "optional Redis address, usually localhost:6379 through a tunnel")
	flag.IntVar(&cfg.RedisDB, "redis-db", 0, "Redis database")
	flag.StringVar(&cfg.RedisPrefix, "redis-prefix", "bkcrab:", "fair queue Redis key prefix")
	flag.StringVar(&cfg.RabbitURL, "rabbit-management-url", "", "optional RabbitMQ management URL, for example http://127.0.0.1:15672")
	flag.StringVar(&cfg.RabbitVHost, "rabbit-vhost", "bkcrab", "RabbitMQ vhost")
	flag.Parse()
	return cfg
}

func validateConfig(cfg labConfig) error {
	if cfg.Users < 2 || cfg.Users > 20 {
		return errors.New("-users must be in 2..20")
	}
	if cfg.DocumentsPerUser < 1 || cfg.DocumentsPerUser > 100 {
		return errors.New("-documents must be in 1..100")
	}
	if cfg.DocumentBytes < 1024 || cfg.DocumentBytes > 8*1024*1024 {
		return errors.New("-document-bytes must be in 1024..8388608")
	}
	if cfg.PollInterval < 100*time.Millisecond || cfg.PollInterval > 30*time.Second {
		return errors.New("-poll must be in 100ms..30s")
	}
	if cfg.Timeout <= 0 || cfg.ExpectedGlobal <= 0 || cfg.ExpectedPerUser <= 0 {
		return errors.New("timeout and expected concurrency values must be positive")
	}
	if strings.TrimSpace(cfg.Output) == "" {
		return errors.New("-output is required")
	}
	return nil
}

func runLab(ctx context.Context, cfg labConfig, adminPassword, adminToken string) (*labReport, error) {
	started := time.Now().UTC()
	report := newReport(cfg, started)
	defer func() {
		if report.FinishedAt.IsZero() {
			report.finish(time.Now().UTC())
		}
	}()
	admin, err := newAPIClient(cfg.BaseURL, adminToken)
	if err != nil {
		return report, err
	}
	if adminToken == "" {
		if adminPassword == "" {
			return report, errors.New("set BKCRAB_LAB_ADMIN_PASSWORD or BKCRAB_LAB_ADMIN_TOKEN")
		}
		if _, err := admin.login(ctx, cfg.AdminLogin, adminPassword); err != nil {
			return report, fmt.Errorf("admin login: %w", err)
		}
	}

	runID, err := randomToken(6)
	if err != nil {
		return report, err
	}
	password, err := randomToken(18)
	if err != nil {
		return report, err
	}
	users := make([]*labUser, 0, cfg.Users)
	var observer *labObserver
	defer func() {
		if observer != nil {
			defer observer.close()
		}
		if cfg.Keep {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cleanupUsers(cleanupCtx, admin, users, report)
		if observer != nil {
			observer.cleanupTenants(cleanupCtx, report)
		}
		report.finish(time.Now().UTC())
	}()

	log.Printf("creating %d isolated users", cfg.Users)
	for index := 0; index < cfg.Users; index++ {
		username := fmt.Sprintf("fqlab-%s-u%02d", runID, index+1)
		account, createErr := admin.createUser(ctx, username, username+"@example.invalid", password)
		if createErr != nil {
			return report, fmt.Errorf("create %s: %w", username, createErr)
		}
		client, clientErr := newAPIClient(cfg.BaseURL, "")
		if clientErr != nil {
			return report, clientErr
		}
		if _, loginErr := client.login(ctx, username, password); loginErr != nil {
			return report, fmt.Errorf("login %s: %w", username, loginErr)
		}
		user := &labUser{Index: index + 1, ID: account.ID, Username: username, TenantHash: tenantHash("rag.index", account.ID), client: client}
		users = append(users, user)
		report.Users = append(report.Users, user.public())
	}

	log.Printf("creating one knowledge base per user")
	var createWG sync.WaitGroup
	createErrors := make(chan error, len(users))
	for _, user := range users {
		createWG.Add(1)
		go func(user *labUser) {
			defer createWG.Done()
			kb, createErr := user.client.createKB(ctx, "Fair queue lab "+runID, "Temporary multi-tenant fairness verification data")
			if createErr != nil {
				createErrors <- fmt.Errorf("create KB for %s: %w", user.Username, createErr)
				return
			}
			user.KBID = kb.ID
		}(user)
	}
	createWG.Wait()
	close(createErrors)
	if err := errors.Join(channelErrors(createErrors)...); err != nil {
		return report, err
	}
	for index := range report.Users {
		report.Users[index].KBID = users[index].KBID
	}

	observer, err = newLabObserver(cfg, admin, users)
	if err != nil {
		return report, err
	}
	observeCtx, stopObserve := context.WithCancel(ctx)
	observeDone := make(chan struct{})
	observerStopped := false
	go func() {
		defer close(observeDone)
		observer.run(observeCtx, cfg.PollInterval, report)
	}()
	defer func() {
		if !observerStopped {
			stopObserve()
			<-observeDone
		}
	}()

	log.Printf("uploading %d documents (%d per user)", cfg.Users*cfg.DocumentsPerUser, cfg.DocumentsPerUser)
	uploadStart := make(chan struct{})
	uploadResults := make(chan uploadResult, cfg.Users*cfg.DocumentsPerUser)
	var uploadWG sync.WaitGroup
	for _, user := range users {
		for docIndex := 1; docIndex <= cfg.DocumentsPerUser; docIndex++ {
			u, n := user, docIndex
			uploadWG.Add(1)
			go func() {
				defer uploadWG.Done()
				<-uploadStart
				name := fmt.Sprintf("fairqueue-u%02d-d%03d.md", u.Index, n)
				body := generatedMarkdown(u.Index, n, cfg.DocumentBytes)
				submittedAt := time.Now().UTC()
				doc, uploadErr := u.client.uploadDocument(ctx, u.KBID, name, []byte(body))
				uploadResults <- uploadResult{User: u, Name: name, SubmittedAt: submittedAt, Document: doc, Err: uploadErr}
			}()
		}
	}
	close(uploadStart)
	uploadWG.Wait()
	close(uploadResults)
	for result := range uploadResults {
		if result.Err != nil {
			report.addError(fmt.Sprintf("upload %s: %v", result.Name, result.Err))
			continue
		}
		report.addDocument(documentObservation{
			ID: result.Document.ID, UserID: result.User.ID, Username: result.User.Username,
			FileName: result.Name, SubmittedAt: result.SubmittedAt,
			LastStatus: result.Document.Status, LastStage: result.Document.Progress.Stage,
		})
	}
	if len(report.Documents) != cfg.Users*cfg.DocumentsPerUser {
		return report, fmt.Errorf("only %d/%d uploads succeeded", len(report.Documents), cfg.Users*cfg.DocumentsPerUser)
	}

	if err := waitForDocuments(ctx, cfg.PollInterval, users, report); err != nil {
		report.addError(err.Error())
	}
	stopObserve()
	<-observeDone
	observerStopped = true
	return report, nil
}

func waitForDocuments(ctx context.Context, interval time.Duration, users []*labUser, report *labReport) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		allTerminal := true
		seenDocuments := 0
		for _, user := range users {
			documents, err := user.client.listDocuments(ctx, user.KBID)
			if err != nil {
				report.addError(fmt.Sprintf("list documents for %s: %v", user.Username, err))
				allTerminal = false
				continue
			}
			for _, document := range documents {
				seenDocuments++
				report.observeDocument(document.ID, document.Status, document.Progress.Stage, time.Now().UTC())
				if !terminalDocumentStatus(document.Status) {
					allTerminal = false
				}
			}
		}
		allTerminal = allTerminal && seenDocuments == len(report.Documents)
		if allTerminal {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for documents: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func cleanupUsers(ctx context.Context, admin *apiClient, users []*labUser, report *labReport) {
	pending := append([]*labUser(nil), users...)
	lastErrors := make(map[string]error, len(pending))
	for len(pending) > 0 && ctx.Err() == nil {
		next := make([]*labUser, 0, len(pending))
		for index := len(pending) - 1; index >= 0; index-- {
			user := pending[index]
			if err := admin.deleteUser(ctx, user.ID); err != nil {
				lastErrors[user.ID] = err
				next = append(next, user)
				continue
			}
			delete(lastErrors, user.ID)
			report.markCleaned(user.ID)
		}
		pending = next
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	for _, user := range pending {
		report.addError(fmt.Sprintf("cleanup user %s: %v", user.Username, lastErrors[user.ID]))
	}
}

func channelErrors(ch <-chan error) []error {
	var result []error
	for err := range ch {
		result = append(result, err)
	}
	return result
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func generatedMarkdown(user, document, targetBytes int) string {
	header := fmt.Sprintf("# Fair queue lab user %d document %d\n\n", user, document)
	paragraph := fmt.Sprintf("Tenant %d document %d contains deterministic indexing workload for observing multi-tenant scheduling. The scheduler should interleave this work with documents owned by other temporary users.\n\n", user, document)
	var builder strings.Builder
	builder.Grow(targetBytes)
	builder.WriteString(header)
	for builder.Len() < targetBytes {
		builder.WriteString(paragraph)
	}
	return builder.String()[:targetBytes]
}

func writeReport(path string, report *labReport) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o755); err != nil && filepath.Dir(filepath.Clean(path)) != "." {
		return err
	}
	data, err := report.marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

type uploadResult struct {
	User        *labUser
	Name        string
	SubmittedAt time.Time
	Document    apiDocument
	Err         error
}
