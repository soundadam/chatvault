// Package sqlcipher decrypts SQLCipher 4 databases (the defaults WCDB uses)
// into plain SQLite files, in pure Go: no sqlcipher binary, no cgo.
//
// Each 4096-byte page ends in a reserved area of IV(16) + HMAC-SHA512(64);
// page 1 starts with the 16-byte KDF salt where SQLite's header would be.
// The rest of the page is AES-256-CBC. Decrypting a page keeps the reserved
// area (zeroed), so the plain file has the same page size and layout and
// SQLite reads it as is.
package sqlcipher

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

const (
	PageSize = 4096
	SaltLen  = 16
	ivLen    = 16
	hmacLen  = 64
	reserve  = ivLen + hmacLen
	// KDFIter is SQLCipher 4's PBKDF2 iteration count for the passphrase key.
	KDFIter = 256_000
)

var sqliteHeader = []byte("SQLite format 3\x00")

// ErrMAC means a page failed its HMAC check: wrong key, or a torn copy.
var ErrMAC = errors.New("sqlcipher: page HMAC mismatch")

// Salt returns the KDF salt, the first 16 bytes of page 1.
func Salt(page1 []byte) []byte { return page1[:SaltLen] }

// DeriveKey turns an account (passphrase) key into the page key for one
// database's salt. It is slow on purpose (256k rounds of PBKDF2).
func DeriveKey(passphrase, salt []byte) []byte {
	k, _ := pbkdf2.Key(sha512.New, string(passphrase), salt, KDFIter, 32)
	return k
}

// Cipher decrypts the pages of one database.
type Cipher struct {
	block cipher.Block
	mac   hash.Hash
}

// New returns a Cipher for the page key of a database with the given salt.
func New(key, salt []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	macSalt := make([]byte, len(salt))
	for i, b := range salt {
		macSalt[i] = b ^ 0x3a
	}
	macKey, err := pbkdf2.Key(sha512.New, string(key), macSalt, 2, 32)
	if err != nil {
		return nil, err
	}
	return &Cipher{block: block, mac: hmac.New(sha512.New, macKey)}, nil
}

// Verify reports whether page passes its HMAC check as page number pgno.
func (c *Cipher) Verify(page []byte, pgno uint32) bool {
	if len(page) != PageSize {
		return false
	}
	off := 0
	if pgno == 1 {
		off = SaltLen
	}
	c.mac.Reset()
	c.mac.Write(page[off : PageSize-hmacLen])
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], pgno)
	c.mac.Write(n[:])
	return hmac.Equal(c.mac.Sum(nil), page[PageSize-hmacLen:])
}

// Decrypt writes the plain form of the encrypted page src into dst. Both are
// PageSize long and may not overlap.
func (c *Cipher) Decrypt(dst, src []byte, pgno uint32) error {
	if !c.Verify(src, pgno) {
		return fmt.Errorf("%w (page %d)", ErrMAC, pgno)
	}
	off := 0
	if pgno == 1 {
		off = SaltLen
		copy(dst, sqliteHeader)
	}
	iv := src[PageSize-reserve : PageSize-reserve+ivLen]
	cipher.NewCBCDecrypter(c.block, iv).CryptBlocks(dst[off:PageSize-reserve], src[off:PageSize-reserve])
	clear(dst[PageSize-reserve:])
	if pgno == 1 {
		// File format read/write versions 2 mean WAL mode. The plain copy has
		// no WAL (it is folded in), so mark it rollback-journal: readers then
		// need no -shm and can open it read-only.
		dst[18], dst[19] = 1, 1
	}
	return nil
}

// MatchKey finds the candidate that opens a database whose first page is
// page1. A candidate is either the database's own page key or an account key
// that derives it; the page key is returned. Page keys are tried first, for
// all candidates, because deriving is slow.
func MatchKey(page1 []byte, candidates [][]byte) ([]byte, bool) {
	if len(page1) < PageSize || bytes.Equal(page1[:len(sqliteHeader)], sqliteHeader) {
		return nil, false
	}
	salt := Salt(page1)
	for _, k := range candidates {
		if c, err := New(k, salt); err == nil && c.Verify(page1[:PageSize], 1) {
			return k, true
		}
	}
	for _, k := range candidates {
		d := DeriveKey(k, salt)
		if c, err := New(d, salt); err == nil && c.Verify(page1[:PageSize], 1) {
			return d, true
		}
	}
	return nil, false
}
