// Package extract implements wxapkg decryption/unpacking and a
// sensitive-information analyzer.
//
// Encrypted wxapkg format (WeChat 4.0+):
//   - magic "V1MMWX" (6 bytes)
//   - AES-256-CBC region file[6:1030] → first 1023 bytes of plaintext
//   - XOR region file[1030:] → each byte XOR appID[len-2]
//   - key = PBKDF2-SHA1(appID, "saltiest", 1000, 32 bytes), IV = "the iv: 16 bytes"
//
// Package format: 0xBE marker, info1(4), indexInfoLength(4), bodyInfoLength(4),
// 0xED marker, fileCount(4), then per entry nameLen(4)+name+offset(4)+size(4).
package extract

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec // G505: the wxapkg format pins PBKDF2-SHA1; changing it breaks decryption.
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	wxapkgMagic = "V1MMWX"
	wxapkgSalt  = "saltiest"
	wxapkgIV    = "the iv: 16 bytes"
	pbkdf2Iter  = 1000
	keyLength   = 32
)

// deriveKey derives the AES-256 key from the appID (PBKDF2-SHA1).
func deriveKey(appID string) ([]byte, error) {
	return pbkdf2.Key(sha1.New, appID, []byte(wxapkgSalt), pbkdf2Iter, keyLength)
}

// Decrypt decrypts one encrypted wxapkg payload; unencrypted packages
// (starting with the 0xBE marker) pass through unchanged.
func Decrypt(data []byte, appID string) ([]byte, error) {
	// The 0xBE marker identifies an unencrypted package before any length
	// rule: a small plain package has no 1030-byte encrypted header.
	if len(data) >= 1 && data[0] == 0xBE {
		return data, nil
	}
	if len(data) < 1030 {
		return nil, fmt.Errorf("file too small (%d bytes) for an encrypted wxapkg", len(data))
	}
	if string(data[:6]) != wxapkgMagic {
		return nil, fmt.Errorf("unknown file format (magic: %x)", data[:6])
	}

	key, err := deriveKey(appID)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data[6:1030])%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted header is not block-aligned")
	}

	decrypted := make([]byte, len(data[6:1030]))
	cipher.NewCBCDecrypter(block, []byte(wxapkgIV)).CryptBlocks(decrypted, data[6:1030])
	header := decrypted[:1023]
	// The decrypted payload is a wxapkg: a wrong appID yields garbage that
	// fails the 0xBE marker, so this doubles as an integrity check.
	if header[0] != 0xBE {
		return nil, fmt.Errorf("decryption failed (wrong appID?): marker %#x, want 0xBE", header[0])
	}

	// XOR the tail with the second-to-last appID character.
	xorKey := byte(0)
	if len(appID) >= 2 {
		xorKey = appID[len(appID)-2]
	}
	tail := make([]byte, len(data[1030:]))
	for i, b := range data[1030:] {
		tail[i] = b ^ xorKey
	}

	return append(header, tail...), nil
}

// Unpack parses a (decrypted) wxapkg and returns its virtual files keyed by
// their internal path.
func Unpack(data []byte) (map[string][]byte, error) {
	// The fixed header is marker1(1) + info1(4) + indexInfoLength(4) +
	// bodyInfoLength(4) + marker2(1) + fileCount(4); anything shorter has no
	// file count to read and must fail instead of running past the slice.
	const headerLength = 18
	if len(data) < headerLength {
		return nil, fmt.Errorf("data too short (%d bytes) for a wxapkg", len(data))
	}
	pos := 0
	if data[pos] != 0xBE {
		return nil, fmt.Errorf("invalid wxapkg (marker1: %#x, want 0xBE)", data[pos])
	}
	pos += 1 + 4 + 4 + 4 // marker + info1 + indexInfoLength + bodyInfoLength
	if data[pos] != 0xED {
		return nil, fmt.Errorf("invalid wxapkg (marker2: %#x, want 0xED)", data[pos])
	}
	pos++
	fileCount := binary.BigEndian.Uint32(data[pos:])
	pos += 4

	// Index entries.
	type entry struct {
		name   string
		offset uint32
		size   uint32
	}
	// fileCount is attacker-controlled, so it must not size the slice: each
	// index entry occupies at least 12 bytes, and the loop below stops as soon
	// as the index runs past the data. Bound the preallocation by the bytes
	// that are actually left instead.
	maxEntries := (len(data) - pos) / 12
	entries := make([]entry, 0, maxEntries)
	for i := uint32(0); i < fileCount && len(entries) < maxEntries; i++ {
		if pos+4 > len(data) {
			break
		}
		nameLen := binary.BigEndian.Uint32(data[pos:])
		pos += 4
		if pos+int(nameLen) > len(data) {
			break
		}
		name := string(data[pos : pos+int(nameLen)])
		pos += int(nameLen)
		if pos+8 > len(data) {
			break
		}
		offset := binary.BigEndian.Uint32(data[pos:])
		size := binary.BigEndian.Uint32(data[pos+4:])
		pos += 8
		entries = append(entries, entry{name: name, offset: offset, size: size})
	}

	files := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if int64(e.offset)+int64(e.size) > int64(len(data)) {
			continue
		}
		files[e.name] = data[e.offset : e.offset+e.size]
	}
	return files, nil
}

// ExtractTo writes unpacked files into outDir, refusing path traversal.
// Returns the written paths.
func ExtractTo(outDir string, files map[string][]byte) ([]string, error) {
	var written []string
	for name, content := range files {
		clean := strings.TrimLeft(strings.ReplaceAll(name, "\\", "/"), "/")
		if clean == "" || strings.Contains(clean, "..") {
			continue
		}
		outPath := filepath.Join(outDir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(outPath, content, 0o600); err != nil {
			return written, err
		}
		written = append(written, outPath)
	}
	return written, nil
}

// ExtractWxapkg decrypts and unpacks one .wxapkg file into outDir, then
// restores the developer-facing source tree.
func ExtractWxapkg(wxapkgPath, outDir, appID string) ([]string, error) {
	written, err := extractWxapkgRaw(wxapkgPath, outDir, appID)
	if err != nil {
		return written, err
	}
	restored, err := RestoreSourceTree(outDir)
	if err != nil {
		return written, fmt.Errorf("source restoration failed after unpack: %w", err)
	}
	return append(written, restored...), nil
}

func extractWxapkgRaw(wxapkgPath, outDir, appID string) ([]string, error) {
	raw, err := os.ReadFile(wxapkgPath)
	if err != nil {
		return nil, err
	}
	decrypted, err := Decrypt(raw, appID)
	if err != nil {
		return nil, err
	}
	files, err := Unpack(decrypted)
	if err != nil {
		return nil, err
	}
	return ExtractTo(outDir, files)
}
