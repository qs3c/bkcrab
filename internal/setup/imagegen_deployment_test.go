package setup

import (
	"os"
	"strings"
	"testing"
)

func TestImagegenDeploymentManifestsCarrySafetyContract(t *testing.T) {
	files := []string{
		"../../deploy/docker/docker-compose.yml", "../../deploy/multi-pod/docker-compose.yaml",
		"../../deploy/helm/bkcrab/templates/configmap.yaml", "../../deploy/k8s/bkcrab.yaml",
	}
	required := []string{
		"BKCRAB_IMAGEGEN_BATCH_MODE", "BKCRAB_IMAGEGEN_MAX_IMAGES_PER_BATCH", "BKCRAB_IMAGEGEN_MAX_IMAGES_PER_TASK",
		"BKCRAB_IMAGEGEN_GLOBAL_CONCURRENCY", "BKCRAB_IMAGEGEN_PER_USER_BASE_CONCURRENCY", "BKCRAB_IMAGEGEN_PER_USER_BURST_CONCURRENCY",
		"BKCRAB_IMAGEGEN_TASK_LEASE", "BKCRAB_IMAGEGEN_TASK_HEARTBEAT", "BKCRAB_IMAGEGEN_RESERVATION_TTL",
		"BKCRAB_IMAGEGEN_RESERVATION_HEARTBEAT", "BKCRAB_IMAGEGEN_PROVIDER_CALL_TIMEOUT", "BKCRAB_IMAGEGEN_PROVIDER_CONCURRENCY_DEFAULT",
		"BKCRAB_FAIR_QUEUE_REDIS_MODE", "standalone", "BKCRAB_FAIR_QUEUE_MYSQL_WRITER_TOPOLOGY", "single",
	}
	for _, filename := range files {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, token := range required {
			if !strings.Contains(text, token) {
				t.Errorf("%s missing %s", filename, token)
			}
		}
	}
}

func TestImagegenDeploymentDocumentsTwoRolloutGateAndSharedWorkspace(t *testing.T) {
	for _, filename := range []string{"../../deploy/docker/README.md", "../../deploy/multi-pod/README.md", "../../deploy/k8s/bkcrab.yaml"} {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, token := range []string{"legacy", "drain", "fair", "ReplicaSet", "S3", "LocalFS"} {
			if !strings.Contains(text, token) {
				t.Errorf("%s missing rollout/workspace token %s", filename, token)
			}
		}
	}
}
