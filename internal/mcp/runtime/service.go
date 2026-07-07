package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/mcp"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusError   = "error"
)

type RuntimeStore interface {
	GetMCPGatewayRuntime(ctx context.Context, userID string) (*store.MCPGatewayRuntimeRecord, error)
	SaveMCPGatewayRuntime(ctx context.Context, rec *store.MCPGatewayRuntimeRecord) error
	ListMCPGatewayRuntimesByStatus(ctx context.Context, statuses ...string) ([]store.MCPGatewayRuntimeRecord, error)
}

type Options struct {
	Store      RuntimeStore
	Docker     DockerClient
	HTTPClient *http.Client
	Config     Config
}

type Service struct {
	store      RuntimeStore
	docker     DockerClient
	httpClient *http.Client
	cfg        Config
	mu         sync.Mutex
	refs       map[string]int
}

func NewService(opts Options) *Service {
	docker := opts.Docker
	if docker == nil {
		docker = NewCLIClient()
	}
	cfg := opts.Config
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	if cfg.ContainerPort == 0 {
		cfg.ContainerPort = 8080
	}
	if cfg.Protocol == "" {
		cfg.Protocol = "all"
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 30 * time.Minute
	}
	if cfg.DeployTimeout <= 0 {
		cfg.DeployTimeout = 5 * time.Minute
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		// 独立的带超时客户端；绝不改动 http.DefaultClient 这个进程级全局单例。
		httpClient = &http.Client{Timeout: cfg.RequestTimeout}
	}
	return &Service{
		store:      opts.Store,
		docker:     docker,
		httpClient: httpClient,
		cfg:        cfg,
		refs:       map[string]int{},
	}
}

func (s *Service) Deploy(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) (*store.MCPGatewayRuntimeRecord, error) {
	if !s.cfg.Enabled {
		return nil, errors.New("mcp gateway runtime is disabled")
	}
	if s.store == nil {
		return nil, errors.New("mcp gateway runtime store is required")
	}
	if userID == "" {
		return nil, errors.New("mcp gateway runtime user_id is required")
	}
	// 给整条部署链路（docker 拉镜像/启动 + /deploy）一个上界。请求上下文可能是
	// context.Background()（agent 构建路径），若不设界，卡死的 docker/网关会永久
	// 占住用户空间加载锁。用 Background 派生而非请求 ctx：部署惠及该用户整个网关，
	// 不应因单个请求取消而中断，但仍需有界。
	if s.cfg.DeployTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.DeployTimeout)
		defer cancel()
	}
	name := containerName(userID)
	ref, err := s.docker.Ensure(ctx, ContainerSpec{
		Name:          name,
		Image:         s.cfg.Image,
		ConfigDir:     filepath.Join(s.cfg.RuntimeDir, userID),
		ContainerPort: s.cfg.ContainerPort,
		Protocol:      s.cfg.Protocol,
	})
	if err != nil {
		return nil, s.saveError(ctx, userID, name, err)
	}
	if err := DeployToGateway(ctx, s.httpClient, ref.BaseURL, servers); err != nil {
		return nil, s.saveError(ctx, userID, name, err)
	}

	now := time.Now().UTC()
	rec, err := s.store.GetMCPGatewayRuntime(ctx, userID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		rec = &store.MCPGatewayRuntimeRecord{UserID: userID}
	}
	deployed, _ := json.Marshal(enabledServers(servers))
	rec.Status = StatusRunning
	rec.DockerContainerID = ref.ID
	rec.ContainerName = name
	rec.Image = s.cfg.Image
	rec.InternalPort = s.cfg.ContainerPort
	rec.ExternalPort = ref.ExternalPort
	rec.BaseURL = ref.BaseURL
	rec.APIKey = "bkcrab-local"
	rec.DeployedServersJSON = string(deployed)
	rec.LastAccessedAt = now
	rec.ErrorMessage = ""
	if err := s.store.SaveMCPGatewayRuntime(ctx, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Service) NewManagerForAgent(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error) {
	return s.NewManagerFromServers(ctx, rc.UserID, rc.MCPServers)
}

func (s *Service) TestServers(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) ([]mcp.ToolDef, error) {
	mgr, err := s.NewManagerFromServers(ctx, userID, servers)
	if err != nil {
		return nil, err
	}
	defer mgr.Close()
	return mgr.ToolDefs(), nil
}

func (s *Service) NewManagerFromServers(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) (*mcp.Manager, error) {
	// 没有任何启用的服务器时不要拉起网关容器：一个全部 enabled=false 的配置
	// 不应触发 docker.Ensure。直接返回空管理器（无工具、无 close hook）。
	if len(enabledServers(servers)) == 0 {
		return mcp.NewManager(nil), nil
	}
	rec, err := s.Deploy(ctx, userID, servers)
	if err != nil {
		return nil, err
	}
	release := s.Acquire(userID)
	client := mcp.NewStreamableHTTPClient(strings.TrimRight(rec.BaseURL, "/")+"/stream", nil)
	mgr := mcp.NewAggregatedManager(client)
	mgr.AddCloseHook(release)
	return mgr, nil
}

func (s *Service) Acquire(userID string) func() {
	s.mu.Lock()
	s.refs[userID]++
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.refs[userID] <= 1 {
				delete(s.refs, userID)
				return
			}
			s.refs[userID]--
		})
	}
}

func (s *Service) StopIdle(ctx context.Context, now time.Time) error {
	if s.store == nil {
		return errors.New("mcp gateway runtime store is required")
	}
	rows, err := s.store.ListMCPGatewayRuntimesByStatus(ctx, StatusRunning)
	if err != nil {
		return err
	}
	for _, rec := range rows {
		if s.refCount(rec.UserID) > 0 {
			continue
		}
		if rec.LastAccessedAt.IsZero() || now.Sub(rec.LastAccessedAt) < s.cfg.IdleTTL {
			continue
		}
		if rec.ContainerName == "" {
			continue
		}
		if err := s.docker.Stop(ctx, rec.ContainerName); err != nil {
			rec.Status = StatusError
			rec.ErrorMessage = err.Error()
		} else {
			rec.Status = StatusStopped
			rec.ErrorMessage = ""
		}
		rec.UpdatedAt = now
		if err := s.store.SaveMCPGatewayRuntime(ctx, &rec); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Start(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = s.StopIdle(ctx, now.UTC())
			}
		}
	}()
}

func (s *Service) StopAll(ctx context.Context) error {
	if s.store == nil {
		return errors.New("mcp gateway runtime store is required")
	}
	rows, err := s.store.ListMCPGatewayRuntimesByStatus(ctx, StatusRunning)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, rec := range rows {
		if rec.ContainerName != "" {
			if err := s.docker.Stop(ctx, rec.ContainerName); err != nil {
				return err
			}
		}
		rec.Status = StatusStopped
		rec.ErrorMessage = ""
		rec.UpdatedAt = now
		if err := s.store.SaveMCPGatewayRuntime(ctx, &rec); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Status(ctx context.Context, userID string) (*store.MCPGatewayRuntimeRecord, error) {
	if s.store == nil {
		return nil, errors.New("mcp gateway runtime store is required")
	}
	return s.store.GetMCPGatewayRuntime(ctx, userID)
}

func (s *Service) refCount(userID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[userID]
}

func (s *Service) saveError(ctx context.Context, userID, name string, cause error) error {
	if s.store == nil {
		return cause
	}
	now := time.Now().UTC()
	rec, err := s.store.GetMCPGatewayRuntime(ctx, userID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		rec = &store.MCPGatewayRuntimeRecord{UserID: userID, ContainerName: name}
	}
	rec.Status = StatusError
	rec.Image = s.cfg.Image
	rec.InternalPort = s.cfg.ContainerPort
	rec.ErrorMessage = cause.Error()
	rec.LastAccessedAt = now
	if err := s.store.SaveMCPGatewayRuntime(ctx, rec); err != nil {
		return err
	}
	return cause
}

func containerName(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return "bkcrab-mcp-gateway-" + hex.EncodeToString(sum[:])[:16]
}

func enabledServers(servers map[string]config.MCPServerConfig) map[string]config.MCPServerConfig {
	out := make(map[string]config.MCPServerConfig, len(servers))
	for name, server := range servers {
		if config.MCPServerEnabled(server) {
			out[name] = server
		}
	}
	return out
}
