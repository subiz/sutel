package sip

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

type CSeq struct {
	Number uint32
	Method string
}

func ParseCSeq(value string) (CSeq, error) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return CSeq{}, fmt.Errorf("%w: invalid CSeq", ErrMalformed)
	}
	number, err := strconv.ParseUint(fields[0], 10, 31)
	if err != nil || !validToken(fields[1]) {
		return CSeq{}, fmt.Errorf("%w: invalid CSeq", ErrMalformed)
	}
	return CSeq{Number: uint32(number), Method: strings.ToUpper(fields[1])}, nil
}

func (m *Message) CSeq() (CSeq, error) { return ParseCSeq(m.Get("CSeq")) }

type URI struct {
	User       string
	Host       string
	Port       uint16
	Parameters map[string]string
}

func ParseURI(value string) (URI, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(trimmed), "sip:") {
		return URI{}, fmt.Errorf("%w: unsupported SIP URI", ErrMalformed)
	}
	rest := trimmed[4:]
	parts := strings.Split(rest, ";")
	authority := parts[0]
	at := strings.LastIndex(authority, "@")
	uri := URI{Parameters: make(map[string]string)}
	hostPort := authority
	if at >= 0 {
		if at == 0 || at == len(authority)-1 {
			return URI{}, fmt.Errorf("%w: invalid SIP URI authority", ErrMalformed)
		}
		uri.User = authority[:at]
		hostPort = authority[at+1:]
	}
	if strings.HasPrefix(hostPort, "[") {
		return URI{}, fmt.Errorf("%w: IPv6 SIP URI is outside V1", ErrMalformed)
	}
	if colon := strings.LastIndexByte(hostPort, ':'); colon >= 0 {
		uri.Host = hostPort[:colon]
		port, err := strconv.ParseUint(hostPort[colon+1:], 10, 16)
		if err != nil || port == 0 {
			return URI{}, fmt.Errorf("%w: invalid SIP URI port", ErrMalformed)
		}
		uri.Port = uint16(port)
	} else {
		uri.Host = hostPort
		uri.Port = 5060
	}
	if uri.Host == "" || strings.ContainsAny(uri.User, "\r\n<>") {
		return URI{}, fmt.Errorf("%w: invalid SIP URI", ErrMalformed)
	}
	address, err := netip.ParseAddr(uri.Host)
	if err != nil || !address.Is4() || address.IsUnspecified() {
		return URI{}, fmt.Errorf("%w: SIP URI requires a concrete IPv4 address", ErrMalformed)
	}
	for _, parameter := range parts[1:] {
		key, value, _ := strings.Cut(parameter, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			uri.Parameters[key] = strings.TrimSpace(value)
		}
	}
	if transport := uri.Parameters["transport"]; transport != "" && !strings.EqualFold(transport, "udp") {
		return URI{}, fmt.Errorf("%w: only UDP SIP URI is supported", ErrMalformed)
	}
	return uri, nil
}

func URIUser(value string) (string, error) {
	address, err := ParseAddress(value)
	if err != nil {
		return "", err
	}
	return address.URI.User, nil
}

type Address struct {
	DisplayName string
	URI         URI
	RawURI      string
	Tag         string
	Parameters  map[string]string
}

func ParseAddress(value string) (Address, error) {
	trimmed := strings.TrimSpace(value)
	var rawURI, suffix, display string
	left, err := indexOutsideQuotes(trimmed, '<')
	if err != nil {
		return Address{}, fmt.Errorf("%w: invalid quoted address", ErrMalformed)
	}
	if left >= 0 {
		rightOffset, findErr := indexOutsideQuotes(trimmed[left+1:], '>')
		if findErr != nil || rightOffset < 0 {
			return Address{}, fmt.Errorf("%w: invalid name-addr", ErrMalformed)
		}
		right := rightOffset + left + 1
		displayValue := strings.TrimSpace(trimmed[:left])
		if strings.HasPrefix(displayValue, "\"") {
			unquoted, unquoteErr := unquoteSIP(displayValue)
			if unquoteErr != nil {
				return Address{}, fmt.Errorf("%w: invalid quoted display name", ErrMalformed)
			}
			display = unquoted
		} else {
			display = displayValue
		}
		rawURI = strings.TrimSpace(trimmed[left+1 : right])
		suffix = trimmed[right+1:]
	} else {
		semicolon := strings.IndexByte(trimmed, ';')
		if semicolon < 0 {
			rawURI = trimmed
		} else {
			rawURI = trimmed[:semicolon]
			suffix = trimmed[semicolon:]
		}
	}
	uri, err := ParseURI(rawURI)
	if err != nil {
		return Address{}, err
	}
	address := Address{DisplayName: display, URI: uri, RawURI: rawURI, Parameters: make(map[string]string)}
	parameters, err := splitOutsideQuotes(strings.TrimPrefix(suffix, ";"), ';')
	if err != nil {
		return Address{}, fmt.Errorf("%w: invalid quoted parameter", ErrMalformed)
	}
	for _, parameter := range parameters {
		key, parameterValue, _ := strings.Cut(parameter, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		address.Parameters[key] = strings.TrimSpace(parameterValue)
	}
	address.Tag = address.Parameters["tag"]
	return address, nil
}

func unquoteSIP(value string) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("invalid quoted string")
	}
	var result strings.Builder
	for index := 1; index < len(value)-1; index++ {
		character := value[index]
		if character == '\\' {
			index++
			if index >= len(value)-1 {
				return "", fmt.Errorf("invalid quoted-pair")
			}
			character = value[index]
		}
		if character == '\r' || character == '\n' {
			return "", fmt.Errorf("invalid quoted string")
		}
		result.WriteByte(character)
	}
	return result.String(), nil
}

func indexOutsideQuotes(value string, target byte) (int, error) {
	inQuote, escaped := false, false
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
		if !inQuote && character == target {
			return index, nil
		}
	}
	if inQuote || escaped {
		return -1, fmt.Errorf("unterminated quoted string")
	}
	return -1, nil
}

func splitOutsideQuotes(value string, separator byte) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	var result []string
	start, inQuote, escaped := 0, false, false
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
		if !inQuote && character == separator {
			result = append(result, value[start:index])
			start = index + 1
		}
	}
	if inQuote || escaped {
		return nil, fmt.Errorf("unterminated quoted string")
	}
	return append(result, value[start:]), nil
}

func HeaderParameter(value, name string) string {
	address, err := ParseAddress(value)
	if err != nil {
		return ""
	}
	return address.Parameters[strings.ToLower(name)]
}

func ViaBranch(value string) string {
	for _, parameter := range strings.Split(value, ";")[1:] {
		key, item, _ := strings.Cut(parameter, "=")
		if strings.EqualFold(strings.TrimSpace(key), "branch") {
			return strings.TrimSpace(item)
		}
	}
	return ""
}

func RequestTarget(message *Message, fallback netip.AddrPort) netip.AddrPort {
	if message == nil {
		return fallback
	}
	address, err := ParseAddress(message.Get("Contact"))
	if err != nil {
		return fallback
	}
	ip, err := netip.ParseAddr(address.URI.Host)
	if err != nil || !ip.Is4() {
		return fallback
	}
	return netip.AddrPortFrom(ip, address.URI.Port)
}

func Reason(status int) string {
	switch status {
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 183:
		return "Session Progress"
	case 200:
		return "OK"
	case 400:
		return "Bad Request"
	case 404:
		return "Not Found"
	case 415:
		return "Unsupported Media Type"
	case 420:
		return "Bad Extension"
	case 481:
		return "Call/Transaction Does Not Exist"
	case 486:
		return "Busy Here"
	case 487:
		return "Request Terminated"
	case 488:
		return "Not Acceptable Here"
	case 500:
		return "Server Internal Error"
	case 501:
		return "Not Implemented"
	case 503:
		return "Service Unavailable"
	case 603:
		return "Decline"
	default:
		return "Response"
	}
}
