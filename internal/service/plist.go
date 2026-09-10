package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// node is a minimal XML element used to read back a plist without adding a
// plist dependency for the template this package writes.
type node struct {
	name     string
	text     string
	children []*node
}

// parsePlist reads an XML plist and returns its top-level dict as a map. Values
// are string, bool, []any, or map[string]any.
func parsePlist(content []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, errors.New("empty plist")
	}
	decoder := xml.NewDecoder(bytes.NewReader(trimmed))
	decoder.Strict = true
	var root *node
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no plist document found")
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		parsed, err := parseNode(decoder, start)
		if err != nil {
			return nil, err
		}
		if parsed.name == "plist" {
			root = parsed
			break
		}
	}
	if root == nil {
		return nil, errors.New("no plist element found")
	}
	for _, child := range root.children {
		if child.name == "dict" {
			return plistDict(child)
		}
	}
	return nil, errors.New("plist has no top-level dict")
}

func parseNode(decoder *xml.Decoder, start xml.StartElement) (*node, error) {
	current := &node{name: start.Name.Local}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch element := token.(type) {
		case xml.StartElement:
			child, err := parseNode(decoder, element)
			if err != nil {
				return nil, err
			}
			current.children = append(current.children, child)
		case xml.CharData:
			current.text += string(element)
		case xml.EndElement:
			return current, nil
		}
	}
}

func plistDict(dict *node) (map[string]any, error) {
	result := make(map[string]any, len(dict.children)/2)
	for index := 0; index < len(dict.children); index += 2 {
		key := dict.children[index]
		if key.name != "key" {
			return nil, fmt.Errorf("plist dict contains %q where a key was expected", key.name)
		}
		if index+1 >= len(dict.children) {
			return nil, fmt.Errorf("plist key %q has no value", key.text)
		}
		value, err := plistValue(dict.children[index+1])
		if err != nil {
			return nil, fmt.Errorf("plist key %q: %w", key.text, err)
		}
		result[strings.TrimSpace(key.text)] = value
	}
	return result, nil
}

func plistValue(item *node) (any, error) {
	switch item.name {
	case "string", "integer", "real", "date", "data":
		return strings.TrimSpace(item.text), nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "array":
		values := make([]any, 0, len(item.children))
		for _, child := range item.children {
			value, err := plistValue(child)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case "dict":
		return plistDict(item)
	default:
		return nil, fmt.Errorf("unsupported plist element %q", item.name)
	}
}

// stringValue reads a string field out of a parsed plist.
func stringValue(document map[string]any, key string) (string, bool) {
	value, ok := document[key].(string)
	return value, ok
}

// stringSliceValue reads an array of strings out of a parsed plist.
func stringSliceValue(document map[string]any, key string) ([]string, bool) {
	values, ok := document[key].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}
