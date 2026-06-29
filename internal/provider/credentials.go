package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qs3c/bkcrab/internal/config"
)

// CredentialEntry 表示一个存储的凭据。
type CredentialEntry struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`   // "api_key"、"oauth"、"token"
	Source string            `json:"source"` // "config"、"env"、"store"
	Keys   map[string]string `json:"keys"`
}

// CredentialManager 处理安全的凭据存储和检索。
type CredentialManager struct {
	masterKey      []byte
	entries        map[string]*CredentialEntry
	storePath      string
	needsReencrypt bool // 在旧密钥回退解密后为 true
	mu             sync.RWMutex
}

// NewCredentialManagerForUser 创建一个限定于特定用户的凭据管理器。
// 凭据存储在 ~/.bkcrab/users/{userID}/credentials.json 中，
// 并使用从用户 ID 派生的密钥加密，因此即使一个用户的文件
// 被移动到磁盘上其他位置，也无法用另一个用户的密钥解密。
func NewCredentialManagerForUser(userID, passphrase string) (*CredentialManager, error) {
	if userID == "" {
		return nil, fmt.Errorf("provider: NewCredentialManagerForUser requires userID")
	}
	home, err := config.HomeDir()
	if err != nil {
		return nil, fmt.Errorf("home dir: %w", err)
	}
	storeDir := filepath.Join(home, "users", userID)
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure user dir: %w", err)
	}

	key := deriveKeyForUser(userID, passphrase)

	cm := &CredentialManager{
		masterKey: key,
		entries:   make(map[string]*CredentialEntry),
		storePath: filepath.Join(storeDir, "credentials.json"),
	}

	// 加载现有凭据
	if err := cm.load(); err != nil && !os.IsNotExist(err) {
		// 如果解密失败，重新开始
		cm.entries = make(map[string]*CredentialEntry)
	}

	// 如果我们使用旧密钥解密，立即使用新的按用户密钥重新保存，
	// 以便文件在磁盘上被迁移。
	if cm.needsReencrypt {
		if err := cm.save(); err == nil {
			cm.needsReencrypt = false
		}
	}

	return cm, nil
}

// Set 存储一个凭据键值对。
func (cm *CredentialManager) Set(name, key, value string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	entry, ok := cm.entries[name]
	if !ok {
		entry = &CredentialEntry{
			Name:   name,
			Type:   "api_key",
			Source: "store",
			Keys:   make(map[string]string),
		}
		cm.entries[name] = entry
	}

	entry.Keys[key] = value
	return cm.save()
}

// Get 检索一个凭据值。
func (cm *CredentialManager) Get(name, key string) (string, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	entry, ok := cm.entries[name]
	if !ok {
		return "", fmt.Errorf("credential %q not found", name)
	}

	val, ok := entry.Keys[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in credential %q", key, name)
	}

	return val, nil
}

// List 返回所有凭据条目。
func (cm *CredentialManager) List() []CredentialEntry {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make([]CredentialEntry, 0, len(cm.entries))
	for _, e := range cm.entries {
		// 复制但不暴露密钥值
		masked := CredentialEntry{
			Name:   e.Name,
			Type:   e.Type,
			Source: e.Source,
			Keys:   make(map[string]string),
		}
		for k, v := range e.Keys {
			if len(v) > 8 {
				masked.Keys[k] = v[:4] + "..." + v[len(v)-4:]
			} else {
				masked.Keys[k] = "****"
			}
		}
		result = append(result, masked)
	}
	return result
}

// Delete 移除一个凭据条目。
func (cm *CredentialManager) Delete(name string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, ok := cm.entries[name]; !ok {
		return fmt.Errorf("credential %q not found", name)
	}

	delete(cm.entries, name)
	return cm.save()
}

// knownEnvVars 将提供者名称映射到其环境变量模式。
var knownEnvVars = map[string][]string{
	"openai":     {"OPENAI_API_KEY"},
	"anthropic":  {"ANTHROPIC_API_KEY"},
	"openrouter": {"OPENROUTER_API_KEY"},
	"google":     {"GOOGLE_API_KEY", "GEMINI_API_KEY"},
	"mistral":    {"MISTRAL_API_KEY"},
	"cohere":     {"COHERE_API_KEY"},
	"groq":       {"GROQ_API_KEY"},
	"together":   {"TOGETHER_API_KEY"},
	"deepseek":   {"DEEPSEEK_API_KEY"},
}

// Discover 扫描环境变量以查找已知的 API 密钥模式。
func (cm *CredentialManager) Discover() []CredentialEntry {
	var discovered []CredentialEntry

	for providerName, envVars := range knownEnvVars {
		for _, envVar := range envVars {
			val := os.Getenv(envVar)
			if val == "" {
				continue
			}
			entry := CredentialEntry{
				Name:   providerName,
				Type:   "api_key",
				Source: "env",
				Keys:   map[string]string{"apiKey": val},
			}
			discovered = append(discovered, entry)
		}
	}

	return discovered
}

// InjectEnv 返回适合注入到沙箱中的环境变量。
func (cm *CredentialManager) InjectEnv() map[string]string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	env := make(map[string]string)

	for name, entry := range cm.entries {
		if apiKey, ok := entry.Keys["apiKey"]; ok {
			// 映射回环境变量名
			envVars, known := knownEnvVars[name]
			if known && len(envVars) > 0 {
				env[envVars[0]] = apiKey
			} else {
				env[strings.ToUpper(name)+"_API_KEY"] = apiKey
			}
		}
	}

	// 同时包含任何从环境变量发现的凭据
	for _, envVars := range knownEnvVars {
		for _, envVar := range envVars {
			if val := os.Getenv(envVar); val != "" {
				env[envVar] = val
			}
		}
	}

	return env
}

func (cm *CredentialManager) save() error {
	data, err := json.Marshal(cm.entries)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	encrypted, err := encrypt(data, cm.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt credentials: %w", err)
	}

	if err := os.WriteFile(cm.storePath, encrypted, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}

	return nil
}

func (cm *CredentialManager) load() error {
	data, err := os.ReadFile(cm.storePath)
	if err != nil {
		return err
	}

	decrypted, err := decrypt(data, cm.masterKey)
	if err != nil {
		// 回退：尝试旧密钥格式（多用户之前），
		// 以便现有安装升级后不会丢失其存储的凭据。
		legacyKey := legacyDeriveKey()
		decrypted, err = decrypt(data, legacyKey)
		if err != nil {
			return fmt.Errorf("decrypt credentials: %w", err)
		}
		cm.needsReencrypt = true
	}

	return json.Unmarshal(decrypted, &cm.entries)
}

// legacyDeriveKey 返回旧的（多用户之前）机器派生密钥，
// 以便我们可以解密在添加按用户 KEK 之前创建的凭据文件。
func legacyDeriveKey() []byte {
	hostname, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	hash := sha256.Sum256([]byte("bkcrab:" + hostname + ":" + home))
	return hash[:]
}

// deriveKeyForUser 将用户 ID 混入加密密钥中，以便每个用户的凭据文件
// 使用不同的 KEK 加密。在没有明确密码短语的情况下，
// 仍然包含机器派生的种子（主机名 + 家目录），
// 以便相同的用户 ID 在不同主机上产生不同的密钥——
// 防止凭据文件跨主机批量复制后被解密。
func deriveKeyForUser(userID, passphrase string) []byte {
	if userID == "" {
		userID = "_anonymous"
	}
	var seed string
	if passphrase != "" {
		seed = "bkcrab:user:" + userID + ":pp:" + passphrase
	} else {
		hostname, _ := os.Hostname()
		home, _ := os.UserHomeDir()
		seed = "bkcrab:user:" + userID + ":host:" + hostname + ":" + home
	}
	hash := sha256.Sum256([]byte(seed))
	return hash[:]
}

func encrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
