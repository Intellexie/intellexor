package diff

import (
	"strings"
)

// Line diff with 3 lines of context, jsdiff createTwoFilesPatch compatible.
func Patch(filename, old, new string) (string, int, int) {
	a := splitLines(old)
	b := splitLines(new)
	ops := diffLines(a, b)
	additions, deletions := 0, 0
	for _, o := range ops {
		if o.kind == '+' {
			additions++
		}
		if o.kind == '-' {
			deletions++
		}
	}
	if additions == 0 && deletions == 0 {
		return "", 0, 0
	}
	var sb strings.Builder
	sb.WriteString("Index: " + filename + "\n")
	sb.WriteString(strings.Repeat("=", 67) + "\n")
	sb.WriteString("--- " + filename + "\n")
	sb.WriteString("+++ " + filename + "\n")
	// hunks
	const ctx = 3
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		end := i + 1
		run := 0
		for j := i; j < len(ops); j++ {
			if ops[j].kind == ' ' {
				run++
				if run > 2*ctx {
					break
				}
			} else {
				run = 0
			}
			if ops[j].kind != ' ' || run <= ctx {
				end = j + 1 + ctx
			}
			if end > len(ops) {
				end = len(ops)
			}
		}
		if end > len(ops) {
			end = len(ops)
		}
		var oldStart, oldLines, newStart, newLines int
		// count context before first change
		k := start
		for k < i && ops[k].kind == ' ' {
			k++
		}
		// oldStart/newStart based on counts up to `start`
		var o, n int
		for _, op := range ops[:start] {
			if op.kind == ' ' || op.kind == '-' {
				o++
			}
			if op.kind == ' ' || op.kind == '+' {
				n++
			}
		}
		oldStart = o + 1
		newStart = n + 1
		for _, op := range ops[start:end] {
			if op.kind == ' ' || op.kind == '-' {
				oldLines++
			}
			if op.kind == ' ' || op.kind == '+' {
				newLines++
			}
		}
		sb.WriteString("@@ -" + itoa(oldStart) + "," + itoa(oldLines) + " +" + itoa(newStart) + "," + itoa(newLines) + " @@\n")
		for _, op := range ops[start:end] {
			switch op.kind {
			case ' ':
				sb.WriteString(" " + op.line + "\n")
			case '-':
				sb.WriteString("-" + op.line + "\n")
			case '+':
				sb.WriteString("+" + op.line + "\n")
			}
		}
		i = end
	}
	return TrimPatch(sb.String()), additions, deletions
}

type op struct {
	kind byte
	line string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func diffLines(a, b []string) []op {
	// common prefix/suffix
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var ops []op
	for _, l := range a[:p] {
		ops = append(ops, op{' ', l})
	}
	ma := a[p : len(a)-s]
	mb := b[p : len(b)-s]
	if len(ma)*len(mb) <= 1_500_000 {
		lcs := lcsDiff(ma, mb)
		ops = append(ops, lcs...)
	} else {
		for _, l := range ma {
			ops = append(ops, op{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, op{'+', l})
		}
	}
	for _, l := range a[len(a)-s:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

func lcsDiff(a, b []string) []op {
	n, m := len(a), len(b)
	// dp table as 1D rolling for memory, then backtrack needs full table — cap size handled by caller
	f := make([][]int32, n+1)
	for i := range f {
		f[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				f[i][j] = f[i+1][j+1] + 1
			} else if f[i+1][j] >= f[i][j+1] {
				f[i][j] = f[i+1][j]
			} else {
				f[i][j] = f[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		} else if f[i+1][j] >= f[i][j+1] {
			ops = append(ops, op{'-', a[i]})
			i++
		} else {
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// TrimPatch removes a common leading indent from +/-/space content lines
func TrimPatch(patch string) string {
	lines := strings.Split(patch, "\n")
	common := -1
	for _, l := range lines {
		if len(l) == 0 || (l[0] != '+' && l[0] != '-' && l[0] != ' ') {
			continue
		}
		if strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "@@") {
			continue
		}
		body := l[1:]
		n := 0
		for n < len(body) && (body[n] == ' ' || body[n] == '\t') {
			n++
		}
		if n == len(body) && len(body) > 0 {
			continue
		}
		if common < 0 || n < common {
			common = n
		}
	}
	if common <= 0 {
		return patch
	}
	for idx, l := range lines {
		if len(l) > 0 && (l[0] == '+' || l[0] == '-' || l[0] == ' ') && !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") {
			body := l[1:]
			if len(body) >= common {
				lines[idx] = string(l[0]) + body[common:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
