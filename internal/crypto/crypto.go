package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"math/rand"
)

const charset = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// pkcs7Pad pads the data to a multiple of blockSize using PKCS#7.
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	pad := make([]byte, padding)
	for i := range pad {
		pad[i] = byte(padding)
	}
	return append(data, pad...)
}

// EncryptPassword encrypts the password using AES-128-CBC, matching the Python implementation.
// key is the salt from the login page. If key is empty, returns the plain password.
func EncryptPassword(password, key string) string {
	if key == "" {
		return password
	}

	iv := []byte(randomString(16))
	padding := randomString(64)
	plaintext := []byte(padding + password)

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return password
	}

	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))

	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, padded)

	return base64.StdEncoding.EncodeToString(ciphertext)
}
