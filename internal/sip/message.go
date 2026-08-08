package sip

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const Version = "SIP/2.0"

var (
	ErrMalformed = errors.New("malformed SIP message")
	ErrTooLarge  = errors.New("SIP message exceeds configured limit")
)

type Header struct {
	Name  string
	Value string
}

type Message struct {
	Method string
	URI    string

	StatusCode int
	Reason     string

	Headers []Header
	Body    []byte
}

func NewRequest(method, uri string) *Message {
	return &Message{Method: strings.ToUpper(method), URI: uri}
}

func NewResponse(status int, reason string) *Message {
	return &Message{StatusCode: status, Reason: reason}
}

func (m *Message) IsRequest() bool { return m != nil && m.Method != "" }

func (m *Message) Add(name, value string) {
	m.Headers = append(m.Headers, Header{Name: canonicalHeaderName(name), Value: value})
}

func (m *Message) Set(name, value string) {
	m.Del(name)
	m.Add(name, value)
}

func (m *Message) Del(name string) {
	canonical := canonicalHeaderName(name)
	filtered := m.Headers[:0]
	for _, header := range m.Headers {
		if canonicalHeaderName(header.Name) != canonical {
			filtered = append(filtered, header)
		}
	}
	m.Headers = filtered
}

func (m *Message) Get(name string) string {
	values := m.Values(name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (m *Message) Values(name string) []string {
	canonical := canonicalHeaderName(name)
	var values []string
	for _, header := range m.Headers {
		if canonicalHeaderName(header.Name) == canonical {
			if canonical == "Route" || canonical == "Record-Route" || canonical == "Via" {
				values = append(values, splitListHeader(header.Value)...)
			} else {
				values = append(values, header.Value)
			}
		}
	}
	return values
}

func splitListHeader(value string) []string {
	var result []string
	start, angleDepth, inQuote, escaped := 0, 0, false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if inQuote && character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch character {
		case '<':
			angleDepth++
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
		case ',':
			if angleDepth == 0 {
				if item := strings.TrimSpace(value[start:index]); item != "" {
					result = append(result, item)
				}
				start = index + 1
			}
		}
	}
	if item := strings.TrimSpace(value[start:]); item != "" {
		result = append(result, item)
	}
	return result
}

func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	clone := *m
	clone.Headers = append([]Header(nil), m.Headers...)
	clone.Body = append([]byte(nil), m.Body...)
	return &clone
}

func (m *Message) Bytes() ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil message", ErrMalformed)
	}
	var output bytes.Buffer
	if m.IsRequest() {
		if !validToken(m.Method) || !validFieldValue(m.URI) || m.URI == "" {
			return nil, fmt.Errorf("%w: invalid request line", ErrMalformed)
		}
		fmt.Fprintf(&output, "%s %s %s\r\n", strings.ToUpper(m.Method), m.URI, Version)
	} else {
		if m.StatusCode < 100 || m.StatusCode > 699 || !validFieldValue(m.Reason) {
			return nil, fmt.Errorf("%w: invalid status line", ErrMalformed)
		}
		fmt.Fprintf(&output, "%s %d %s\r\n", Version, m.StatusCode, m.Reason)
	}
	for _, header := range m.Headers {
		name := canonicalHeaderName(header.Name)
		if name == "Content-Length" {
			continue
		}
		if !validHeaderName(name) || !validFieldValue(header.Value) {
			return nil, fmt.Errorf("%w: invalid header %q", ErrMalformed, header.Name)
		}
		fmt.Fprintf(&output, "%s: %s\r\n", name, header.Value)
	}
	fmt.Fprintf(&output, "Content-Length: %d\r\n\r\n", len(m.Body))
	output.Write(m.Body)
	return output.Bytes(), nil
}

func Parse(data []byte, maximum int) (*Message, error) {
	if maximum <= 0 {
		maximum = 64 * 1024
	}
	if len(data) > maximum {
		return nil, ErrTooLarge
	}
	separator := []byte("\r\n\r\n")
	lineSeparator := "\r\n"
	index := bytes.Index(data, separator)
	if index < 0 {
		separator = []byte("\n\n")
		lineSeparator = "\n"
		index = bytes.Index(data, separator)
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: missing header terminator", ErrMalformed)
	}
	headerData := string(data[:index])
	bodyData := data[index+len(separator):]
	rawLines := strings.Split(headerData, lineSeparator)
	if len(rawLines) == 0 || strings.TrimSpace(rawLines[0]) == "" {
		return nil, fmt.Errorf("%w: missing start line", ErrMalformed)
	}
	lines := []string{strings.TrimSuffix(rawLines[0], "\r")}
	for _, raw := range rawLines[1:] {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(lines) == 1 {
				return nil, fmt.Errorf("%w: orphan folded header", ErrMalformed)
			}
			lines[len(lines)-1] += " " + strings.TrimSpace(line)
			continue
		}
		lines = append(lines, line)
	}
	message, err := parseStartLine(lines[0])
	if err != nil {
		return nil, err
	}
	var contentLengths []int
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("%w: invalid header line", ErrMalformed)
		}
		name := canonicalHeaderName(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		if !validHeaderName(name) || !validFieldValue(value) {
			return nil, fmt.Errorf("%w: invalid header %q", ErrMalformed, name)
		}
		message.Headers = append(message.Headers, Header{Name: name, Value: value})
		if name == "Content-Length" {
			length, err := strconv.Atoi(value)
			if err != nil || length < 0 {
				return nil, fmt.Errorf("%w: invalid Content-Length", ErrMalformed)
			}
			contentLengths = append(contentLengths, length)
		}
	}
	length := 0
	if len(contentLengths) > 0 {
		length = contentLengths[0]
		for _, value := range contentLengths[1:] {
			if value != length {
				return nil, fmt.Errorf("%w: conflicting Content-Length", ErrMalformed)
			}
		}
	} else {
		// UDP already provides a complete message boundary. Without an explicit
		// Content-Length, the body consumes the remainder of this datagram.
		length = len(bodyData)
	}
	if len(bodyData) != length {
		return nil, fmt.Errorf("%w: body length is %d, want %d", ErrMalformed, len(bodyData), length)
	}
	message.Body = append([]byte(nil), bodyData...)
	return message, nil
}

func parseStartLine(line string) (*Message, error) {
	if strings.HasPrefix(line, Version+" ") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: invalid status line", ErrMalformed)
		}
		status, err := strconv.Atoi(parts[1])
		if err != nil || status < 100 || status > 699 {
			return nil, fmt.Errorf("%w: invalid status code", ErrMalformed)
		}
		return NewResponse(status, parts[2]), nil
	}
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[2] != Version || !validToken(parts[0]) {
		return nil, fmt.Errorf("%w: invalid request line", ErrMalformed)
	}
	return NewRequest(parts[0], parts[1]), nil
}

var compactHeaders = map[string]string{
	"v": "Via", "f": "From", "t": "To", "i": "Call-ID",
	"m": "Contact", "l": "Content-Length", "c": "Content-Type",
}

func canonicalHeaderName(name string) string {
	trimmed := strings.TrimSpace(name)
	if expanded, ok := compactHeaders[strings.ToLower(trimmed)]; ok {
		return expanded
	}
	if special, ok := map[string]string{
		"call-id": "Call-ID", "cseq": "CSeq", "www-authenticate": "WWW-Authenticate",
	}[strings.ToLower(trimmed)]; ok {
		return special
	}
	parts := strings.Split(strings.ToLower(trimmed), "-")
	for index, part := range parts {
		if part != "" {
			parts[index] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "-")
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character <= 32 || character >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={} ", character) {
			return false
		}
	}
	return true
}

func validHeaderName(value string) bool { return validToken(value) }

func validFieldValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n")
}
