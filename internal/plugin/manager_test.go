package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverLoadsBundledMem0Plugin(t *testing.T) {
	pluginsDir := filepath.Clean(filepath.Join("..", "..", "plugins"))

	mgr := NewManager(nil)
	if err := mgr.Discover([]string{pluginsDir}); err != nil {
		t.Fatalf("Discover(%q): %v", pluginsDir, err)
	}

	inst := mgr.Plugin("mem0")
	if inst == nil {
		t.Fatalf("bundled mem0 plugin was not discovered from %s", pluginsDir)
	}
	if inst.Manifest.Type != "hook" {
		t.Fatalf("mem0 type = %q, want hook", inst.Manifest.Type)
	}
	if !hasCapability(inst.Manifest, "hook") {
		t.Fatalf("mem0 capabilities = %#v, want hook capability", inst.Manifest.Capabilities)
	}
	if inst.Manifest.Command == "" {
		t.Fatal("mem0 command is empty")
	}
}

func TestDockerImageSeedsBundledPlugins(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	dockerfilePath := filepath.Join(repoRoot, "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(data)

	for _, want := range []string{
		"python3",
		"COPY plugins/ /usr/local/share/bkcrab/plugins/",
		"COPY scripts/docker-entrypoint.sh /usr/local/bin/bkcrab-entrypoint",
		`ENTRYPOINT ["bkcrab-entrypoint", "bkcrab"]`,
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile does not contain %q", want)
		}
	}

	entrypointPath := filepath.Join(repoRoot, "scripts", "docker-entrypoint.sh")
	entrypoint, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read docker entrypoint: %v", err)
	}
	entrypointText := string(entrypoint)
	for _, want := range []string{
		"/usr/local/share/bkcrab/plugins",
		"$BKCRAB_HOME/plugins",
		"cp -R",
	} {
		if !strings.Contains(entrypointText, want) {
			t.Fatalf("docker entrypoint does not contain %q", want)
		}
	}
}

func TestMem0PluginDoesNotSkipExistingConversationHistory(t *testing.T) {
	pluginPath := filepath.Clean(filepath.Join("..", "..", "plugins", "mem0", "plugin.py"))
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read mem0 plugin: %v", err)
	}
	script := string(data)

	if strings.Contains(script, "has_assistant") {
		t.Fatal("mem0 plugin must not skip memory lookup just because prior conversation history contains assistant messages")
	}
	if !strings.Contains(script, `last_msg.get("role") != "user"`) {
		t.Fatal("mem0 plugin should still skip non-user tail messages during tool iterations")
	}
}

func TestMem0PluginUsesOfficialSearchTopKParameter(t *testing.T) {
	pluginPath := filepath.Clean(filepath.Join("..", "..", "plugins", "mem0", "plugin.py"))
	data, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read mem0 plugin: %v", err)
	}
	script := string(data)

	if !strings.Contains(script, `"top_k": config["topK"]`) {
		t.Fatal("mem0 plugin should send topK as the official /search top_k parameter")
	}
	if strings.Contains(script, `"limit": config["topK"]`) {
		t.Fatal("mem0 plugin should not send topK as legacy limit parameter")
	}
}
