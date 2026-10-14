package util

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Ascending time-sortable id: prefix_<13 digit ms><6 random chars>
func ID(prefix string) string {
	ms := time.Now().UnixMilli()
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
