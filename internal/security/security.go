// Package security contains the small cryptographic building blocks used by
// wmux. It deliberately keeps key management, password hashing and session
// tokens independent from HTTP and storage concerns.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/scrypt"
)

const (
	MasterKeySize = 32
	TokenSize     = 32

	scryptLogN    = 15
	scryptR       = 8
	scryptP       = 1
	scryptSaltLen = 16
	scryptKeyLen  = 32
)

var (
	ErrInvalidKey          = errors.New("security: AES-256 key must be exactly 32 bytes")
	ErrInvalidCiphertext   = errors.New("security: invalid ciphertext")
	ErrInvalidPasswordHash = errors.New("security: invalid password hash")
	ErrEmptyPassword       = errors.New("security: password must not be empty")

	keyFileMu sync.Mutex
	sealMagic = []byte{'w', 'm', 'u', 'x', 1}
)

// LoadOrCreateMasterKey loads a 256-bit raw key or atomically creates one. The
// containing directory is mode 0700 and the key is always mode 0600. Symlink
// key paths are rejected.
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("master key path is empty")
	}
	keyFileMu.Lock()
	defer keyFileMu.Unlock()

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create master key directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure master key directory: %w", err)
	}

	if _, err := os.Lstat(path); err == nil {
		return loadMasterKey(path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect master key: %w", err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".master.key-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary master key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("secure temporary master key: %w", err)
	}
	if _, err := tmp.Write(key); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync master key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close master key: %w", err)
	}
	ok = true

	// A hard link publishes the fully written key without replacing a key that
	// another process may have created concurrently.
	if err := os.Link(tmpName, path); err != nil {
		if _, statErr := os.Lstat(path); statErr == nil {
			return loadMasterKey(path)
		}
		return nil, fmt.Errorf("publish master key: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure master key: %w", err)
	}
	return append([]byte(nil), key...), nil
}

func loadMasterKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect master key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("master key must be a regular file and not a symlink")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure master key: %w", err)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("master key has %d bytes, want %d", len(key), MasterKeySize)
	}
	return key, nil
}

// Encrypt seals plaintext with AES-256-GCM. Its output includes a format
// marker and random nonce and is safe to store directly as a BLOB.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	return EncryptWithAAD(key, plaintext, nil)
}

// EncryptWithAAD is Encrypt with authenticated (but unencrypted) context.
func EncryptWithAAD(key, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	result := make([]byte, 0, len(sealMagic)+len(nonce)+len(plaintext)+gcm.Overhead())
	result = append(result, sealMagic...)
	result = append(result, nonce...)
	result = gcm.Seal(result, nonce, plaintext, aad)
	return result, nil
}

// Decrypt opens data created by Encrypt.
func Decrypt(key, ciphertext []byte) ([]byte, error) {
	return DecryptWithAAD(key, ciphertext, nil)
}

// DecryptWithAAD opens data created by EncryptWithAAD.
func DecryptWithAAD(key, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	prefixLen := len(sealMagic) + gcm.NonceSize()
	if len(ciphertext) < prefixLen+gcm.Overhead() || subtle.ConstantTimeCompare(ciphertext[:len(sealMagic)], sealMagic) != 1 {
		return nil, ErrInvalidCiphertext
	}
	nonce := ciphertext[len(sealMagic):prefixLen]
	plaintext, err := gcm.Open(nil, nonce, ciphertext[prefixLen:], aad)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != MasterKeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return gcm, nil
}

// EncryptJSON marshals v and encrypts the resulting JSON.
func EncryptJSON(key []byte, v any) ([]byte, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal encrypted JSON: %w", err)
	}
	return Encrypt(key, plaintext)
}

// DecryptJSON decrypts ciphertext and unmarshals it into v.
func DecryptJSON(key, ciphertext []byte, v any) error {
	plaintext, err := Decrypt(key, ciphertext)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(plaintext, v); err != nil {
		return fmt.Errorf("unmarshal encrypted JSON: %w", err)
	}
	return nil
}

// HashPassword returns a self-describing scrypt password hash.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}
	salt := make([]byte, scryptSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	derived, err := scrypt.Key([]byte(password), salt, 1<<scryptLogN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}
	return fmt.Sprintf("$scrypt$ln=%d,r=%d,p=%d$%s$%s",
		scryptLogN,
		scryptR,
		scryptP,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	), nil
}

// VerifyPassword checks password against an encoded value from HashPassword.
// Malformed or unreasonably expensive hashes are rejected before invoking
// scrypt, preventing a corrupted database from causing excessive work.
func VerifyPassword(password, encoded string) (bool, error) {
	logN, r, p, salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	actual, err := scrypt.Key([]byte(password), salt, 1<<logN, r, p, len(expected))
	if err != nil {
		return false, fmt.Errorf("derive password hash: %w", err)
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func parsePasswordHash(encoded string) (logN, r, p int, salt, derived []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "scrypt" {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	parameters := strings.Split(parts[2], ",")
	if len(parameters) != 3 {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	values := make(map[string]int, 3)
	for _, parameter := range parameters {
		pair := strings.SplitN(parameter, "=", 2)
		if len(pair) != 2 {
			return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
		}
		value, parseErr := strconv.Atoi(pair[1])
		if parseErr != nil || value <= 0 {
			return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
		}
		if _, duplicate := values[pair[0]]; duplicate {
			return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
		}
		values[pair[0]] = value
	}
	logN, okN := values["ln"]
	r, okR := values["r"]
	p, okP := values["p"]
	if !okN || !okR || !okP || logN < 10 || logN > 20 || r > 32 || p > 16 {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	// scrypt's memory/work parameters must also fit in int safely.
	if uint64(r)*uint64(p) >= 1<<30 || logN >= strconv.IntSize-1 || uint64(1<<logN) > uint64(math.MaxInt)/(128*uint64(r)) {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	salt, decodeErr := base64.RawStdEncoding.DecodeString(parts[3])
	if decodeErr != nil || len(salt) < 16 || len(salt) > 64 {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	derived, decodeErr = base64.RawStdEncoding.DecodeString(parts[4])
	if decodeErr != nil || len(derived) < 16 || len(derived) > 64 {
		return 0, 0, 0, nil, nil, ErrInvalidPasswordHash
	}
	return logN, r, p, salt, derived, nil
}

// GenerateToken returns 256 bits of entropy encoded without URL padding.
func GenerateToken() (string, error) {
	raw := make([]byte, TokenSize)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken returns the SHA-256 digest that should be persisted instead of the
// bearer token itself.
func HashToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return append([]byte(nil), digest[:]...)
}
