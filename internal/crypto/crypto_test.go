//go:build fuse3

package crypto

import (
	"bytes"
	"testing"
)

func testEncryptor(t *testing.T) *Encryptor {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	enc, err := NewEncryptor(key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	enc := testEncryptor(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"one_byte", []byte{0x42}},
		{"1KB", bytes.Repeat([]byte("A"), 1024)},
		{"1MB", bytes.Repeat([]byte("B"), 1<<20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := enc.Encrypt(tc.data)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			pt, err := enc.Decrypt(ct)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(pt, tc.data) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(pt), len(tc.data))
			}
		})
	}
}

func TestEncryptProducesDistinctCiphertexts(t *testing.T) {
	enc := testEncryptor(t)
	plain := []byte("same data")
	ct1, err := enc.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	ct2, err := enc.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("expected distinct ciphertexts due to random nonce")
	}
}

func TestDecryptRejectsCorrupted(t *testing.T) {
	enc := testEncryptor(t)
	ct, err := enc.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a byte in the ciphertext portion (after the nonce)
	ct[NonceLen+1] ^= 0xFF
	_, err = enc.Decrypt(ct)
	if err == nil {
		t.Fatal("expected GCM auth failure on corrupted ciphertext")
	}
}

func TestDecryptRejectsTruncated(t *testing.T) {
	enc := testEncryptor(t)
	short := make([]byte, NonceLen-1)
	_, err := enc.Decrypt(short)
	if err == nil {
		t.Fatal("expected error for data shorter than nonce")
	}
}

func TestNewEncryptorRejectsWrongKeyLength(t *testing.T) {
	for _, size := range []int{16, 64} {
		_, err := NewEncryptor(make([]byte, size))
		if err == nil {
			t.Fatalf("expected error for %d-byte key", size)
		}
	}
}

func TestDeriveKeyDeterministic(t *testing.T) {
	salt := []byte("1234567890123456")
	k1 := DeriveKey("passphrase", salt)
	k2 := DeriveKey("passphrase", salt)
	if !bytes.Equal(k1, k2) {
		t.Fatal("same passphrase+salt should produce same key")
	}
	if len(k1) != KeyLen {
		t.Fatalf("key length %d, want %d", len(k1), KeyLen)
	}
}

func TestDeriveKeyDifferentSalts(t *testing.T) {
	k1 := DeriveKey("passphrase", []byte("salt_one________"))
	k2 := DeriveKey("passphrase", []byte("salt_two________"))
	if bytes.Equal(k1, k2) {
		t.Fatal("different salts should produce different keys")
	}
}

func TestGenerateKeyAndSalt(t *testing.T) {
	key1, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key2, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if len(key1) != KeyLen {
		t.Fatalf("key length %d, want %d", len(key1), KeyLen)
	}
	if bytes.Equal(key1, key2) {
		t.Fatal("two generated keys should differ")
	}

	salt1, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	salt2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	if len(salt1) != SaltLen {
		t.Fatalf("salt length %d, want %d", len(salt1), SaltLen)
	}
	if bytes.Equal(salt1, salt2) {
		t.Fatal("two generated salts should differ")
	}
}
