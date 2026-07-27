package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
			nil,
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
		WorkspacePath string `json:"WorkspacePath"`
	}
	if err := json.Unmarshal(configData, &gatewayConfig); err != nil {
		t.Fatalf("decode gateway config: %v", err)
	}
	if got, want := gatewayConfig.WorkspacePath, "/app/vm"; got != want {
		t.Fatalf("WorkspacePath = %q, want %q", got, want)
	}

	wantRun := []string{
		"docker", "run", "-d",
		"--name", "gateway",
		"-p", "0.0.0.0::8080",
		"-v", filepath.Clean(configDir) + ":/app/vm",
		"--restart", "unless-stopped",
		"ghcr.io/lucky-aeon/mcp-gateway:test",
		"-cfg", "/app/vm/config.json",
		"-yes",
	}
	if len(runner.calls) != 3 {
		t.Fatalf("command count = %d, want 3: %#v", len(runner.calls), runner.calls)
	}
	if !reflect.DeepEqual(runner.calls[1], wantRun) {
		t.Fatalf("docker run command = %#v, want %#v", runner.calls[1], wantRun)
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
