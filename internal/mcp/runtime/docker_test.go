package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordingCommandRunner struct {
	calls   [][]string
	outputs [][]byte
	errs    []error
}

func (r *recordingCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	index := len(r.calls) - 1
	var output []byte
	if index < len(r.outputs) {
		output = r.outputs[index]
	}
	var err error
	if index < len(r.errs) {
		err = r.errs[index]
	}
	return output, err
}

func TestCLIClientEnsureUsesRemoteDockerHostForPublishedGateway(t *testing.T) {
	runner := &recordingCommandRunner{
		outputs: [][]byte{
			[]byte("Error: No such object: gateway"),
			[]byte("container-id\n"),
			[]byte("0.0.0.0:32768\n"),
		},
		errs: []error{errors.New("not found"), nil, nil},
	}
	configDir := t.TempDir()
	client := &CLIClient{
		Runner:     runner,
		DockerHost: "tcp://sandbox-docker:2375",
	}

	ref, err := client.Ensure(context.Background(), ContainerSpec{
		Name:          "gateway",
		Image:         "ghcr.io/lucky-aeon/mcp-gateway:test",
		ConfigDir:     configDir,
		ContainerPort: 8080,
		Protocol:      "all",
	})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if got, want := ref.BaseURL, "http://sandbox-docker:32768"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
	configData, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatalf("read gateway config: %v", err)
	}
	var gatewayConfig struct {
		WorkspacePath   string `json:"WorkspacePath"`
		GatewayProtocol string `json:"GatewayProtocol"`
	}
	if err := json.Unmarshal(configData, &gatewayConfig); err != nil {
		t.Fatalf("decode gateway config: %v", err)
	}
	if got, want := gatewayConfig.WorkspacePath, "/app/vm"; got != want {
		t.Fatalf("WorkspacePath = %q, want %q", got, want)
	}
	if got, want := gatewayConfig.GatewayProtocol, "streamhttp"; got != want {
		t.Fatalf("GatewayProtocol = %q, want %q", got, want)
	}

	wantRun := []string{
		"docker", "run", "-d",
		"--name", "gateway",
		"-p", "0.0.0.0::8080",
		"-v", filepath.Clean(configDir) + ":/app/vm",
		"--restart", "unless-stopped",
		"ghcr.io/lucky-aeon/mcp-gateway:test",
		"-cfg", "/app/vm/config.json",
		"-protocol", "streamhttp",
		"-yes",
	}
	if len(runner.calls) != 3 {
		t.Fatalf("command count = %d, want 3: %#v", len(runner.calls), runner.calls)
	}
	if !reflect.DeepEqual(runner.calls[1], wantRun) {
		t.Fatalf("docker run command = %#v, want %#v", runner.calls[1], wantRun)
	}
}

func TestCLIClientEnsureRecreatesContainerWhenImageChanges(t *testing.T) {
	runner := &recordingCommandRunner{
		outputs: [][]byte{
			[]byte(`[{"Id":"old-id","Name":"/gateway","Config":{"Image":"ghcr.io/lucky-aeon/mcp-gateway:latest"},"State":{"Running":true}}]`),
			[]byte("gateway\n"),
			[]byte("new-id\n"),
			[]byte("0.0.0.0:32770\n"),
		},
	}
	configDir := t.TempDir()
	client := &CLIClient{
		Runner:     runner,
		DockerHost: "tcp://sandbox-docker:2375",
	}

	ref, err := client.Ensure(context.Background(), ContainerSpec{
		Name:          "gateway",
		Image:         "qs3c/mcp-gateway:patched",
		ConfigDir:     configDir,
		ContainerPort: 8080,
		Protocol:      "streamhttp",
	})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if got, want := ref.ID, "new-id"; got != want {
		t.Fatalf("container ID = %q, want %q", got, want)
	}
	if got, want := ref.BaseURL, "http://sandbox-docker:32770"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
	wantRemove := []string{"docker", "rm", "-f", "gateway"}
	if !reflect.DeepEqual(runner.calls[1], wantRemove) {
		t.Fatalf("docker remove command = %#v, want %#v", runner.calls[1], wantRemove)
	}
	wantRun := []string{
		"docker", "run", "-d",
		"--name", "gateway",
		"-p", "0.0.0.0::8080",
		"-v", filepath.Clean(configDir) + ":/app/vm",
		"--restart", "unless-stopped",
		"qs3c/mcp-gateway:patched",
		"-cfg", "/app/vm/config.json",
		"-protocol", "streamhttp",
		"-yes",
	}
	if !reflect.DeepEqual(runner.calls[2], wantRun) {
		t.Fatalf("docker run command = %#v, want %#v", runner.calls[2], wantRun)
	}
}

func TestCLIClientEnsureReportsDockerInspectFailure(t *testing.T) {
	runner := &recordingCommandRunner{
		outputs: [][]byte{[]byte("Cannot connect to the Docker daemon at tcp://sandbox-docker:2375")},
		errs:    []error{errors.New("exit status 1")},
	}
	client := &CLIClient{Runner: runner}

	_, err := client.Ensure(context.Background(), ContainerSpec{
		Name:          "gateway",
		Image:         "qs3c/mcp-gateway:patched",
		ConfigDir:     t.TempDir(),
		ContainerPort: 8080,
	})
	if err == nil {
		t.Fatal("Ensure() error = nil, want Docker inspect failure")
	}
	if got := err.Error(); !strings.Contains(got, "docker inspect gateway") || !strings.Contains(got, "Cannot connect") {
		t.Fatalf("Ensure() error = %q, want inspect/connect detail", got)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("command count = %d, want 1: %#v", len(runner.calls), runner.calls)
	}
}

func TestCLIClientResolvePortKeepsLocalDaemonOnLoopback(t *testing.T) {
	runner := &recordingCommandRunner{
		outputs: [][]byte{[]byte("0.0.0.0:32769\n:::32769\n")},
	}
	client := &CLIClient{Runner: runner}

	ref, err := client.resolvePort(context.Background(), "gateway", 8080, "container-id")
	if err != nil {
		t.Fatalf("resolvePort() error = %v", err)
	}
	if got, want := ref.BaseURL, "http://127.0.0.1:32769"; got != want {
		t.Fatalf("BaseURL = %q, want %q", got, want)
	}
}

func TestNewCLIClientReadsDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://sandbox-docker:2375")
	client := NewCLIClient()
	if got, want := client.DockerHost, "tcp://sandbox-docker:2375"; got != want {
		t.Fatalf("DockerHost = %q, want %q", got, want)
	}
}

func TestGatewayProtocolUsesStreamHTTPForAll(t *testing.T) {
	for _, input := range []string{"", "all", "streamable-http", "streamhttp"} {
		if got, want := gatewayProtocol(input), "streamhttp"; got != want {
			t.Fatalf("gatewayProtocol(%q) = %q, want %q", input, got, want)
		}
	}
}
