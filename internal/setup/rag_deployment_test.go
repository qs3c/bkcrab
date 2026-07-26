package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRAGDeploymentComposeConstrainsParser(t *testing.T) {
	root := deploymentRepoRoot(t)
	raw := deploymentRead(t, filepath.Join(root, "deploy", "docker", "docker-compose.rag.yml"))
	envExample := deploymentRead(t, filepath.Join(root, "deploy", "docker", ".env.example"))
	parserRuntime := deploymentRead(t, filepath.Join(root, "services", "rag-parser", "app", "main.py"))
	parserDockerfile := deploymentRead(t, filepath.Join(root, "services", "rag-parser", "Dockerfile"))

	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode compose overlay: %v", err)
	}
	services := deploymentMap(t, document["services"], "services")
	parser := deploymentMap(t, services["rag-parser"], "services.rag-parser")
	gateway := deploymentMap(t, services["bkcrab"], "services.bkcrab")

	image := deploymentString(t, parser["image"], "services.rag-parser.image")
	if image == "" || strings.Contains(strings.ToLower(image), ":latest") || strings.Contains(strings.ToLower(image), ":dev") {
		t.Fatalf("rag-parser image must use a fixed production-safe version, got %q", image)
	}
	if user := deploymentString(t, parser["user"], "services.rag-parser.user"); user == "" || strings.HasPrefix(user, "0") {
		t.Fatalf("rag-parser must run as a non-root uid, got %q", user)
	}
	if readOnly, ok := parser["read_only"].(bool); !ok || !readOnly {
		t.Fatalf("rag-parser must have a read-only root filesystem")
	}
	deploymentRequireContains(t, raw,
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
		"RAG_PARSER_TEMP_ROOT",
		"/tmp:size=",
		"no-new-privileges:true",
		"cap_drop:",
		"cpus:",
		"mem_limit:",
		"healthcheck:",
	)

	parserEnv := deploymentMap(t, parser["environment"], "services.rag-parser.environment")
	for key, want := range map[string]string{
		"RAG_PARSER_TEMP_ROOT": "/tmp",
		"HOME":                 "/tmp",
		"TMPDIR":               "/tmp",
		"XDG_CACHE_HOME":       "/tmp/cache",
	} {
		if got := deploymentString(t, parserEnv[key], "services.rag-parser.environment."+key); got != want {
			t.Fatalf("rag-parser %s = %q, want %q", key, got, want)
		}
	}
	// LibreOffice creates its local IPC pipe directly below /tmp. Mounting only
	// the parser's child directory leaves /tmp on the read-only root filesystem.
	if strings.Contains(string(raw), "/tmp/rag-parser:size=") {
		t.Fatal("rag-parser tmpfs must cover /tmp for LibreOffice IPC")
	}
	for _, forbidden := range []string{
		"API_KEY", "SECRET", "PASSWORD", "OBJECT_STORE", "MINIO", "EMBEDDING", "DOCUMENT_AI", "VISION_MODEL",
	} {
		for key := range parserEnv {
			if strings.Contains(strings.ToUpper(key), forbidden) {
				t.Fatalf("rag-parser must not receive secret/provider/object-store variable %q", key)
			}
		}
	}
	gatewayEnv := deploymentMap(t, gateway["environment"], "services.bkcrab.environment")
	if got := deploymentString(t, gatewayEnv["BKCRAB_RAG_PARSER_ENDPOINT"], "bkcrab parser endpoint"); got != "http://rag-parser:8080" {
		t.Fatalf("bkcrab parser endpoint = %q", got)
	}
	if _, ok := gatewayEnv["BKCRAB_RAG_DOCUMENT_AI_API_KEY"]; !ok {
		t.Fatal("DocumentAI secret must be injected into bkcrab")
	}
	for _, key := range []string{
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
	} {
		if parserEnv[key] != gatewayEnv[key] {
			t.Fatalf("parser and gateway %s must share one Compose value", key)
		}
	}
	if parserEnv["RAG_PARSER_MAX_ENTRY_BYTES"] != gatewayEnv["BKCRAB_RAG_LIMITS_MAX_ASSET_BYTES"] {
		t.Fatalf("parser maxEntryBytes and gateway maxAssetBytes must share one Compose value")
	}
	if gatewayEnv["BKCRAB_RAG_PARSER_TIMEOUT_MS"] != gatewayEnv["BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS"] {
		t.Fatal("parser HTTP timeout and parse limit must share one Compose value")
	}
	for _, deprecated := range []string{"RAG_MAX_INPUT_BYTES", "RAG_PARSE_TIMEOUT_SECONDS"} {
		if strings.Contains(string(envExample), deprecated+"=") {
			t.Fatalf("Compose must not expose independently drifting %s", deprecated)
		}
	}
	deploymentRequireContains(t, parserRuntime,
		`"BKCRAB_RAG_LIMITS_MAX_FILE_MB"`,
		`"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES"`,
		`"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS"`,
		"must match the limit derived from",
	)
	deploymentRequireContains(t, parserDockerfile,
		"RAG_PARSER_TEMP_ROOT=/tmp",
		"HOME=/tmp",
		"TMPDIR=/tmp",
		"XDG_CACHE_HOME=/tmp/cache",
	)
	if strings.Contains(string(parserDockerfile), "RAG_PARSER_TEMP_ROOT=/tmp/rag-parser") {
		t.Fatal("rag-parser image default temp root must keep /tmp writable for LibreOffice IPC")
	}

	dependsOn := deploymentMap(t, gateway["depends_on"], "services.bkcrab.depends_on")
	parserDependency := deploymentMap(t, dependsOn["rag-parser"], "bkcrab depends_on rag-parser")
	if got := deploymentString(t, parserDependency["condition"], "rag-parser dependency condition"); got != "service_healthy" {
		t.Fatalf("bkcrab must wait for a healthy parser, got %q", got)
	}

	networks := deploymentMap(t, document["networks"], "networks")
	parserNetwork := deploymentMap(t, networks["rag-parser-internal"], "networks.rag-parser-internal")
	if internal, ok := parserNetwork["internal"].(bool); !ok || !internal {
		t.Fatal("rag-parser network must be internal")
	}
	deploymentRequireStringListContains(t, parser["networks"], "rag-parser-internal")
	deploymentRequireStringListContains(t, gateway["networks"], "default")
	deploymentRequireStringListContains(t, gateway["networks"], "rag-parser-internal")
}

func TestRAGDeploymentKubernetesConstrainsParser(t *testing.T) {
	root := deploymentRepoRoot(t)
	parserManifest := deploymentRead(t, filepath.Join(root, "deploy", "k8s", "rag-parser.yaml"))
	policyManifest := deploymentRead(t, filepath.Join(root, "deploy", "k8s", "rag-parser-networkpolicy.yaml"))
	gatewayManifest := deploymentRead(t, filepath.Join(root, "deploy", "k8s", "bkcrab.yaml"))
	deploymentRequireValidYAML(t, parserManifest, "deploy/k8s/rag-parser.yaml")
	deploymentRequireValidYAML(t, policyManifest, "deploy/k8s/rag-parser-networkpolicy.yaml")
	deploymentRequireValidYAML(t, gatewayManifest, "deploy/k8s/bkcrab.yaml")

	deploymentRequireContains(t, parserManifest,
		"kind: Deployment",
		"kind: Service",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"seccompProfile:",
		"drop: [\"ALL\"]",
		"readinessProbe:",
		"livenessProbe:",
		"resources:",
		"sizeLimit:",
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
		`{ name: RAG_PARSER_TEMP_ROOT, value: "/tmp" }`,
		"{ name: parser-tmp, mountPath: /tmp }",
	)
	if strings.Contains(string(parserManifest), "/tmp/rag-parser") {
		t.Fatal("Kubernetes parser must mount the whole /tmp directory for LibreOffice IPC")
	}
	for _, deprecated := range []string{
		"name: RAG_PARSER_MAX_INPUT_BYTES",
		"name: RAG_PARSER_MAX_OUTPUT_BYTES",
		"name: RAG_PARSER_PARSE_TIMEOUT_SECONDS",
	} {
		if strings.Contains(string(parserManifest), deprecated) {
			t.Fatalf("Kubernetes parser must consume canonical limits, found %q", deprecated)
		}
	}
	lowerParser := strings.ToLower(string(parserManifest))
	if strings.Contains(lowerParser, ":latest") || strings.Contains(lowerParser, ":dev") {
		t.Fatal("Kubernetes rag-parser image must be fixed and must not use latest/dev")
	}
	for _, forbidden := range []string{"secretKeyRef", "OBJECT_STORE", "DOCUMENT_AI_API_KEY", "EMBEDDING_API_KEY"} {
		if strings.Contains(string(parserManifest), forbidden) {
			t.Fatalf("Kubernetes parser manifest must not contain %q", forbidden)
		}
	}

	deploymentRequireContains(t, policyManifest,
		"policyTypes:",
		"- Ingress",
		"- Egress",
		"ingress: []",
		"egress: []",
		"app: rag-parser",
		"app: bkcrab",
		"port: 8080",
	)
	deploymentRequireContains(t, gatewayManifest,
		"BKCRAB_RAG_PARSER_ENDPOINT",
		"http://rag-parser:8080",
		"BKCRAB_RAG_DOCUMENT_AI_API_KEY",
		"DOCUMENT_AI_API_KEY",
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
	)
	for _, duplicated := range []string{
		"RAG_PARSER_MAX_INPUT_BYTES:",
		"RAG_PARSER_MAX_OUTPUT_BYTES:",
		"RAG_PARSER_PARSE_TIMEOUT_SECONDS:",
	} {
		if strings.Contains(string(gatewayManifest), duplicated) {
			t.Fatalf("Kubernetes ConfigMap must not duplicate canonical limit as %q", duplicated)
		}
	}
}

func TestRAGDeploymentHelmConstrainsParser(t *testing.T) {
	root := deploymentRepoRoot(t)
	values := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "values.yaml"))
	parser := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "templates", "rag-parser.yaml"))
	policy := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "templates", "rag-parser-networkpolicy.yaml"))
	config := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "templates", "configmap.yaml"))
	secrets := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "templates", "secrets.yaml"))
	gateway := deploymentRead(t, filepath.Join(root, "deploy", "helm", "bkcrab", "templates", "gateway.yaml"))

	deploymentRequireContains(t, values,
		"rag:",
		"advancedEnabled: false",
		"officeEnabled: false",
		"enrichmentEnabled: false",
		"parser:",
		"networkPolicy:",
		"maxFileMB: 50",
		"maxExtractedBytes: 209715200",
	)
	var valuesDocument map[string]any
	if err := yaml.Unmarshal(values, &valuesDocument); err != nil {
		t.Fatalf("decode Helm values: %v", err)
	}
	ragValues := deploymentMap(t, valuesDocument["rag"], "values.rag")
	parserValues := deploymentMap(t, ragValues["parser"], "values.rag.parser")
	imageValues := deploymentMap(t, parserValues["image"], "values.rag.parser.image")
	parserTag := strings.ToLower(deploymentString(t, imageValues["tag"], "values.rag.parser.image.tag"))
	if parserTag == "" || parserTag == "latest" || parserTag == "dev" {
		t.Fatalf("Helm rag-parser defaults must pin a production-safe image version, got %q", parserTag)
	}
	deploymentRequireContains(t, parser,
		"kind: Deployment",
		"kind: Service",
		"automountServiceAccountToken: false",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		"readinessProbe:",
		"livenessProbe:",
		"resources:",
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		".Values.rag.limits.maxFileMB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		".Values.rag.limits.maxExtractedBytes",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
		".Values.rag.limits.parseTimeoutMS",
		"mountPath: /tmp",
	)
	if strings.Contains(string(parser), "/tmp/rag-parser") {
		t.Fatal("Helm parser must mount the whole /tmp directory for LibreOffice IPC")
	}
	for _, deprecated := range []string{
		"name: RAG_PARSER_MAX_INPUT_BYTES",
		"name: RAG_PARSER_MAX_OUTPUT_BYTES",
		"name: RAG_PARSER_PARSE_TIMEOUT_SECONDS",
	} {
		if strings.Contains(string(parser), deprecated) {
			t.Fatalf("Helm parser must consume canonical limits, found %q", deprecated)
		}
	}
	for _, forbidden := range []string{"secretKeyRef", "DOCUMENT_AI_API_KEY", "OBJECT_STORE", "EMBEDDING_API_KEY"} {
		if strings.Contains(string(parser), forbidden) {
			t.Fatalf("Helm parser template must not contain %q", forbidden)
		}
	}
	deploymentRequireContains(t, policy,
		"ingress: []",
		"egress: []",
		"- Ingress",
		"- Egress",
		"app: {{ include \"bkcrab.fullname\" . }}-gateway",
		"app: {{ include \"bkcrab.fullname\" . }}-rag-parser",
	)
	deploymentRequireContains(t, config,
		"BKCRAB_RAG_PARSER_ENDPOINT",
		"BKCRAB_RAG_LIMITS_MAX_FILE_MB",
		"BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
		"BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
	)
	deploymentRequireContains(t, secrets, "DOCUMENT_AI_API_KEY:")
	deploymentRequireContains(t, gateway,
		"BKCRAB_RAG_DOCUMENT_AI_API_KEY",
		"key: DOCUMENT_AI_API_KEY",
	)
}

func deploymentRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func deploymentRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func deploymentMap(t *testing.T, value any, where string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map", where, value)
	}
	return result
}

func deploymentString(t *testing.T, value any, where string) string {
	t.Helper()
	result, ok := value.(string)
	if !ok {
		t.Fatalf("%s is %T, want string", where, value)
	}
	return result
}

func deploymentRequireContains(t *testing.T, raw []byte, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(string(raw), value) {
			t.Errorf("deployment file does not contain %q", value)
		}
	}
}

func deploymentRequireValidYAML(t *testing.T, raw []byte, name string) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoded := 0
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode %s document %d: %v", name, decoded+1, err)
		}
		if len(document) > 0 {
			decoded++
		}
	}
	if decoded == 0 {
		t.Fatalf("%s contains no YAML documents", name)
	}
}

func deploymentRequireStringListContains(t *testing.T, value any, expected string) {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("network list is %T, want []any", value)
	}
	for _, item := range items {
		if item == expected {
			return
		}
	}
	t.Fatalf("network list %#v does not contain %q", items, expected)
}
