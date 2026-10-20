// Package plist decodes XML property lists (as printed by diskutil and
// hdiutil with -plist) into plain Go values: map[string]any, []any,
// string, int64, float64, bool and []byte.
package plist

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Decode parses an XML plist document.
func Decode(data []byte) (any, error) {
	d := xml.NewDecoder(strings.NewReader(string(data)))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return value(d, se)
		}
	}
}

func value(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		var key string
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					if key, err = text(d); err != nil {
						return nil, err
					}
					continue
				}
				v, err := value(d, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := value(d, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "true", "false":
		d.Skip()
		return se.Name.Local == "true", nil
	}
	s, err := text(d)
	if err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "integer":
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	case "real":
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	case "data":
		return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	case "string", "date":
		return s, nil
	}
	return nil, fmt.Errorf("plist: unsupported element <%s>", se.Name.Local)
}

func text(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return sb.String(), nil
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		}
	}
}

// Dict, Arr, Str, Int and Bool read typed values without panicking.
func Dict(v any) map[string]any { m, _ := v.(map[string]any); return m }
func Arr(v any) []any           { a, _ := v.([]any); return a }
func Str(v any) string          { s, _ := v.(string); return s }
func Int(v any) int64           { i, _ := v.(int64); return i }
func Bool(v any) bool           { b, _ := v.(bool); return b }
