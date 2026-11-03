package interaction

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type Parsed struct {
	Worker, Verb string
	Arguments    map[string]string
}

// Parse accepts one explicit command. Quoted text is data, never shell syntax.
func Parse(input string) (p Parsed, err error) {
	if len(input) == 0 || len(input) > 4096 || !utf8.ValidString(input) || strings.ContainsAny(input, "\x00\r\n") {
		return p, fmt.Errorf("invalid command text")
	}
	var tokens []string
	var b strings.Builder
	quoted, escape, started := false, false, false
	for _, r := range input {
		if escape {
			if r != '"' && r != '\\' {
				return p, fmt.Errorf("unsupported escape")
			}
			b.WriteRune(r)
			escape = false
			continue
		}
		if r == '\\' && quoted {
			escape = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			started = true
			continue
		}
		if unicode.IsControl(r) && r != '\t' {
			return p, fmt.Errorf("control character")
		}
		if !quoted && unicode.IsSpace(r) {
			if started {
				tokens = append(tokens, b.String())
				b.Reset()
				started = false
			}
			continue
		}
		b.WriteRune(r)
		started = true
	}
	if quoted || escape {
		return p, fmt.Errorf("unterminated quote")
	}
	if started {
		tokens = append(tokens, b.String())
	}
	if len(tokens) < 2 || len(tokens) > 18 {
		return p, fmt.Errorf("use <worker> <command> [name=value]")
	}
	p.Worker = tokens[0]
	p.Verb = tokens[1]
	p.Arguments = map[string]string{}
	if !protocol.ValidToken(p.Worker) || !protocol.ValidToken(p.Verb) {
		return p, fmt.Errorf("invalid addressing")
	}
	for _, t := range tokens[2:] {
		key, value, ok := strings.Cut(t, "=")
		if !ok && (p.Verb == "snooze" || p.Verb == "accept-risk") {
			key, value, ok = "duration", t, true
		}
		if !ok || !protocol.ValidToken(key) || value == "" {
			return p, fmt.Errorf("expected named argument")
		}
		if _, exists := p.Arguments[key]; exists {
			return p, fmt.Errorf("duplicate argument")
		}
		p.Arguments[key] = value
	}
	return p, nil
}

func Duration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		n, e := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32)
		if e != nil || n < 1 || n > 90 {
			return 0, fmt.Errorf("invalid duration")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, e := time.ParseDuration(value)
	if e != nil || d < time.Minute || d > 90*24*time.Hour {
		return 0, fmt.Errorf("invalid duration")
	}
	return d, nil
}
func ValidateArguments(d Descriptor, args map[string]string) error {
	for name, a := range d.Arguments {
		if a.Required && args[name] == "" {
			return fmt.Errorf("required argument missing")
		}
	}
	for name, value := range args {
		a, ok := d.Arguments[name]
		if !ok {
			return fmt.Errorf("unknown argument")
		}
		switch a.Kind {
		case "token":
			if !protocol.ValidToken(value) || utf8.RuneCountInString(value) > a.Max {
				return fmt.Errorf("invalid token")
			}
		case "text":
			if !utf8.ValidString(value) || utf8.RuneCountInString(value) > a.Max {
				return fmt.Errorf("text too long")
			}
		case "duration":
			duration, err := Duration(value)
			if err != nil || duration > time.Duration(a.Max)*time.Second {
				return fmt.Errorf("duration out of bounds")
			}
		default:
			return fmt.Errorf("unsupported argument")
		}
	}
	if d.TargetRequired && (args["subject"] == "" || args["target"] == "") {
		return fmt.Errorf("explicit subject and target required")
	}
	return nil
}
