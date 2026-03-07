package workload

import (
	"fmt"
	"strings"
)

func IsNotLeader(errText string) bool {
	return strings.Contains(strings.ToLower(errText), "not leader")
}

func FixedKey(id uint64) string {
	return fmt.Sprintf("%08x", uint32(id))
}

func FixedPayload(payloadBytes int, seed uint64) string {
	if payloadBytes <= 0 {
		return ""
	}
	prefix := fmt.Sprintf("%08x", uint32(seed))
	if payloadBytes <= len(prefix) {
		return prefix[:payloadBytes]
	}
	return prefix + strings.Repeat("v", payloadBytes-len(prefix))
}
