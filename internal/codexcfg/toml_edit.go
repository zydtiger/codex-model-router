package codexcfg

import (
	"fmt"
	"strconv"
	"strings"
)

// This file edits root-level TOML string assignments without reformatting the
// file. The rules are deliberately conservative: anything the line scanner is not
// sure about is an error, never a guess.

type valueKind int

const (
	kindString valueKind = iota
	kindOther
	kindMultiLine
)

// rootValue is one root-level assignment discovered in the text.
type rootValue struct {
	key      string
	raw      string
	value    string
	kind     valueKind
	line     int
	valuePos int
	valueEnd int
}

type rootLine struct {
	text   string // content without the line ending
	suffix string // the original line ending
}

// parseRoot walks the root section of a TOML document.
func parseRoot(text string) (*rootSection, error) {
	lines := splitLines(text)
	section := &rootSection{lines: lines, values: map[string]*rootValue{}, rootEnd: len(lines)}
	for index := range lines {
		line := lines[index].text
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section.rootEnd = index
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, valueOffset, ok := assignmentKey(line)
		if !ok {
			// Not an assignment. The TOML parse in BuildPlan already rejected
			// malformed input, so this is a line the editor must not touch.
			continue
		}
		value, kind := scanValue(line, valueOffset)
		end := valueEnd(line, valueOffset)
		section.values[key] = &rootValue{
			key:      key,
			raw:      strings.TrimSpace(line[valueOffset:end]),
			value:    value,
			kind:     kind,
			line:     index,
			valuePos: valueOffset,
			valueEnd: end,
		}
	}
	return section, nil
}

// rootSection is the parsed root of a config file.
type rootSection struct {
	lines   []rootLine
	values  map[string]*rootValue
	rootEnd int
}

func (s *rootSection) value(key string) *rootValue {
	return s.values[key]
}

// assignmentKey splits "key = value" and reports the key plus the offset of the
// value. A dotted key belongs to a nested table, so it is reported under its
// dotted spelling and never matches a bare root key.
func assignmentKey(line string) (string, int, bool) {
	position := 0
	for position < len(line) && (line[position] == ' ' || line[position] == '\t') {
		position++
	}

	var key string
	if position < len(line) && (line[position] == '"' || line[position] == '\'') {
		quote := line[position]
		position++
		closed := strings.IndexByte(line[position:], quote)
		if closed < 0 {
			return "", 0, false
		}
		key = line[position : position+closed]
		position += closed + 1
	} else {
		start := position
		for position < len(line) && line[position] != ' ' && line[position] != '\t' && line[position] != '=' {
			position++
		}
		key = line[start:position]
		if key == "" {
			return "", 0, false
		}
	}

	for position < len(line) && (line[position] == ' ' || line[position] == '\t') {
		position++
	}
	if position >= len(line) || line[position] != '=' {
		return "", 0, false
	}
	position++
	for position < len(line) && (line[position] == ' ' || line[position] == '\t') {
		position++
	}
	return key, position, true
}

// scanValue reports the decoded string value and how the value is written.
func scanValue(line string, offset int) (string, valueKind) {
	if offset >= len(line) {
		return "", kindOther
	}
	body := line[offset:]
	switch body[0] {
	case '"':
		if strings.HasPrefix(body, `"""`) {
			return "", kindMultiLine
		}
		decoded, length, ok := decodeBasicString(body)
		if !ok {
			return "", kindMultiLine
		}
		if trailing := strings.TrimSpace(body[length:]); trailing != "" && !strings.HasPrefix(trailing, "#") {
			// A value followed by something other than a comment is not the
			// simple shape this editor can rewrite.
			return "", kindOther
		}
		return decoded, kindString
	case '\'':
		if strings.HasPrefix(body, "'''") {
			return "", kindMultiLine
		}
		end := strings.IndexByte(body[1:], '\'')
		if end < 0 {
			return "", kindMultiLine
		}
		if trailing := strings.TrimSpace(body[end+2:]); trailing != "" && !strings.HasPrefix(trailing, "#") {
			return "", kindOther
		}
		return body[1 : end+1], kindString
	}
	if strings.HasPrefix(body, "[") || strings.HasPrefix(body, "{") {
		return "", kindMultiLine
	}
	return strings.TrimSpace(stripComment(body)), kindOther
}

// valueEnd is the byte offset just past the value, before any trailing comment.
func valueEnd(line string, offset int) int {
	if offset >= len(line) {
		return len(line)
	}
	body := line[offset:]
	switch body[0] {
	case '"':
		if strings.HasPrefix(body, `"""`) {
			return len(line)
		}
		_, length, ok := decodeBasicString(body)
		if !ok {
			return len(line)
		}
		return offset + length
	case '\'':
		if strings.HasPrefix(body, "'''") {
			return len(line)
		}
		end := strings.IndexByte(body[1:], '\'')
		if end < 0 {
			return len(line)
		}
		return offset + end + 2
	}
	if strings.HasPrefix(body, "[") || strings.HasPrefix(body, "{") {
		return len(line)
	}
	trimmed := strings.TrimRight(body, " \t")
	if comment := strings.IndexByte(trimmed, '#'); comment >= 0 {
		return offset + comment
	}
	return offset + len(trimmed)
}

// decodeBasicString reads one TOML basic string and returns its decoded value and
// the number of source bytes consumed.
func decodeBasicString(source string) (string, int, bool) {
	if len(source) == 0 || source[0] != '"' {
		return "", 0, false
	}
	var builder strings.Builder
	index := 1
	for index < len(source) {
		char := source[index]
		switch char {
		case '"':
			return builder.String(), index + 1, true
		case '\\':
			if index+1 >= len(source) {
				return "", 0, false
			}
			next := source[index+1]
			switch next {
			case '"', '\\', '/':
				builder.WriteByte(next)
				index += 2
			case 'b':
				builder.WriteByte('\b')
				index += 2
			case 't':
				builder.WriteByte('\t')
				index += 2
			case 'n':
				builder.WriteByte('\n')
				index += 2
			case 'f':
				builder.WriteByte('\f')
				index += 2
			case 'r':
				builder.WriteByte('\r')
				index += 2
			default:
				// \u and \U escapes are not needed for a path or URL; treat them
				// as an unsupported shape rather than guessing.
				return "", 0, false
			}
		case '\n', '\r':
			return "", 0, false
		default:
			builder.WriteByte(char)
			index++
		}
	}
	return "", 0, false
}

func stripComment(value string) string {
	if comment := strings.IndexByte(value, '#'); comment >= 0 {
		return value[:comment]
	}
	return value
}

func splitLines(text string) []rootLine {
	var lines []rootLine
	for len(text) > 0 {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			lines = append(lines, rootLine{text: text})
			return lines
		}
		suffix := "\n"
		body := text[:index]
		if strings.HasSuffix(body, "\r") {
			body = strings.TrimSuffix(body, "\r")
			suffix = "\r\n"
		}
		lines = append(lines, rootLine{text: body, suffix: suffix})
		text = text[index+1:]
	}
	return lines
}

func joinLines(lines []rootLine) string {
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString(line.text)
		builder.WriteString(line.suffix)
	}
	return builder.String()
}

// setRootString writes one root-level string assignment, replacing an existing
// value in place or adding the key at the end of the root section.
func setRootString(text, key, value string) (string, error) {
	section, err := parseRoot(text)
	if err != nil {
		return "", err
	}
	assignment := fmt.Sprintf("%s = %s", key, strconv.Quote(value))
	if existing := section.value(key); existing != nil {
		switch existing.kind {
		case kindString, kindOther:
			line := section.lines[existing.line].text
			section.lines[existing.line].text = line[:existing.valuePos] + strconv.Quote(value) + padValueTail(line, existing.valueEnd)
			return joinLines(section.lines), nil
		default:
			return "", fmt.Errorf("%s has a multi-line or quoted form that this tool will not rewrite; set it by hand", key)
		}
	}

	insertAt := section.rootEnd
	if last := lastAssignmentLine(section); last >= 0 {
		insertAt = last + 1
	}
	suffix := "\n"
	if insertAt < len(section.lines) {
		if section.lines[insertAt].suffix != "" {
			suffix = section.lines[insertAt].suffix
		}
	} else if len(section.lines) > 0 && section.lines[len(section.lines)-1].suffix != "" {
		suffix = section.lines[len(section.lines)-1].suffix
	}
	if len(section.lines) == 0 {
		section.lines = []rootLine{{text: assignment, suffix: suffix}}
		return joinLines(section.lines), nil
	}

	// A new key placed directly before a table header needs a blank line so the
	// header keeps the visual separation it had from the root section. insertAt is
	// zero when the file begins with a table, which is also when there is no previous
	// line to measure.
	needBlankAfter := insertAt > 0 && insertAt < len(section.lines) &&
		strings.HasPrefix(strings.TrimSpace(section.lines[insertAt].text), "[") &&
		strings.TrimSpace(section.lines[insertAt-1].text) != ""
	inserted := []rootLine{{text: assignment, suffix: suffix}}
	if needBlankAfter {
		inserted = append(inserted, rootLine{text: "", suffix: suffix})
	}
	section.lines = append(section.lines[:insertAt], append(inserted, section.lines[insertAt:]...)...)
	return joinLines(section.lines), nil
}

// padValueTail keeps a trailing comment when a value is replaced.
func padValueTail(line string, end int) string {
	tail := line[end:]
	if strings.TrimSpace(tail) == "" {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(tail), "#") {
		return " " + strings.TrimSpace(tail)
	}
	return ""
}

func lastAssignmentLine(section *rootSection) int {
	last := -1
	for _, existing := range section.values {
		if existing.line > last {
			last = existing.line
		}
	}
	return last
}

// removeRootKey deletes one root-level assignment line.
func removeRootKey(text, key string) (string, error) {
	section, err := parseRoot(text)
	if err != nil {
		return "", err
	}
	existing := section.value(key)
	if existing == nil {
		return text, nil
	}
	if existing.kind == kindMultiLine {
		return "", fmt.Errorf("%s has a multi-line form that this tool will not delete; remove it by hand", key)
	}
	section.lines = append(section.lines[:existing.line], section.lines[existing.line+1:]...)
	return joinLines(section.lines), nil
}

// RootString reads a root-level string value for tests and diagnostics.
func RootString(text, key string) (string, bool, error) {
	section, err := parseRoot(text)
	if err != nil {
		return "", false, err
	}
	value := section.value(key)
	if value == nil {
		return "", false, nil
	}
	if value.kind != kindString {
		return value.raw, true, nil
	}
	return value.value, true, nil
}
