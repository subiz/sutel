package sutel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/subiz/sutel/internal/sip"
	"github.com/subiz/sutel/rtpverifier"
)

type callDialog struct {
	callID          string
	localTag        string
	remoteTag       string
	localAddress    string
	remoteAddress   string
	remoteTarget    string
	peer            netip.AddrPort
	inviteCSeq      uint32
	remoteCSeq      uint32
	localCSeq       uint32
	inviteBranch    string
	codec           Codec
	payload         uint8
	telephoneEvent  int
	remoteMedia     netip.AddrPort
	remoteSDP       sip.SDP
	lastInviteOK    *sip.Message
	inviteACK       *sip.Message
	inviteACKTarget netip.AddrPort
	invite          *sip.Message
	routeSet        []string
}

type sipDatagram struct {
	message  *sip.Message
	source   netip.AddrPort
	parseErr error
	err      error
}

func (s *Session) sendSIP(message *sip.Message, target netip.AddrPort, retransmission bool) error {
	applyResponseVia(message, target)
	data, err := message.Bytes()
	if err != nil {
		return fmt.Errorf("%w: serialize message: %v", ErrProtocol, err)
	}
	if err := s.carrier.send(data, target); err != nil {
		return err
	}
	s.stateMu.Lock()
	if message.IsRequest() {
		s.sipStats.RequestsSent++
	} else {
		s.sipStats.ResponsesSent++
	}
	if retransmission {
		s.sipStats.Retransmissions++
	}
	if !s.sipStats.RemoteAddr.IsValid() {
		s.sipStats.RemoteAddr = target
	}
	s.stateMu.Unlock()
	typeName := message.Method
	detail := message.Method
	if !message.IsRequest() {
		typeName = strconv.Itoa(message.StatusCode)
		detail = fmt.Sprintf("%d %s", message.StatusCode, message.Reason)
	}
	s.addEvent(SIPLayer, Sent, typeName, detail)
	return nil
}

func applyResponseVia(message *sip.Message, source netip.AddrPort) {
	if message == nil || message.IsRequest() || !source.IsValid() {
		return
	}
	for index := range message.Headers {
		if !strings.EqualFold(message.Headers[index].Name, "Via") {
			continue
		}
		parts := strings.Split(message.Headers[index].Value, ";")
		fields := strings.Fields(parts[0])
		advertisedIP := netip.Addr{}
		if len(fields) > 0 {
			sentBy := fields[len(fields)-1]
			if address, err := netip.ParseAddrPort(sentBy); err == nil {
				advertisedIP = address.Addr()
			} else if address, err := netip.ParseAddr(sentBy); err == nil {
				advertisedIP = address
			}
		}
		hasReceived := false
		for parameterIndex := 1; parameterIndex < len(parts); parameterIndex++ {
			key, _, _ := strings.Cut(strings.TrimSpace(parts[parameterIndex]), "=")
			switch strings.ToLower(key) {
			case "rport":
				parts[parameterIndex] = fmt.Sprintf("rport=%d", source.Port())
			case "received":
				parts[parameterIndex] = "received=" + source.Addr().String()
				hasReceived = true
			}
		}
		if advertisedIP.IsValid() && advertisedIP.Unmap() != source.Addr().Unmap() && !hasReceived {
			parts = append(parts, "received="+source.Addr().String())
		}
		message.Headers[index].Value = strings.Join(parts, ";")
		return
	}
}

func (s *Session) readSIP(ctx context.Context) (*sip.Message, netip.AddrPort, error) {
	message, source, _, err := s.readSIPWithTimers(ctx, nil, nil, nil)
	return message, source, err
}

type sipReadTimeout uint8

const (
	noSIPReadTimeout sipReadTimeout = iota
	retransmitSIPReadTimeout
	deadlineSIPReadTimeout
	localHangupSIPReadTimeout
)

func (s *Session) readSIPWithTimers(ctx context.Context, retransmit, deadline <-chan time.Time, localHangup <-chan struct{}) (*sip.Message, netip.AddrPort, sipReadTimeout, error) {
	for {
		var datagram sipDatagram
		select {
		case <-ctx.Done():
			return nil, netip.AddrPort{}, noSIPReadTimeout, ctx.Err()
		case <-deadline:
			return nil, netip.AddrPort{}, deadlineSIPReadTimeout, nil
		case <-localHangup:
			return nil, netip.AddrPort{}, localHangupSIPReadTimeout, nil
		case <-retransmit:
			// Prefer the transaction deadline when both timers become ready in the
			// same clock advance; no retransmission is useful at expiry.
			select {
			case <-deadline:
				return nil, netip.AddrPort{}, deadlineSIPReadTimeout, nil
			default:
			}
			return nil, netip.AddrPort{}, retransmitSIPReadTimeout, nil
		case datagram = <-s.sipInput:
		}
		if datagram.err != nil {
			return nil, netip.AddrPort{}, noSIPReadTimeout, datagram.err
		}
		if datagram.parseErr != nil {
			s.stateMu.Lock()
			s.sipStats.MalformedMessages++
			s.stateMu.Unlock()
			s.addEvent(SIPLayer, Received, "malformed", datagram.parseErr.Error())
			continue
		}
		message, source := datagram.message, datagram.source
		s.stateMu.Lock()
		if message.IsRequest() {
			s.sipStats.RequestsReceived++
		} else {
			s.sipStats.ResponsesReceived++
		}
		if !s.sipStats.RemoteAddr.IsValid() {
			s.sipStats.RemoteAddr = source
		}
		s.stateMu.Unlock()
		typeName, detail := message.Method, message.Method
		if !message.IsRequest() {
			typeName = strconv.Itoa(message.StatusCode)
			detail = fmt.Sprintf("%d %s", message.StatusCode, message.Reason)
		}
		s.addEvent(SIPLayer, Received, typeName, detail)
		return message, source, noSIPReadTimeout, nil
	}
}

func (s *Session) enqueueSIP(datagram sipDatagram) {
	select {
	case <-s.ctx.Done():
		return
	case s.sipInput <- datagram:
		return
	default:
		s.stateMu.Lock()
		s.sipStats.IgnoredMessages++
		s.stateMu.Unlock()
		s.addEvent(SIPLayer, Received, "dropped", "SIP input queue full")
	}
}

func (s *Session) ignoreMessage() {
	s.stateMu.Lock()
	s.sipStats.IgnoredMessages++
	s.stateMu.Unlock()
}

func responseFor(request *sip.Message, status int, localTag string, body []byte) *sip.Message {
	response := sip.NewResponse(status, sip.Reason(status))
	for _, via := range request.Values("Via") {
		response.Add("Via", via)
	}
	for _, route := range request.Values("Record-Route") {
		response.Add("Record-Route", route)
	}
	response.Add("From", request.Get("From"))
	to := request.Get("To")
	if localTag != "" && sip.HeaderParameter(to, "tag") == "" {
		to += ";tag=" + localTag
	}
	response.Add("To", to)
	response.Add("Call-ID", request.Get("Call-ID"))
	response.Add("CSeq", request.Get("CSeq"))
	if len(body) > 0 {
		response.Add("Content-Type", "application/sdp")
		response.Body = body
	}
	return response
}

func localContact(address netip.AddrPort, user string) string {
	return fmt.Sprintf("<sip:%s@%s:%d;transport=udp>", sanitizeUser(user), address.Addr(), address.Port())
}

func sanitizeUser(value string) string {
	if value == "" || strings.ContainsAny(value, "\r\n<>@; ") {
		return "sutel"
	}
	return value
}

func sipURI(user string, address netip.AddrPort) string {
	return fmt.Sprintf("sip:%s@%s:%d;transport=udp", sanitizeUser(user), address.Addr(), address.Port())
}

func appendRequestHeaders(request *sip.Message, branch, from, to, callID string, cseq uint32, contact string) {
	request.Add("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", requestLocalHost(contact), branch))
	request.Add("Max-Forwards", "70")
	request.Add("From", from)
	request.Add("To", to)
	request.Add("Call-ID", callID)
	request.Add("CSeq", fmt.Sprintf("%d %s", cseq, request.Method))
	if contact != "" {
		request.Add("Contact", contact)
	}
}

func requestLocalHost(contact string) string {
	address, err := sip.ParseAddress(contact)
	if err != nil {
		return "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", address.URI.Host, address.URI.Port)
}

func isMatching(message *sip.Message, callID string, cseq uint32, method string) bool {
	if message.Get("Call-ID") != callID {
		return false
	}
	parsed, err := message.CSeq()
	return err == nil && parsed.Number == cseq && strings.EqualFold(parsed.Method, method)
}

func chooseCodec(description sip.SDP, codecs []Codec) (Codec, uint8, bool) {
	for _, codec := range codecs {
		for _, payload := range description.Formats {
			mapping, ok := description.RTPMaps[payload]
			if !ok || mapping.ClockRate != 8000 {
				continue
			}
			if (codec == PCMA && strings.EqualFold(mapping.Encoding, "PCMA")) || (codec == PCMU && strings.EqualFold(mapping.Encoding, "PCMU")) {
				return codec, payload, true
			}
		}
	}
	return PCMA, 0, false
}

func (s *Session) configureMedia(dialog *callDialog) error {
	if !s.mediaAddressAllowed(dialog.remoteSDP.Connection) {
		return fmt.Errorf("%w: media address %s is outside local test boundary", ErrNegotiation, dialog.remoteSDP.Connection)
	}
	payloads := map[uint8]rtpverifier.Codec{dialog.payload: rtpverifier.PCMA}
	if dialog.codec == PCMU {
		payloads[dialog.payload] = rtpverifier.PCMU
	}
	if err := s.receiver.Configure(payloads, dialog.telephoneEvent, dialog.remoteSDP.Connection); err != nil {
		return fmt.Errorf("%w: configure RTP: %v", ErrNegotiation, err)
	}
	dialog.remoteMedia = netip.AddrPortFrom(dialog.remoteSDP.Connection, dialog.remoteSDP.AudioPort)
	s.stateMu.Lock()
	s.media = NegotiatedMedia{AudioCodec: dialog.codec, TelephoneEventPayloadType: dialog.telephoneEvent}
	s.stateMu.Unlock()
	return nil
}

func (s *Session) mediaAddressAllowed(address netip.Addr) bool {
	local := netip.MustParseAddr(s.carrier.config.LocalIP)
	if local.IsLoopback() {
		return address.IsLoopback()
	}
	return address == local
}

func (s *Session) setOutcome(update func(*SIPOutcome)) {
	s.stateMu.Lock()
	update(&s.outcome)
	s.stateMu.Unlock()
}

func parseDTMFInfo(message *sip.Message) (string, time.Duration, int) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(message.Get("Content-Type"), ";")[0]))
	if contentType != "application/dtmf-relay" {
		return "", 0, 415
	}
	fields := make(map[string]string)
	for _, raw := range strings.Split(strings.ReplaceAll(string(message.Body), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if _, exists := fields[key]; !ok || key == "" || exists {
			return "", 0, 400
		}
		fields[key] = strings.TrimSpace(value)
	}
	digit := strings.ToUpper(fields["signal"])
	if err := validateDTMFDigits(digit); err != nil || len(digit) != 1 {
		return "", 0, 400
	}
	durationMS, err := strconv.Atoi(fields["duration"])
	if err != nil || durationMS <= 0 || durationMS > 10000 {
		return "", 0, 400
	}
	return digit, time.Duration(durationMS) * time.Millisecond, 200
}

func (s *Session) handleDialogRequest(message *sip.Message, source netip.AddrPort, dialog *callDialog) (bool, error) {
	if !message.IsRequest() {
		if dialog.inviteACK != nil && message.StatusCode >= 200 && message.StatusCode < 300 && isMatching(message, dialog.callID, dialog.inviteCSeq, "INVITE") {
			if sip.HeaderParameter(message.Get("To"), "tag") != dialog.remoteTag {
				return true, s.terminateForkedDialog(message, source, dialog)
			}
			return false, s.sendSIP(dialog.inviteACK, dialog.inviteACKTarget, true)
		}
		s.ignoreMessage()
		return false, nil
	}
	if message.Get("Call-ID") != dialog.callID {
		s.ignoreMessage()
		return false, nil
	}
	method := strings.ToUpper(message.Method)
	if method == "ACK" {
		return false, nil
	}
	if method == "INVITE" && isMatching(message, dialog.callID, dialog.inviteCSeq, "INVITE") && sip.ViaBranch(message.Get("Via")) == dialog.inviteBranch && dialog.lastInviteOK != nil {
		return false, s.sendSIP(dialog.lastInviteOK, source, true)
	}
	if sip.HeaderParameter(message.Get("From"), "tag") != dialog.remoteTag || sip.HeaderParameter(message.Get("To"), "tag") != dialog.localTag {
		return false, s.sendSIP(responseFor(message, 481, "", nil), source, false)
	}
	cseq, err := message.CSeq()
	if err != nil || cseq.Method != method {
		return false, s.sendSIP(responseFor(message, 400, "", nil), source, false)
	}
	duplicate := cseq.Number <= dialog.remoteCSeq
	if !duplicate {
		dialog.remoteCSeq = cseq.Number
	}
	switch method {
	case "BYE":
		if err := s.sendSIP(responseFor(message, 200, "", nil), source, duplicate); err != nil {
			return false, err
		}
		if !duplicate {
			s.setOutcome(func(outcome *SIPOutcome) { outcome.TerminatedBy = SystemUnderTest })
			return true, nil
		}
	case "INFO":
		digit, duration, status := parseDTMFInfo(message)
		if err := s.sendSIP(responseFor(message, status, "", nil), source, duplicate); err != nil {
			return false, err
		}
		if status == 200 && !duplicate {
			s.stateMu.Lock()
			s.infoDTMF = append(s.infoDTMF, dtmfRecord{event: DTMFEvent{Digit: digit, Duration: duration}, method: SIPInfo, at: time.Now()})
			s.stateMu.Unlock()
		}
	default:
		return false, s.sendSIP(responseFor(message, 501, "", nil), source, false)
	}
	return false, nil
}

func (s *Session) terminateForkedDialog(response *sip.Message, source netip.AddrPort, original *callDialog) error {
	if original.invite == nil {
		return fmt.Errorf("%w: forked dialog is outside V1", ErrProtocol)
	}
	remoteTarget := original.invite.URI
	if contact, err := sip.ParseAddress(response.Get("Contact")); err == nil {
		remoteTarget = contact.RawURI
	}
	fork := &callDialog{
		callID: original.callID, localTag: original.localTag, remoteTag: sip.HeaderParameter(response.Get("To"), "tag"),
		localAddress: original.localAddress, remoteAddress: response.Get("To"), remoteTarget: remoteTarget,
		peer: sip.RequestTarget(response, source), inviteCSeq: original.inviteCSeq, localCSeq: original.inviteCSeq,
		inviteBranch: original.inviteBranch, routeSet: routeSet(response.Values("Record-Route"), true),
	}
	ack := s.makeInviteACK(original.invite, response, remoteTarget, s.carrier.ids.Branch(), true)
	target, routeErr := routeDialogRequest(ack, fork)
	var ackErr, byeErr error
	if routeErr == nil {
		ackErr = s.sendSIP(ack, target, false)
		if ackErr == nil && fork.remoteTag != "" {
			byeErr = s.sendDialogBYEOnce(fork)
		}
	}
	return errors.Join(fmt.Errorf("%w: forked dialog is outside V1", ErrProtocol), routeErr, ackErr, byeErr)
}

func routeSet(values []string, reverse bool) []string {
	routes := append([]string(nil), values...)
	if reverse {
		for left, right := 0, len(routes)-1; left < right; left, right = left+1, right-1 {
			routes[left], routes[right] = routes[right], routes[left]
		}
	}
	return routes
}

func routeDialogRequest(request *sip.Message, dialog *callDialog) (netip.AddrPort, error) {
	if len(dialog.routeSet) == 0 {
		return dialog.peer, nil
	}
	first, err := sip.ParseAddress(dialog.routeSet[0])
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%w: invalid dialog route: %v", ErrProtocol, err)
	}
	address, err := netip.ParseAddr(first.URI.Host)
	if err != nil || !address.Is4() {
		return netip.AddrPort{}, fmt.Errorf("%w: dialog route is not IPv4", ErrProtocol)
	}
	target := netip.AddrPortFrom(address, first.URI.Port)
	if _, loose := first.URI.Parameters["lr"]; loose {
		for _, route := range dialog.routeSet {
			request.Add("Route", route)
		}
		return target, nil
	}
	request.URI = first.RawURI
	for _, route := range dialog.routeSet[1:] {
		request.Add("Route", route)
	}
	request.Add("Route", "<"+dialog.remoteTarget+">")
	return target, nil
}
