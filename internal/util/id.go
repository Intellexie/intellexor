package util

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// idMu guards lastIDMs so IDs generated concurrently stay strictly ordered.
var (
	idMu     sync.Mutex
	lastIDMs int64
)

// Ascending time-sortable id: prefix_<13 digit ms><6 random chars>.
//
// The millisecond component is guaranteed strictly increasing per process,
// so ids created back-to-back (e.g. a user message and its assistant reply)
// never tie on the timestamp — the store sorts ids lexically, and a tie
// would make load order random. The random suffix keeps ids unique across
// processes.
func ID(prefix string) string {
	idMu.Lock()
	defer idMu.Unlock()
	ms := time.Now().UnixMilli()
	if ms <= lastIDMs {
		// Same millisecond as the previous id (or the clock stepped back):
		// step past it so ordering stays unambiguous. A burst of n ids
		// drifts at most n ms into the future, which is fine — ids are
		// opaque and nothing parses the timestamp back out.
		ms = lastIDMs + 1
	}
	lastIDMs = ms
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteString("_")
	sb.WriteString(fmt.Sprintf("%013d", ms))
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

func Slug(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return strings.ToLower(sb.String())
}

func Now() int64 { return time.Now().UnixMilli() }

func Bool(v bool) *bool { return &v }
