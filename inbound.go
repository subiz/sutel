package sutel

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	mediaaudio "github.com/subiz/sutel/audio"
	"github.com/subiz/sutel/internal/sip"
)

func (s *Session) runInbound() error {
	scenario := s.inbound
	payload := scenario.Codec.PayloadType()
	body, err := sip.GenerateSDP(
		s.receiver.Addr().Addr(), s.receiver.Addr().Port(), []uint8{payload},
		map[uint8]sip.RTPMap{payload: {Encoding: scenario.Codec.String(), ClockRate: 8000}},
		101, sip.SendRecv,
	)
	if err != nil {
		return fmt.Errorf("%w: generate offer: %v", ErrNegotiation, err)
	}
	localTag := s.carrier.ids.Tag()
	callID := s.carrier.ids.CallID()
	branch := s.carrier.ids.Branch()
	requestURI := sipURI(scenario.To, scenario.TargetSIPAddr)
	contact := localContact(s.sipAddr, scenario.From)
	localAddress := fmt.Sprintf("<%s>;tag=%s", sipURI(scenario.From, s.sipAddr), localTag)
	if scenario.FromDisplayName != "" {
		displayName := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(scenario.FromDisplayName)
		localAddress = fmt.Sprintf(`"%s" <%s>;tag=%s`, displayName, sipURI(scenario.From, s.sipAddr), localTag)
	}
	remoteAddress := fmt.Sprintf("<%s>", sipURI(scenario.To, scenario.TargetSIPAddr))
	invite := sip.NewRequest("INVITE", requestURI)
	appendRequestHeaders(invite, branch, localAddress, remoteAddress, callID, 1, contact)
	invite.Add("Content-Type", "application/sdp")
	invite.Body = body

	final, peer, err := s.runInviteClientTransaction(invite, scenario.RingTimeout, nil)
	if err != nil {
		return err
	}
	if final.StatusCode == 401 && scenario.DigestCredentials != nil {
		staleACK := s.makeInviteACK(invite, final, requestURI, branch, false)
		if err := s.sendSIP(staleACK, peer, false); err != nil {
			return err
		}
		authenticated := invite.Clone()
		branch = s.carrier.ids.Branch()
		authenticated.Set("Via", fmt.Sprintf("SIP/2.0/UDP %s;branch=%s;rport", requestLocalHost(contact), branch))
		authenticated.Set("CSeq", "2 INVITE")
		authorization, err := digestAuthorization(
			final.Values("WWW-Authenticate"), scenario.DigestCredentials.Username,
			scenario.DigestCredentials.Password, authenticated.Method, authenticated.URI,
			s.carrier.ids.ID("cnonce-"),
		)
		if err != nil {
			return fmt.Errorf("%w: digest challenge: %v", ErrProtocol, err)
		}
		authenticated.Set("Authorization", authorization)
		// The retried transaction may still observe retransmissions of the 401;
		// staleACK re-acknowledges them so the challenge transaction completes.
		final, peer, err = s.runInviteClientTransaction(authenticated, scenario.RingTimeout, staleACK)
		if err != nil {
			return err
		}
		invite = authenticated
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = final.StatusCode })
	expectedStatus := scenario.ExpectStatus
	if expectedStatus == 0 {
		expectedStatus = 200
	}
	var statusErr error
	if final.StatusCode != expectedStatus {
		statusErr = fmt.Errorf("%w: inbound final status got %d, want %d", ErrVerification, final.StatusCode, expectedStatus)
	}
	if final.StatusCode < 200 || final.StatusCode >= 300 {
		ack := s.makeInviteACK(invite, final, requestURI, branch, false)
		if err := s.sendSIP(ack, peer, false); err != nil {
			return err
		}
		absorbErr := s.absorbNon2xxFinal(invite, ack)
		return errors.Join(statusErr, absorbErr)
	}
	remoteTag := sip.HeaderParameter(final.Get("To"), "tag")
	contactAddress, contactErr := sip.ParseAddress(final.Get("Contact"))
	remoteTarget := requestURI
	if contactErr == nil {
		remoteTarget = contactAddress.RawURI
	}
	dialog := &callDialog{
		callID: callID, localTag: localTag, remoteTag: remoteTag,
		localAddress: localAddress, remoteAddress: final.Get("To"), remoteTarget: remoteTarget,
		peer: sip.RequestTarget(final, peer), inviteCSeq: mustCSeq(invite), remoteCSeq: 0, localCSeq: mustCSeq(invite), inviteBranch: branch,
		invite:   invite.Clone(),
		routeSet: routeSet(final.Values("Record-Route"), true),
	}
	ack := s.makeInviteACK(invite, final, remoteTarget, s.carrier.ids.Branch(), true)
	dialog.inviteACK = ack
	ackTarget, err := routeDialogRequest(ack, dialog)
	if err != nil {
		return err
	}
	dialog.inviteACKTarget = ackTarget
	if err := s.sendSIP(ack, ackTarget, false); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.Established = true })
	if remoteTag == "" || contactErr != nil {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: 200 response requires To tag and Contact", ErrProtocol))
	}
	if statusErr != nil {
		return s.failEstablishedInbound(dialog, statusErr)
	}
	if !strings.EqualFold(strings.TrimSpace(strings.Split(final.Get("Content-Type"), ";")[0]), "application/sdp") || len(final.Body) == 0 {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: 200 response requires SDP", ErrNegotiation))
	}
	answer, err := sip.ParseSDP(final.Body)
	if err != nil {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: invalid SDP answer: %v", ErrNegotiation, err))
	}
	codec, payload, ok := chooseCodec(answer, []Codec{scenario.Codec})
	if !ok || codec != scenario.Codec {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: answer did not accept %s", ErrNegotiation, scenario.Codec))
	}
	dialog.codec, dialog.payload, dialog.remoteSDP = codec, payload, answer
	dialog.telephoneEvent = -1
	if answer.TelephoneEvent >= 0 {
		mapping := answer.RTPMaps[uint8(answer.TelephoneEvent)]
		if strings.EqualFold(mapping.Encoding, "telephone-event") && mapping.ClockRate == 8000 {
			dialog.telephoneEvent = answer.TelephoneEvent
		}
	}
	if (scenario.Playback != nil || hasRFC4733(scenario.DTMF)) && !answer.CanSend() {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: remote SDP direction does not receive media", ErrNegotiation))
	}
	if scenario.ExpectAudio != nil && !answer.CanReceive() {
		return s.failEstablishedInbound(dialog, fmt.Errorf("%w: remote SDP direction does not send expected media", ErrNegotiation))
	}
	if err := s.configureMedia(dialog); err != nil {
		return s.failEstablishedInbound(dialog, err)
	}
	establishedAt := s.carrier.clock.Now()
	mediaCtx, cancelMedia := context.WithCancel(s.ctx)
	mediaDone := make(chan error, 1)
	go func() { mediaDone <- s.playInboundMedia(mediaCtx, dialog) }()

	terminated, infoErr := s.sendInboundINFO(dialog)
	if infoErr != nil {
		cancelMedia()
		mediaErr := <-mediaDone
		if errors.Is(mediaErr, context.Canceled) {
			mediaErr = nil
		}
		byeErr := s.sendDialogBYEOnce(dialog)
		return errors.Join(infoErr, mediaErr, byeErr)
	}
	if terminated {
		cancelMedia()
		mediaErr := <-mediaDone
		if errors.Is(mediaErr, context.Canceled) {
			mediaErr = nil
		}
		return mediaErr
	}
	hangupAfter := time.Duration(0)
	if scenario.CallDuration > 0 {
		hangupAfter = max(0, establishedAt.Add(scenario.CallDuration).Sub(s.carrier.clock.Now()))
	}
	termination, dialogErr := dialogNeedsLocalBYE, error(nil)
	if scenario.CallDuration == 0 || hangupAfter > 0 {
		termination, dialogErr = s.runEstablishedDialog(dialog, hangupAfter, nil)
	}
	cancelMedia()
	mediaErr := <-mediaDone
	if errors.Is(mediaErr, context.Canceled) {
		mediaErr = nil
	}
	var byeErr error
	if dialogErr != nil {
		byeErr = s.sendDialogBYEOnce(dialog)
	} else if termination == dialogNeedsLocalBYE {
		byeErr = s.finishLocalDialog(dialog)
	}
	return errors.Join(dialogErr, mediaErr, byeErr)
}

// digestAuthorization answers the first supported challenge among the
// WWW-Authenticate header values: Digest with MD5 (RFC 2617) or SHA-256
// (RFC 7616), qop absent or auth. A server may offer several challenges with
// different algorithms; unsupported ones are skipped.
func digestAuthorization(challenges []string, username, password, method, uri, cnonce string) (string, error) {
	lastErr := fmt.Errorf("missing Digest challenge")
	for _, challenge := range challenges {
		authorization, err := answerDigestChallenge(challenge, username, password, method, uri, cnonce)
		if err == nil {
			return authorization, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func answerDigestChallenge(challenge, username, password, method, uri, cnonce string) (string, error) {
	parameters, err := parseDigestChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm, nonce := parameters["realm"], parameters["nonce"]
	if realm == "" || nonce == "" {
		return "", fmt.Errorf("missing realm or nonce")
	}
	algorithm, hash := "MD5", digestMD5
	switch offered := parameters["algorithm"]; {
	case offered == "" || strings.EqualFold(offered, "MD5"):
	case strings.EqualFold(offered, "SHA-256"):
		algorithm, hash = "SHA-256", digestSHA256
	default:
		return "", fmt.Errorf("unsupported algorithm %q", parameters["algorithm"])
	}
	ha1 := hash(username + ":" + realm + ":" + password)
	ha2 := hash(method + ":" + uri)
	qop := ""
	if offered := parameters["qop"]; offered != "" {
		for _, candidate := range strings.Split(offered, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), "auth") {
				qop = "auth"
				break
			}
		}
		if qop == "" {
			return "", fmt.Errorf("qop auth is not offered")
		}
	}
	nc := "00000001"
	responseInput := ha1 + ":" + nonce + ":" + ha2
	if qop != "" {
		responseInput = ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2
	}
	parts := []string{
		`Digest username="` + quoteDigestValue(username) + `"`,
		`realm="` + quoteDigestValue(realm) + `"`,
		`nonce="` + quoteDigestValue(nonce) + `"`,
		`uri="` + quoteDigestValue(uri) + `"`,
		`response="` + hash(responseInput) + `"`,
		"algorithm=" + algorithm,
	}
	if opaque := parameters["opaque"]; opaque != "" {
		parts = append(parts, `opaque="`+quoteDigestValue(opaque)+`"`)
	}
	if qop != "" {
		parts = append(parts, "qop=auth", "nc="+nc, `cnonce="`+quoteDigestValue(cnonce)+`"`)
	}
	return strings.Join(parts, ", "), nil
}

func digestMD5(value string) string {
	sum := md5.Sum([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func digestSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum)
}

func quoteDigestValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

func parseDigestChallenge(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	schemeEnd := strings.IndexAny(value, " \t")
	if schemeEnd < 0 || !strings.EqualFold(value[:schemeEnd], "Digest") {
		return nil, fmt.Errorf("unsupported challenge %q", value)
	}
	rest := strings.TrimSpace(value[schemeEnd:])
	parameters := map[string]string{}
	for _, field := range splitDigestFields(rest) {
		name, raw, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("malformed digest parameter %q", field)
		}
		name, raw = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(raw)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = unescapeDigestValue(raw[1 : len(raw)-1])
		}
		if _, duplicate := parameters[name]; duplicate {
			return nil, fmt.Errorf("duplicate digest parameter %q", name)
		}
		parameters[name] = raw
	}
	return parameters, nil
}

func unescapeDigestValue(value string) string {
	var result strings.Builder
	escaped := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if !escaped && character == '\\' {
			escaped = true
			continue
		}
		escaped = false
		result.WriteByte(character)
	}
	return result.String()
}

func splitDigestFields(value string) []string {
	var fields []string
	start, quoted, escaped := 0, false, false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			escaped = false
			continue
		}
		if quoted && character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			quoted = !quoted
			continue
		}
		if character == ',' && !quoted {
			if field := strings.TrimSpace(value[start:index]); field != "" {
				fields = append(fields, field)
			}
			start = index + 1
		}
	}
	if field := strings.TrimSpace(value[start:]); field != "" {
		fields = append(fields, field)
	}
	return fields
}

func (s *Session) failEstablishedInbound(dialog *callDialog, cause error) error {
	byeErr := s.sendDialogBYEOnce(dialog)
	if byeErr == nil {
		s.setOutcome(func(outcome *SIPOutcome) { outcome.TerminatedBy = Sutel })
	}
	return errors.Join(cause, byeErr)
}

func (s *Session) absorbNon2xxFinal(invite, ack *sip.Message) error {
	duration := min(2*s.carrier.config.SIPT1, s.carrier.config.SIPT2)
	timer := s.carrier.clock.NewTimer(duration)
	defer stopTransactionTimer(timer)
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, nil, timerChannel(timer), nil)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if timeout == deadlineSIPReadTimeout {
			return nil
		}
		if !message.IsRequest() && message.StatusCode >= 300 && isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "INVITE") {
			if err := s.sendSIP(ack, source, true); err != nil {
				return err
			}
			continue
		}
		if message.IsRequest() && (strings.EqualFold(message.Method, "BYE") || strings.EqualFold(message.Method, "INFO") || strings.EqualFold(message.Method, "CANCEL")) {
			if err := s.sendSIP(responseFor(message, 481, "", nil), source, false); err != nil {
				return err
			}
			continue
		}
		s.ignoreMessage()
	}
}

func (s *Session) runInviteClientTransaction(invite *sip.Message, ringTimeout time.Duration, staleACK *sip.Message) (*sip.Message, netip.AddrPort, error) {
	if err := s.sendSIP(invite, s.inbound.TargetSIPAddr, false); err != nil {
		return nil, netip.AddrPort{}, err
	}
	callID := invite.Get("Call-ID")
	cseq := mustCSeq(invite)
	interval := s.carrier.config.SIPT1
	deadlineTimer := s.carrier.clock.NewTimer(ringTimeout)
	defer stopTransactionTimer(deadlineTimer)
	retransmitTimer := s.carrier.clock.NewTimer(interval)
	defer func() { stopTransactionTimer(retransmitTimer) }()
	provisional := false
	for {
		message, peer, timeout, err := s.readSIPWithTimers(s.ctx, timerChannel(retransmitTimer), timerChannel(deadlineTimer), nil)
		if err != nil {
			return nil, netip.AddrPort{}, err
		}
		if timeout == deadlineSIPReadTimeout {
			if provisional {
				return nil, netip.AddrPort{}, errors.Join(context.DeadlineExceeded, s.cancelTimedOutInvite(invite))
			}
			return nil, netip.AddrPort{}, context.DeadlineExceeded
		}
		if timeout == retransmitSIPReadTimeout {
			if err := s.sendSIP(invite, s.inbound.TargetSIPAddr, true); err != nil {
				return nil, netip.AddrPort{}, err
			}
			stopTransactionTimer(retransmitTimer)
			interval = min(interval*2, s.carrier.config.SIPT2)
			retransmitTimer = s.carrier.clock.NewTimer(interval)
			continue
		}
		if message.IsRequest() {
			s.ignoreMessage()
			continue
		}
		// Khi retry INVITE sau 401, transaction cũ có thể retransmit challenge
		// trước khi nhận ACK. ACK lại đúng CSeq/branch cũ và tiếp tục chờ response
		// của INVITE authenticated thay vì coi 401 cũ là response hiện tại.
		if staleACK != nil && message.StatusCode == 401 &&
			isMatching(message, callID, mustCSeq(staleACK), "INVITE") {
			if err := s.sendSIP(staleACK, peer, true); err != nil {
				return nil, netip.AddrPort{}, err
			}
			continue
		}
		if !isMatching(message, callID, cseq, "INVITE") {
			s.ignoreMessage()
			continue
		}
		if message.StatusCode < 200 {
			if !provisional {
				provisional = true
				stopTransactionTimer(retransmitTimer)
				retransmitTimer = nil
			}
			continue
		}
		return message, peer, nil
	}
}

func (s *Session) cancelTimedOutInvite(invite *sip.Message) error {
	cancel := sip.NewRequest("CANCEL", invite.URI)
	appendRequestHeaders(cancel, sip.ViaBranch(invite.Get("Via")), invite.Get("From"), invite.Get("To"), invite.Get("Call-ID"), mustCSeq(invite), "")
	if err := s.sendSIP(cancel, s.inbound.TargetSIPAddr, false); err != nil {
		return err
	}
	timer := s.carrier.clock.NewTimer(min(2*s.carrier.config.SIPT1, s.carrier.config.SIPT2))
	defer stopTransactionTimer(timer)
	for {
		message, peer, timeout, err := s.readSIPWithTimers(s.ctx, nil, timerChannel(timer), nil)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if timeout == deadlineSIPReadTimeout {
			return nil
		}
		if !message.IsRequest() && isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "CANCEL") {
			continue
		}
		if !message.IsRequest() && message.StatusCode >= 200 && isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "INVITE") {
			if message.StatusCode >= 200 && message.StatusCode < 300 {
				return s.cleanupLateSuccessfulInvite(invite, message, peer)
			}
			ack := s.makeInviteACK(invite, message, invite.URI, sip.ViaBranch(invite.Get("Via")), false)
			return s.sendSIP(ack, peer, false)
		}
		s.ignoreMessage()
	}
}

func (s *Session) cleanupLateSuccessfulInvite(invite, response *sip.Message, peer netip.AddrPort) error {
	s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = response.StatusCode })
	contact, contactErr := sip.ParseAddress(response.Get("Contact"))
	remoteTarget := invite.URI
	if contactErr == nil {
		remoteTarget = contact.RawURI
	}
	dialog := &callDialog{
		callID: invite.Get("Call-ID"), localTag: sip.HeaderParameter(invite.Get("From"), "tag"), remoteTag: sip.HeaderParameter(response.Get("To"), "tag"),
		localAddress: invite.Get("From"), remoteAddress: response.Get("To"), remoteTarget: remoteTarget,
		peer: sip.RequestTarget(response, peer), inviteCSeq: mustCSeq(invite), localCSeq: mustCSeq(invite),
		inviteBranch: sip.ViaBranch(invite.Get("Via")), routeSet: routeSet(response.Values("Record-Route"), true),
	}
	ack := s.makeInviteACK(invite, response, remoteTarget, s.carrier.ids.Branch(), true)
	target, routeErr := routeDialogRequest(ack, dialog)
	var ackErr, byeErr error
	if routeErr == nil {
		ackErr = s.sendSIP(ack, target, false)
		// Contact is mandatory in a successful INVITE response, but cleanup is
		// deliberately best-effort: fall back to the original request URI when
		// the peer omitted it so an established dialog is not silently orphaned.
		if ackErr == nil && dialog.remoteTag != "" {
			byeErr = s.sendDialogBYEOnce(dialog)
		}
	}
	if routeErr == nil && ackErr == nil {
		s.setOutcome(func(outcome *SIPOutcome) { outcome.Established = true })
	}
	if routeErr == nil && ackErr == nil && byeErr == nil && dialog.remoteTag != "" {
		s.setOutcome(func(outcome *SIPOutcome) { outcome.TerminatedBy = Sutel })
	}
	return errors.Join(routeErr, ackErr, byeErr)
}

func (s *Session) makeInviteACK(invite, response *sip.Message, requestURI, branch string, success bool) *sip.Message {
	ack := sip.NewRequest("ACK", requestURI)
	contact := invite.Get("Contact")
	appendRequestHeaders(ack, branch, invite.Get("From"), response.Get("To"), invite.Get("Call-ID"), mustCSeq(invite), contact)
	if !success {
		ack.Del("Contact")
	}
	return ack
}

func (s *Session) playInboundMedia(ctx context.Context, dialog *callDialog) error {
	if !dialog.remoteSDP.CanSend() && (s.inbound.Playback != nil || hasRFC4733(s.inbound.DTMF)) {
		return fmt.Errorf("%w: remote SDP direction does not receive media", ErrNegotiation)
	}
	if s.inbound.Playback != nil {
		samples, err := mediaaudio.Normalize8kMono(s.inbound.Playback.File)
		if err != nil {
			return err
		}
		if err := s.sender.Play(ctx, dialog.remoteMedia, toRTPCodec(dialog.codec), dialog.payload, samples); err != nil {
			return err
		}
	}
	for _, action := range s.inbound.DTMF {
		if action.Method != RFC4733 {
			continue
		}
		if dialog.telephoneEvent < 0 {
			return fmt.Errorf("%w: telephone-event was not negotiated", ErrNegotiation)
		}
		if err := s.sender.SendDTMF(ctx, dialog.remoteMedia, uint8(dialog.telephoneEvent), action.Digits, action.Interval); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) sendInboundINFO(dialog *callDialog) (bool, error) {
	for _, action := range s.inbound.DTMF {
		if action.Method != SIPInfo {
			continue
		}
		runes := []rune(strings.ToUpper(action.Digits))
		for index, digit := range runes {
			terminated, err := s.sendDialogINFO(dialog, string(digit))
			if err != nil || terminated {
				return terminated, err
			}
			if index < len(runes)-1 && action.Interval > 0 {
				terminated, err := s.waitDialogDuration(dialog, action.Interval)
				if err != nil || terminated {
					return terminated, err
				}
			}
		}
	}
	return false, nil
}

func hasRFC4733(actions []DTMFAction) bool {
	for _, action := range actions {
		if action.Method == RFC4733 {
			return true
		}
	}
	return false
}
