package sip

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	pionsdp "github.com/pion/sdp/v3"
)

type MediaDirection uint8

const (
	SendRecv MediaDirection = iota
	SendOnly
	RecvOnly
	Inactive
)

type RTPMap struct {
	Encoding  string
	ClockRate int
}

type SDP struct {
	Connection     netip.Addr
	AudioPort      uint16
	Formats        []uint8
	RTPMaps        map[uint8]RTPMap
	FMTP           map[uint8]string
	TelephoneEvent int
	Direction      MediaDirection
	PacketTime     int
}

// ParseSDP uses Pion for RFC 4566 syntax and keeps Sutel's V1 policy here.
func ParseSDP(body []byte) (SDP, error) {
	var description pionsdp.SessionDescription
	if err := description.Unmarshal(body); err != nil {
		return SDP{}, fmt.Errorf("%w: parse SDP: %v", ErrMalformed, err)
	}
	if len(description.MediaDescriptions) != 1 || !strings.EqualFold(description.MediaDescriptions[0].MediaName.Media, "audio") {
		return SDP{}, fmt.Errorf("%w: V1 requires exactly one audio media section", ErrMalformed)
	}
	var audio *pionsdp.MediaDescription
	for _, media := range description.MediaDescriptions {
		if strings.EqualFold(media.MediaName.Media, "audio") {
			if audio != nil {
				return SDP{}, fmt.Errorf("%w: multiple audio media sections", ErrMalformed)
			}
			audio = media
		}
	}
	if audio == nil || audio.MediaName.Port.Value <= 0 || audio.MediaName.Port.Value > 65535 || len(audio.MediaName.Protos) != 2 || !strings.EqualFold(audio.MediaName.Protos[0], "RTP") || !strings.EqualFold(audio.MediaName.Protos[1], "AVP") {
		return SDP{}, fmt.Errorf("%w: missing or unsupported audio media", ErrMalformed)
	}
	result := SDP{
		AudioPort:      uint16(audio.MediaName.Port.Value),
		RTPMaps:        make(map[uint8]RTPMap),
		FMTP:           make(map[uint8]string),
		TelephoneEvent: -1,
		Direction:      SendRecv,
		PacketTime:     20,
	}
	for _, raw := range audio.MediaName.Formats {
		payload, err := strconv.ParseUint(raw, 10, 7)
		if err != nil {
			return SDP{}, fmt.Errorf("%w: invalid payload type", ErrMalformed)
		}
		result.Formats = append(result.Formats, uint8(payload))
	}
	if len(result.Formats) == 0 {
		return SDP{}, fmt.Errorf("%w: audio has no formats", ErrMalformed)
	}
	connection := audio.ConnectionInformation
	if connection == nil {
		connection = description.ConnectionInformation
	}
	if connection == nil || !strings.EqualFold(connection.NetworkType, "IN") || !strings.EqualFold(connection.AddressType, "IP4") || connection.Address == nil {
		return SDP{}, fmt.Errorf("%w: only IN IP4 media is supported", ErrMalformed)
	}
	address, err := netip.ParseAddr(connection.Address.Address)
	if err != nil || !address.Is4() || address.IsUnspecified() {
		return SDP{}, fmt.Errorf("%w: invalid IPv4 media connection", ErrMalformed)
	}
	result.Connection = address

	applyAttributes := func(attributes []pionsdp.Attribute) error {
		for _, attribute := range attributes {
			switch strings.ToLower(attribute.Key) {
			case "sendrecv":
				result.Direction = SendRecv
			case "sendonly":
				result.Direction = SendOnly
			case "recvonly":
				result.Direction = RecvOnly
			case "inactive":
				result.Direction = Inactive
			case "rtpmap":
				payload, mapping, err := parseRTPMap(attribute.Value)
				if err != nil {
					return err
				}
				result.RTPMaps[payload] = mapping
				if strings.EqualFold(mapping.Encoding, "telephone-event") {
					result.TelephoneEvent = int(payload)
				}
			case "fmtp":
				payload, format, err := parseFMTP(attribute.Value)
				if err != nil {
					return err
				}
				result.FMTP[payload] = format
			case "ptime":
				ptime, err := strconv.Atoi(strings.TrimSpace(attribute.Value))
				if err != nil || ptime <= 0 {
					return fmt.Errorf("%w: invalid ptime", ErrMalformed)
				}
				result.PacketTime = ptime
			}
		}
		return nil
	}
	if err := applyAttributes(description.Attributes); err != nil {
		return SDP{}, err
	}
	if err := applyAttributes(audio.Attributes); err != nil {
		return SDP{}, err
	}
	for _, payload := range result.Formats {
		mapping, exists := result.RTPMaps[payload]
		switch payload {
		case 0:
			if exists && (!strings.EqualFold(mapping.Encoding, "PCMU") || mapping.ClockRate != 8000) {
				return SDP{}, fmt.Errorf("%w: payload 0 is not PCMU/8000", ErrMalformed)
			}
			result.RTPMaps[payload] = RTPMap{Encoding: "PCMU", ClockRate: 8000}
		case 8:
			if exists && (!strings.EqualFold(mapping.Encoding, "PCMA") || mapping.ClockRate != 8000) {
				return SDP{}, fmt.Errorf("%w: payload 8 is not PCMA/8000", ErrMalformed)
			}
			result.RTPMaps[payload] = RTPMap{Encoding: "PCMA", ClockRate: 8000}
		}
	}
	if result.TelephoneEvent >= 0 && !result.HasFormat(uint8(result.TelephoneEvent)) {
		result.TelephoneEvent = -1
	}
	if result.TelephoneEvent >= 0 {
		if format, ok := result.FMTP[uint8(result.TelephoneEvent)]; ok {
			if err := validateTelephoneEvents(format); err != nil {
				return SDP{}, err
			}
		}
	}
	return result, nil
}

func parseFMTP(value string) (uint8, string, error) {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("%w: invalid fmtp", ErrMalformed)
	}
	payload, err := strconv.ParseUint(fields[0], 10, 7)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid fmtp payload", ErrMalformed)
	}
	return uint8(payload), strings.Join(fields[1:], " "), nil
}

func validateTelephoneEvents(value string) error {
	events := strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
	if events == "" {
		return fmt.Errorf("%w: empty telephone-event fmtp", ErrMalformed)
	}
	for _, item := range strings.Split(events, ",") {
		bounds := strings.Split(strings.TrimSpace(item), "-")
		if len(bounds) > 2 || len(bounds) == 0 {
			return fmt.Errorf("%w: invalid telephone-event fmtp", ErrMalformed)
		}
		first, err := strconv.ParseUint(bounds[0], 10, 8)
		if err != nil {
			return fmt.Errorf("%w: invalid telephone-event fmtp", ErrMalformed)
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.ParseUint(bounds[1], 10, 8)
			if err != nil || last < first {
				return fmt.Errorf("%w: invalid telephone-event fmtp", ErrMalformed)
			}
		}
	}
	return nil
}

func parseRTPMap(value string) (uint8, RTPMap, error) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, RTPMap{}, fmt.Errorf("%w: invalid rtpmap", ErrMalformed)
	}
	payload, err := strconv.ParseUint(fields[0], 10, 7)
	parts := strings.Split(fields[1], "/")
	if err != nil || len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		return 0, RTPMap{}, fmt.Errorf("%w: invalid rtpmap", ErrMalformed)
	}
	clock, err := strconv.Atoi(parts[1])
	if err != nil || clock <= 0 {
		return 0, RTPMap{}, fmt.Errorf("%w: invalid rtpmap clock rate", ErrMalformed)
	}
	return uint8(payload), RTPMap{Encoding: parts[0], ClockRate: clock}, nil
}

func (s SDP) HasFormat(payload uint8) bool {
	for _, item := range s.Formats {
		if item == payload {
			return true
		}
	}
	return false
}

// CanSend reports whether Sutel may send media under the remote description.
func (s SDP) CanSend() bool { return s.Direction == SendRecv || s.Direction == RecvOnly }

// CanReceive reports whether Sutel may receive media under the remote description.
func (s SDP) CanReceive() bool { return s.Direction == SendRecv || s.Direction == SendOnly }

// AnswerDirection returns the local direction compatible with a remote offer.
func (s SDP) AnswerDirection() MediaDirection {
	switch s.Direction {
	case SendOnly:
		return RecvOnly
	case RecvOnly:
		return SendOnly
	case Inactive:
		return Inactive
	default:
		return SendRecv
	}
}

func GenerateSDP(address netip.Addr, port uint16, audioPayloads []uint8, audioMappings map[uint8]RTPMap, telephoneEvent int, direction MediaDirection) ([]byte, error) {
	if !address.IsValid() || !address.Is4() || address.IsUnspecified() || port == 0 || len(audioPayloads) == 0 {
		return nil, fmt.Errorf("%w: invalid local media address", ErrMalformed)
	}
	description := &pionsdp.SessionDescription{
		Version: 0,
		Origin: pionsdp.Origin{
			Username: "sutel", SessionID: 1, SessionVersion: 1,
			NetworkType: "IN", AddressType: "IP4", UnicastAddress: address.String(),
		},
		SessionName: "Sutel",
		ConnectionInformation: &pionsdp.ConnectionInformation{
			NetworkType: "IN", AddressType: "IP4", Address: &pionsdp.Address{Address: address.String()},
		},
		TimeDescriptions: []pionsdp.TimeDescription{{Timing: pionsdp.Timing{StartTime: 0, StopTime: 0}}},
	}
	media := &pionsdp.MediaDescription{MediaName: pionsdp.MediaName{
		Media: "audio", Port: pionsdp.RangedPort{Value: int(port)}, Protos: []string{"RTP", "AVP"},
	}}
	for _, payload := range audioPayloads {
		if int(payload) == telephoneEvent {
			return nil, fmt.Errorf("%w: audio and telephone-event share payload %d", ErrMalformed, payload)
		}
		media.MediaName.Formats = append(media.MediaName.Formats, strconv.Itoa(int(payload)))
		mapping, ok := audioMappings[payload]
		if !ok {
			switch payload {
			case 0:
				mapping = RTPMap{Encoding: "PCMU", ClockRate: 8000}
			case 8:
				mapping = RTPMap{Encoding: "PCMA", ClockRate: 8000}
			default:
				return nil, fmt.Errorf("%w: audio payload %d requires rtpmap", ErrMalformed, payload)
			}
		}
		encoding := strings.ToUpper(strings.TrimSpace(mapping.Encoding))
		if mapping.ClockRate != 8000 || encoding != "PCMA" && encoding != "PCMU" || payload == 0 && encoding != "PCMU" || payload == 8 && encoding != "PCMA" {
			return nil, fmt.Errorf("%w: invalid audio rtpmap %d %s/%d", ErrMalformed, payload, mapping.Encoding, mapping.ClockRate)
		}
		media.Attributes = append(media.Attributes, pionsdp.NewAttribute("rtpmap", fmt.Sprintf("%d %s/8000", payload, encoding)))
	}
	if telephoneEvent >= 0 {
		if telephoneEvent > 127 {
			return nil, fmt.Errorf("%w: invalid telephone-event payload", ErrMalformed)
		}
		payload := strconv.Itoa(telephoneEvent)
		media.MediaName.Formats = append(media.MediaName.Formats, payload)
		media.Attributes = append(media.Attributes,
			pionsdp.NewAttribute("rtpmap", payload+" telephone-event/8000"),
			pionsdp.NewAttribute("fmtp", payload+" 0-16"),
		)
	}
	directionName := map[MediaDirection]string{SendRecv: "sendrecv", SendOnly: "sendonly", RecvOnly: "recvonly", Inactive: "inactive"}[direction]
	media.Attributes = append(media.Attributes, pionsdp.NewAttribute("ptime", "20"), pionsdp.NewPropertyAttribute(directionName))
	description.MediaDescriptions = []*pionsdp.MediaDescription{media}
	data, err := description.Marshal()
	if err != nil {
		return nil, fmt.Errorf("%w: marshal SDP: %v", ErrMalformed, err)
	}
	return data, nil
}
