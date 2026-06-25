package instructions

// dotenv parsing for VCS deploys.
//
// On a VCS deploy the cluster agent is the only party that holds the git
// checkout, so it is the only place the committed plain-env file
// (referenced by `env:` in runos.yaml) can be read and resolved into the
// key/value map the conductor reconciles into the app's ConfigMap. This is
// the VCS-side analogue of what the CLI does locally before a CLI deploy.
//
// The parser below is ported from the CLI's internal/envfile.Parse so the
// two deploy paths interpret the same committed .config.env byte-for-byte
// identically (CLI issue 73): a value that parses to "x" on a CLI deploy
// must parse to "x" on a VCS deploy. Keep this in lockstep with the CLI's
// envfile package; a divergence is a silent per-path env drift.
//
// Wire format accepted (permissive, matches the CLI reader):
//   - Blank lines and `# comment` lines (after optional leading whitespace).
//   - Unquoted:      KEY=value           (trailing whitespace stripped; no escapes)
//   - Double-quoted: KEY="value"         (escapes \\ \" \n \r \t; spans lines)
//   - Single-quoted: KEY='value'         (no escapes; spans lines)

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// parseDotenv reads a dotenv-style byte payload and returns the key-value
// map. The parser is permissive: it returns the keys it understands and
// silently skips malformed lines (a stray non-`KEY=value` line doesn't take
// down the whole deploy). Ported from CLI internal/envfile.Parse.
func parseDotenv(data []byte) map[string]string {
	out := map[string]string{}
	s := string(data)
	i := 0
	n := len(s)
	for i < n {
		// Skip leading whitespace within a line, but stop at newline so
		// blank lines remain blank.
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		// Blank line.
		if s[i] == '\n' {
			i++
			continue
		}
		// Comment line.
		if s[i] == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			if i < n {
				i++
			}
			continue
		}
		// Read key up to `=` or end-of-line.
		keyStart := i
		for i < n && s[i] != '=' && s[i] != '\n' {
			i++
		}
		key := strings.TrimSpace(s[keyStart:i])
		if i >= n || s[i] == '\n' {
			// Malformed line (no `=`); skip it.
			if i < n {
				i++
			}
			continue
		}
		// Consume `=` and any whitespace between `=` and the value.
		i++
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		// Decode the value.
		value, advanced := decodeDotenvValue(s, i)
		i = advanced
		// Consume rest of line.
		for i < n && s[i] != '\n' {
			i++
		}
		if i < n {
			i++
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

// decodeDotenvValue reads a value starting at s[i] and returns (value, new
// index after the value). Handles double-quoted, single-quoted, and
// unquoted shapes. Ported from CLI internal/envfile.decodeValue.
func decodeDotenvValue(s string, i int) (string, int) {
	n := len(s)
	if i >= n {
		return "", i
	}
	switch s[i] {
	case '"':
		i++
		var b strings.Builder
		for i < n {
			c := s[i]
			if c == '"' {
				return b.String(), i + 1
			}
			if c == '\\' && i+1 < n {
				switch s[i+1] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"':
					b.WriteByte('"')
				case '\\':
					b.WriteByte('\\')
				default:
					b.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			b.WriteByte(c)
			i++
		}
		// Unterminated double-quote: take everything we got.
		return b.String(), i
	case '\'':
		i++
		start := i
		for i < n && s[i] != '\'' {
			i++
		}
		val := s[start:i]
		if i < n {
			i++
		}
		return val, i
	default:
		start := i
		for i < n && s[i] != '\n' {
			i++
		}
		return strings.TrimRight(s[start:i], " \t\r"), i
	}
}

// validateDotenvValues refuses env-var values that would break the
// downstream kubectl apply when the conductor reconciles them into the
// app's ConfigMap. Bytes kubectl flags as `yaml: control character` (any C0
// control char outside \n \r \t, plus 0x7f DEL) and bytes that don't form
// valid UTF-8 are rejected with a per-key error naming the offending
// variable, so a bad committed .config.env fails loud at fetch time rather
// than mid-orchestration. Ported from CLI internal/envfile.Validate
// (CLI issue 87). Keys are checked in sorted order for deterministic errors.
func validateDotenvValues(envVars map[string]string) error {
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := envVars[k]
		if !utf8.ValidString(v) {
			return fmt.Errorf("env var %q value is not valid UTF-8; check the source file for binary or partial multi-byte content", k)
		}
		for i := 0; i < len(v); i++ {
			c := v[i]
			// Allow newline, carriage return, tab. Everything else below
			// 0x20 is a control char kubectl YAML-marshal rejects; 0x7f
			// (DEL) is in the same camp.
			if c == '\n' || c == '\r' || c == '\t' {
				continue
			}
			if c < 0x20 || c == 0x7f {
				return fmt.Errorf("env var %q value contains control byte 0x%02x at position %d; allowed control chars are \\n, \\r, \\t only", k, c, i)
			}
		}
	}
	return nil
}
