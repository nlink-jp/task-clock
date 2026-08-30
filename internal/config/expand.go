package config

import (
	"fmt"
	"strings"
)

// Expand substitutes ${VAR} references in s using lookup (mcp.json style;
// no shell is involved). `$${` escapes a literal `${`. An undefined or
// malformed variable is an error, never a silent empty string (RFP §2).
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			out.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			name := s[i+2 : i+2+end]
			if !validVarName(name) {
				return "", fmt.Errorf("invalid variable name %q in %q", name, s)
			}
			val, ok := lookup(name)
			if !ok {
				return "", fmt.Errorf("undefined variable ${%s} in %q", name, s)
			}
			out.WriteString(val)
			i += 2 + end + 1
		default:
			out.WriteByte('$')
			i++
		}
	}
	return out.String(), nil
}

func validVarName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
