// zcode 凭证解密: 让无头进程读得到桌面端写下的登录 JWT。
//
// 背景 (移植自上游 ZCode packages/services/src/credential/providers/
// credentialCipherProvider.ts): ~/.zcode/v2/credentials.json 的值以
// "enc:v1:<iv>.<tag>.<ciphertext>" 存储, 算法 AES-256-GCM (IV 12B,
// AuthTag 16B, 三段均 base64url 无填充)。密钥是 sha256(secret), 而
// secret 在未显式配置 ZCODE_CREDENTIAL_SECRET 时由三个环境事实派生:
//
//	zcode-credential-fallback:<os.platform()>:<os.homedir()>:<username>
//
// 这不是加密意义上的秘密 —— 它只绑定「哪台机器上的哪个用户」, 目的
// 是让别的账号读不到本机的凭证文件。因此桥可以在自己进程里复现同一
// 派生, 无需任何额外配置。
//
// 只解 Start Plan 需要的 zcodejwttoken, 不导出其它凭证。
package zcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	encryptedValuePrefix = "enc:v1:"
	credentialIVBytes    = 12
	credentialTagBytes   = 16
	// credentialSecretEnvKey 与上游同名: 显式配置时优先于派生值。
	credentialSecretEnvKey = "ZCODE_CREDENTIAL_SECRET"

	// ZcodeJWTTokenKey 是 Start Plan 授权唯一用到的凭证键
	// (codingPlanProviderAvailability.ts: ZCODE_JWT_TOKEN_KEY)。
	ZcodeJWTTokenKey = "zcodejwttoken"
)

// base64URLToBytes 解 base64url。Node 的 Buffer.from(s,"base64url")
// 容忍无填充, Go 的 base64 需要补齐。
func base64URLToBytes(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}

// credentialSecret 复现上游 defaultCredentialSecret。
func credentialSecret() (string, error) {
	if v := strings.TrimSpace(os.Getenv(credentialSecretEnvKey)); v != "" {
		return v, nil
	}
	username := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("credential secret 派生失败 (homedir): %w", err)
	}
	return fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", runtime.GOOS, home, username), nil
}

// decryptCredentialValue 解开一个 enc:v1: 值; 未加密的值原样返回。
func decryptCredentialValue(value string) (string, error) {
	if !strings.HasPrefix(value, encryptedValuePrefix) {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, encryptedValuePrefix), ".")
	if len(parts) != 3 {
		return "", errors.New("credential 密文格式非法")
	}
	iv, err := base64URLToBytes(parts[0])
	if err != nil {
		return "", fmt.Errorf("credential IV 解码失败: %w", err)
	}
	tag, err := base64URLToBytes(parts[1])
	if err != nil {
		return "", fmt.Errorf("credential AuthTag 解码失败: %w", err)
	}
	ciphertext, err := base64URLToBytes(parts[2])
	if err != nil {
		return "", fmt.Errorf("credential 密文解码失败: %w", err)
	}
	if len(iv) != credentialIVBytes || len(tag) != credentialTagBytes {
		return "", fmt.Errorf("credential 长度非法 (iv=%d tag=%d)", len(iv), len(tag))
	}
	secret, err := credentialSecret()
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", fmt.Errorf("credential cipher 初始化失败: %w", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, credentialIVBytes)
	if err != nil {
		return "", fmt.Errorf("credential GCM 初始化失败: %w", err)
	}
	// Go 的 GCM 把 AuthTag 接在密文**尾部** (Seal/Open 的约定), 而磁盘格式
	// 把 tag 单独存一段。拼接顺序必须是 ciphertext‖tag, 反过来会静默
	// 校验失败 —— 表现为 "密钥不匹配", 与真实原因无关。
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plaintext, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", errors.New("credential 解密失败 (密钥不匹配或密文已损坏)")
	}
	return string(plaintext), nil
}

// LoadCredential 读取 credentials.json 里的一个凭证键并解密。
//
// credsPath 为空时用默认路径 ~/.zcode/v2/credentials.json。文件不存在
// 属正常 (纯环境变量安装), 返回空串而非错误 —— 调用方据此走「无订阅
// 凭证」分支, 而不是让建会话失败。
func LoadCredential(credsPath, key string) (string, error) {
	path := credsPath
	if path == "" {
		dir := zcodeV2Dir("")
		if dir == "" {
			return "", nil
		}
		path = filepath.Join(dir, "credentials.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("凭证文件读取失败 (%s): %w", path, err)
	}
	var store map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		return "", fmt.Errorf("凭证文件结构异常 (%s): %w", path, err)
	}
	raw, ok := store[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return "", nil
	}
	plaintext, err := decryptCredentialValue(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(plaintext), nil
}

// LoadZcodeJWTToken 是 Start Plan 的入口凭证。
func LoadZcodeJWTToken(credsPath string) (string, error) {
	return LoadCredential(credsPath, ZcodeJWTTokenKey)
}
