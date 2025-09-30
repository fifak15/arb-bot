package screener

import (
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/sha3"
)

func toChecksumAddress(addr string) (string, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		return "", fmt.Errorf("empty address")
	}
	if strings.HasPrefix(a, "0x") || strings.HasPrefix(a, "0X") {
		a = a[2:]
	}
	if len(a) != 40 {
		return "", fmt.Errorf("bad hex length: %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		return "", fmt.Errorf("not hex: %w", err)
	}

	lower := strings.ToLower(a)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lower))
	hash := h.Sum(nil)
	hexhash := make([]byte, 64)
	hex.Encode(hexhash, hash)

	out := make([]byte, 40)
	for i := 0; i < 40; i++ {
		ch := lower[i]
		if ch >= 'a' && ch <= 'f' {
			var nibble byte
			if hexhash[i] >= '0' && hexhash[i] <= '9' {
				nibble = hexhash[i] - '0'
			} else {
				nibble = 10 + (hexhash[i] - 'a')
			}
			if nibble >= 8 {
				ch = byte(strings.ToUpper(string(ch))[0])
			}
		}
		out[i] = ch
	}
	return "0x" + string(out), nil
}
