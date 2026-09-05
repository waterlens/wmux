package security

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreateMasterKeyConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "master.key")
	const workers = 12
	keys := make(chan []byte, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := LoadOrCreateMasterKey(path)
			keys <- key
			errs <- err
		}()
	}
	wg.Wait()
	close(keys)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("LoadOrCreateMasterKey: %v", err)
		}
	}
	var first []byte
	for key := range keys {
		if len(key) != MasterKeySize {
			t.Fatalf("key length = %d", len(key))
		}
		if first == nil {
			first = key
		} else if !bytes.Equal(first, key) {
			t.Fatal("concurrent callers got different keys")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("master key mode = %o", info.Mode().Perm())
	}
}

func TestMasterKeyRejectsInvalidFileAndSymlink(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "bad.key")
	if err := os.WriteFile(bad, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMasterKey(bad); err == nil {
		t.Fatal("expected short key error")
	}
	good := filepath.Join(root, "good.key")
	if err := os.WriteFile(good, make([]byte, MasterKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.key")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateMasterKey(link); err == nil {
		t.Fatal("expected symlink key error")
	}
}

type sealedValue struct {
	Password string `json:"password"`
}

func TestEncryptJSONRoundTripAndAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{9}, MasterKeySize)
	sealed, err := EncryptJSON(key, sealedValue{Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("ciphertext exposes plaintext")
	}
	var got sealedValue
	if err := DecryptJSON(key, sealed, &got); err != nil {
		t.Fatal(err)
	}
	if got.Password != "hunter2" {
		t.Fatalf("round trip = %#v", got)
	}
	if err := DecryptJSON(bytes.Repeat([]byte{8}, MasterKeySize), sealed, &got); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong key error = %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if err := DecryptJSON(key, sealed, &got); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tamper error = %v", err)
	}
	sealed[0] ^= 1
	if err := DecryptJSON(key, sealed, &got); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("format marker error = %v", err)
	}
	if _, err := EncryptJSON([]byte("short"), sealedValue{}); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("short key error = %v", err)
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("verify correct password = %v, %v", ok, err)
	}
	ok, err = VerifyPassword("wrong", hash)
	if err != nil || ok {
		t.Fatalf("verify wrong password = %v, %v", ok, err)
	}
	if _, err := VerifyPassword("x", "$scrypt$ln=30,r=8,p=1$bad$bad"); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("malformed hash error = %v", err)
	}
	if _, err := HashPassword(""); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("empty password error = %v", err)
	}
}

func TestGenerateAndHashToken(t *testing.T) {
	token1, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash1 := HashToken(token1)
	token2, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	hash2 := HashToken(token2)
	if token1 == token2 || bytes.Equal(hash1, hash2) {
		t.Fatal("tokens were not unique")
	}
	if len(hash1) != 32 || !bytes.Equal(hash1, HashToken(token1)) {
		t.Fatal("unexpected token hash")
	}
}
