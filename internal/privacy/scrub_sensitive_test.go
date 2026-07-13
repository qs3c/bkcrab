package privacy

import "testing"

func TestContainsSensitiveInstanceData(t *testing.T) {
	for _, text := range []string{
		"token=abc12345",
		"tenant_id: tenant-42",
		"customer_name: Acme Private Holdings",
		"connect postgres://alice:hunter2@db.example/app",
		"owner alice@example.com",
	} {
		if !ContainsSensitiveInstanceData(text) {
			t.Errorf("sensitive fixture was not detected: %q", text)
		}
	}
}

func TestContainsSensitiveInstanceDataAllowsParameters(t *testing.T) {
	for _, text := range []string{
		"token=${API_TOKEN}",
		"tenant_id: TENANT_ID",
		"customer_name: <customer-name>",
		"Read the token from an environment variable.",
	} {
		if ContainsSensitiveInstanceData(text) {
			t.Errorf("parameterized fixture was rejected: %q", text)
		}
	}
}
