package sutel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	mediaaudio "github.com/subiz/sutel/audio"
	"github.com/subiz/sutel/internal/sip"
	"github.com/subiz/sutel/rtpverifier"
)

func (s *Session) runOutbound() error {
	invite, peer, offered, dialog, err := s.waitForInvite()
	if err != nil {
		return err
	}
	receivedAt := s.carrier.clock.Now()
	if _, timeout := s.outbound.Behavior.(Timeout); timeout {
		return s.runSilentTimeout(invite)
	}
	switch s.outbound.Behavior.(type) {
	case Busy:
		return s.finishNegativeInvite(invite, peer, dialog.localTag, 486)
	case Reject:
		return s.finishNegativeInvite(invite, peer, dialog.localTag, 603)
	case NotFound:
		return s.finishNegativeInvite(invite, peer, dialog.localTag, 404)
	case ServiceUnavailable:
		return s.finishNegativeInvite(invite, peer, dialog.localTag, 503)
	case NoAnswer:
		return s.runNoAnswer(invite, peer, dialog, receivedAt)
	}
	codec, payload, common := chooseCodec(offered, s.outbound.Codecs)
	if !common {
		response := responseFor(invite, 488, dialog.localTag, nil)
		if err := s.sendSIP(response, peer, false); err != nil {
			return err
		}
		s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = 488 })
		ackErr := s.waitForInviteACK(invite, response, peer)
		return errors.Join(fmt.Errorf("%w: no common codec", ErrNegotiation), ackErr)
	}
	dialog.codec, dialog.payload, dialog.remoteSDP = codec, payload, offered
	dialog.telephoneEvent = -1
	if offered.TelephoneEvent >= 0 {
		mapping := offered.RTPMaps[uint8(offered.TelephoneEvent)]
		if strings.EqualFold(mapping.Encoding, "telephone-event") && mapping.ClockRate == 8000 {
			dialog.telephoneEvent = offered.TelephoneEvent
		}
	}
	needsSend, needsReceive := false, s.outbound.ExpectAudio != nil
	switch behavior := s.outbound.Behavior.(type) {
	case Answer:
		needsSend = behavior.Playback != nil || behavior.Echo != nil
		needsReceive = needsReceive || behavior.Echo != nil
	case EarlyMedia, EarlyFailure:
		needsSend = true
	}
	if (needsSend && !offered.CanSend()) || (needsReceive && !offered.CanReceive()) {
		response := responseFor(invite, 488, dialog.localTag, nil)
		if err := s.sendSIP(response, peer, false); err != nil {
			return err
		}
		s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = 488 })
		ackErr := s.waitForInviteACK(invite, response, peer)
		return errors.Join(fmt.Errorf("%w: remote SDP direction is incompatible with declared media", ErrNegotiation), ackErr)
	}
	if err := s.configureMedia(dialog); err != nil {
		response := responseFor(invite, 488, dialog.localTag, nil)
		_ = s.sendSIP(response, peer, false)
		s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = 488 })
		_ = s.waitForInviteACK(invite, response, peer)
		return err
	}

	switch behavior := s.outbound.Behavior.(type) {
	case Answer:
		return s.runOutboundAnswer(invite, peer, dialog, receivedAt, behavior)
	case EarlyMedia:
		return s.runOutboundEarlyMedia(invite, peer, dialog, receivedAt, behavior)
	case EarlyFailure:
		return s.runOutboundEarlyFailure(invite, peer, dialog, receivedAt, behavior)
	case NetworkLoss:
		return s.runOutboundNetworkLoss(invite, peer, dialog, behavior)
	default:
		return fmt.Errorf("%w: unsupported outbound behavior %T", ErrProtocol, behavior)
	}
}

func (s *Session) waitForInvite() (*sip.Message, netip.AddrPort, sip.SDP, *callDialog, error) {
	for {
		message, peer, err := s.readSIP(s.ctx)
		if err != nil {
			return nil, netip.AddrPort{}, sip.SDP{}, nil, err
		}
		if !message.IsRequest() || !strings.EqualFold(message.Method, "INVITE") {
			if message.IsRequest() && (strings.EqualFold(message.Method, "INFO") || strings.EqualFold(message.Method, "BYE")) {
				_ = s.sendSIP(responseFor(message, 481, "", nil), peer, false)
			} else {
				s.ignoreMessage()
			}
			continue
		}
		offered, dialog, status, validationErr := s.validateInvite(message, peer)
		if validationErr != nil {
			response := responseFor(message, status, s.carrier.ids.Tag(), nil)
			sendErr := s.sendSIP(response, peer, false)
			var ackErr error
			if sendErr == nil && validInviteServerTransaction(message) {
				ackErr = s.waitForInviteACK(message, response, peer)
			}
			return nil, netip.AddrPort{}, sip.SDP{}, nil, errors.Join(validationErr, sendErr, ackErr)
		}
		return message, peer, offered, dialog, nil
	}
}

func validInviteServerTransaction(message *sip.Message) bool {
	if message == nil || !message.IsRequest() || !strings.EqualFold(message.Method, "INVITE") || strings.TrimSpace(message.Get("Call-ID")) == "" {
		return false
	}
	cseq, err := message.CSeq()
	return err == nil && cseq.Method == "INVITE" && strings.HasPrefix(sip.ViaBranch(message.Get("Via")), "z9hG4bK")
}

func (s *Session) validateInvite(message *sip.Message, peer netip.AddrPort) (sip.SDP, *callDialog, int, error) {
	requestURI, err := sip.ParseURI(message.URI)
	if err != nil {
		return sip.SDP{}, nil, 400, fmt.Errorf("%w: invalid Request-URI", ErrProtocol)
	}
	if requestURI.Host != s.sipAddr.Addr().String() || requestURI.Port != s.sipAddr.Port() {
		return sip.SDP{}, nil, 404, fmt.Errorf("%w: Request-URI does not target this carrier", ErrVerification)
	}
	from, err := sip.ParseAddress(message.Get("From"))
	if err != nil || from.Tag == "" {
		return sip.SDP{}, nil, 400, fmt.Errorf("%w: invalid From", ErrProtocol)
	}
	to, err := sip.ParseAddress(message.Get("To"))
	if err != nil {
		return sip.SDP{}, nil, 400, fmt.Errorf("%w: invalid To", ErrProtocol)
	}
	if expected := s.outbound.From; expected != "" && from.URI.User != expected {
		return sip.SDP{}, nil, 404, fmt.Errorf("%w: From number got %q, want %q", ErrVerification, from.URI.User, expected)
	}
	if expected := s.outbound.To; expected != "" && to.URI.User != expected {
		return sip.SDP{}, nil, 404, fmt.Errorf("%w: To number got %q, want %q", ErrVerification, to.URI.User, expected)
	}
	if expected := s.outbound.To; expected != "" && requestURI.User != expected {
		return sip.SDP{}, nil, 404, fmt.Errorf("%w: Request-URI number got %q, want %q", ErrVerification, requestURI.User, expected)
	}
	contact, err := sip.ParseAddress(message.Get("Contact"))
	if err != nil {
		return sip.SDP{}, nil, 400, fmt.Errorf("%w: invalid Contact", ErrProtocol)
	}
	callID := strings.TrimSpace(message.Get("Call-ID"))
	cseq, err := message.CSeq()
	branch := sip.ViaBranch(message.Get("Via"))
	if callID == "" || err != nil || cseq.Method != "INVITE" || !strings.HasPrefix(branch, "z9hG4bK") {
		return sip.SDP{}, nil, 400, fmt.Errorf("%w: invalid INVITE transaction headers", ErrProtocol)
	}
	if !strings.EqualFold(strings.TrimSpace(strings.Split(message.Get("Content-Type"), ";")[0]), "application/sdp") || len(message.Body) == 0 {
		return sip.SDP{}, nil, 415, fmt.Errorf("%w: INVITE requires application/sdp", ErrNegotiation)
	}
	offered, err := sip.ParseSDP(message.Body)
	if err != nil {
		return sip.SDP{}, nil, 488, fmt.Errorf("%w: %v", ErrNegotiation, err)
	}
	localTag := s.carrier.ids.Tag()
	localAddress := message.Get("To") + ";tag=" + localTag
	return offered, &callDialog{
		callID: callID, localTag: localTag, remoteTag: from.Tag,
		localAddress: localAddress, remoteAddress: message.Get("From"),
		remoteTarget: contact.RawURI, peer: sip.RequestTarget(message, peer),
		inviteCSeq: cseq.Number, remoteCSeq: cseq.Number, localCSeq: cseq.Number, inviteBranch: branch,
		routeSet: routeSet(message.Values("Record-Route"), false),
	}, 200, nil
}

func (s *Session) finishNegativeInvite(invite *sip.Message, peer netip.AddrPort, localTag string, status int) error {
	response := responseFor(invite, status, localTag, nil)
	if err := s.sendSIP(response, peer, false); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = status })
	return s.waitForInviteACK(invite, response, peer)
}

func (s *Session) runSilentTimeout(invite *sip.Message) error {
	for {
		message, _, err := s.readSIP(s.ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if !message.IsRequest() || !isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "INVITE") {
			s.ignoreMessage()
		}
	}
}

func (s *Session) runNoAnswer(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, started time.Time) error {
	trying := responseFor(invite, 100, "", nil)
	if err := s.sendSIP(trying, peer, false); err != nil {
		return err
	}
	ringing := responseFor(invite, 180, dialog.localTag, nil)
	ringing.Add("Contact", localContact(s.sipAddr, "sutel"))
	if err := s.sendSIP(ringing, peer, false); err != nil {
		return err
	}
	for {
		message, source, err := s.readSIP(s.ctx)
		if err != nil {
			return err
		}
		if message.IsRequest() && strings.EqualFold(message.Method, "INVITE") && isMatching(message, dialog.callID, dialog.inviteCSeq, "INVITE") && sip.ViaBranch(message.Get("Via")) == dialog.inviteBranch {
			if err := s.sendSIP(ringing, source, true); err != nil {
				return err
			}
			continue
		}
		if !message.IsRequest() || !strings.EqualFold(message.Method, "CANCEL") {
			s.ignoreMessage()
			continue
		}
		if !validInviteCANCEL(message, invite, dialog) {
			if err := s.sendSIP(responseFor(message, 481, "", nil), source, false); err != nil {
				return err
			}
			continue
		}
		return s.finishCanceledInvite(invite, peer, dialog, message, source)
	}
}

func validInviteCANCEL(cancel, invite *sip.Message, dialog *callDialog) bool {
	return cancel != nil && cancel.IsRequest() && strings.EqualFold(cancel.Method, "CANCEL") &&
		isMatching(cancel, dialog.callID, dialog.inviteCSeq, "CANCEL") &&
		sip.ViaBranch(cancel.Get("Via")) == dialog.inviteBranch && cancel.URI == invite.URI &&
		cancel.Get("From") == invite.Get("From") && cancel.Get("To") == invite.Get("To")
}

func (s *Session) finishCanceledInvite(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, cancel *sip.Message, source netip.AddrPort) error {
	if err := s.sendSIP(responseFor(cancel, 200, dialog.localTag, nil), source, false); err != nil {
		return err
	}
	terminated := responseFor(invite, 487, dialog.localTag, nil)
	if err := s.sendSIP(terminated, peer, false); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) {
		outcome.InviteFinalStatus = 487
		outcome.Canceled = true
	})
	return s.waitForInviteACK(invite, terminated, peer)
}

func (s *Session) waitForBehaviorDeadline(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, started time.Time, after time.Duration, latest *sip.Message) (bool, error) {
	wait := started.Add(after).Sub(s.carrier.clock.Now())
	if wait <= 0 {
		return false, nil
	}
	timer := s.carrier.clock.NewTimer(wait)
	defer stopTransactionTimer(timer)
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, nil, timerChannel(timer), nil)
		if err != nil {
			return false, err
		}
		if timeout == deadlineSIPReadTimeout {
			return false, nil
		}
		if message.IsRequest() && strings.EqualFold(message.Method, "INVITE") && isMatching(message, dialog.callID, dialog.inviteCSeq, "INVITE") && sip.ViaBranch(message.Get("Via")) == dialog.inviteBranch {
			if latest != nil {
				if err := s.sendSIP(latest, source, true); err != nil {
					return false, err
				}
			} else {
				s.ignoreMessage()
			}
			continue
		}
		if message.IsRequest() && strings.EqualFold(message.Method, "CANCEL") {
			if !validInviteCANCEL(message, invite, dialog) {
				if err := s.sendSIP(responseFor(message, 481, "", nil), source, false); err != nil {
					return false, err
				}
				continue
			}
			return true, s.finishCanceledInvite(invite, peer, dialog, message, source)
		}
		if message.IsRequest() && (strings.EqualFold(message.Method, "BYE") || strings.EqualFold(message.Method, "INFO")) {
			if err := s.sendSIP(responseFor(message, 481, "", nil), source, false); err != nil {
				return false, err
			}
			continue
		}
		s.ignoreMessage()
	}
}

func (s *Session) runOutboundAnswer(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, started time.Time, behavior Answer) error {
	var latest *sip.Message
	if !behavior.OmitTrying {
		canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.TryingAfter, latest)
		if err != nil || canceled {
			return err
		}
		latest = responseFor(invite, 100, "", nil)
		if err := s.sendSIP(latest, peer, false); err != nil {
			return err
		}
	}
	canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.RingingAfter, latest)
	if err != nil || canceled {
		return err
	}
	ringing := responseFor(invite, 180, dialog.localTag, nil)
	ringing.Add("Contact", localContact(s.sipAddr, "sutel"))
	if err := s.sendSIP(ringing, peer, false); err != nil {
		return err
	}
	latest = ringing
	canceled, err = s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.AnswerAfter, latest)
	if err != nil || canceled {
		return err
	}
	return s.answerAndRunDialog(invite, peer, dialog, s.remainingUntil(started, behavior.HangupAfter), &behavior)
}

func (s *Session) runOutboundNetworkLoss(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, behavior NetworkLoss) error {
	if err := s.sendSIP(responseFor(invite, 100, "", nil), peer, false); err != nil {
		return err
	}
	ringing := responseFor(invite, 180, dialog.localTag, nil)
	ringing.Add("Contact", localContact(s.sipAddr, "sutel"))
	if err := s.sendSIP(ringing, peer, false); err != nil {
		return err
	}
	if err := s.establishOutboundDialog(invite, peer, dialog, false); err != nil {
		return err
	}
	if err := s.runEstablishedDialogUntilNetworkLoss(dialog, behavior.After); err != nil {
		return err
	}
	// Stop SIP immediately at the loss boundary. The ordinary execute cleanup
	// remains responsible for bounded RTP drain and final in-memory results.
	s.carrier.shutdown()
	return nil
}

func (s *Session) runOutboundEarlyMedia(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, started time.Time, behavior EarlyMedia) error {
	latest := responseFor(invite, 100, "", nil)
	if err := s.sendSIP(latest, peer, false); err != nil {
		return err
	}
	canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.ProgressAfter, latest)
	if err != nil || canceled {
		return err
	}
	sdpBody, err := s.localAnswerSDP(dialog)
	if err != nil {
		return err
	}
	progress := responseFor(invite, 183, dialog.localTag, sdpBody)
	progress.Add("Contact", localContact(s.sipAddr, "sutel"))
	if err := s.sendSIP(progress, peer, false); err != nil {
		return err
	}
	latest = progress
	cancelMedia, mediaDone := s.startEarlyPlayback(dialog, behavior.File)
	if behavior.SendRinging {
		canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.RingingAfter, latest)
		if err != nil || canceled {
			cancelMedia()
			<-mediaDone
			return err
		}
		ringing := responseFor(invite, 180, dialog.localTag, nil)
		ringing.Add("Contact", localContact(s.sipAddr, "sutel"))
		if err := s.sendSIP(ringing, peer, false); err != nil {
			cancelMedia()
			<-mediaDone
			return err
		}
		latest = ringing
	}
	canceled, err = s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.AnswerAfter, latest)
	if err != nil || canceled {
		cancelMedia()
		<-mediaDone
		return err
	}
	cancelMedia()
	mediaErr := <-mediaDone
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return mediaErr
	}
	return s.answerAndRunDialog(invite, peer, dialog, s.remainingUntil(started, behavior.HangupAfter), nil)
}

func (s *Session) runOutboundEarlyFailure(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, started time.Time, behavior EarlyFailure) error {
	var latest *sip.Message
	if !behavior.OmitTrying {
		canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.TryingAfter, latest)
		if err != nil || canceled {
			return err
		}
		latest = responseFor(invite, 100, "", nil)
		if err := s.sendSIP(latest, peer, false); err != nil {
			return err
		}
	}
	canceled, err := s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.ProgressAfter, latest)
	if err != nil || canceled {
		return err
	}
	body, err := s.localAnswerSDP(dialog)
	if err != nil {
		return err
	}
	progress := responseFor(invite, 183, dialog.localTag, body)
	progress.Add("Contact", localContact(s.sipAddr, "sutel"))
	progress.Add("Content-Disposition", "session")
	if err := s.sendSIP(progress, peer, false); err != nil {
		return err
	}
	latest = progress
	cancelMedia, mediaDone := s.startEarlyPlayback(dialog, behavior.File)
	canceled, err = s.waitForBehaviorDeadline(invite, peer, dialog, started, behavior.FailureAfter, latest)
	if err != nil || canceled {
		cancelMedia()
		<-mediaDone
		return err
	}
	cancelMedia()
	mediaErr := <-mediaDone
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return mediaErr
	}
	status := behavior.FinalStatus
	if status == 0 {
		status = 486
	}
	final := responseFor(invite, status, dialog.localTag, nil)
	if behavior.FinalReason != "" {
		final.Reason = behavior.FinalReason
	}
	if behavior.ReasonHeader != "" {
		final.Add("Reason", behavior.ReasonHeader)
	}
	if err := s.sendSIP(final, peer, false); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = status })
	return s.waitForInviteACK(invite, final, peer)
}

func (s *Session) startEarlyPlayback(dialog *callDialog, path string) (context.CancelFunc, <-chan error) {
	mediaCtx, cancelMedia := context.WithCancel(s.ctx)
	mediaDone := make(chan error, 1)
	go func() {
		samples, err := mediaaudio.Normalize8kMono(path)
		if err != nil {
			mediaDone <- err
			return
		}
		if !dialog.remoteSDP.CanSend() {
			mediaDone <- nil
			return
		}
		mediaDone <- s.sender.Play(mediaCtx, dialog.remoteMedia, toRTPCodec(dialog.codec), dialog.payload, samples)
	}()
	return cancelMedia, mediaDone
}

func (s *Session) remainingUntil(started time.Time, after time.Duration) time.Duration {
	if after == 0 {
		return 0
	}
	remaining := started.Add(after).Sub(s.carrier.clock.Now())
	if remaining <= 0 {
		return -1
	}
	return remaining
}

func (s *Session) answerAndRunDialog(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, hangupAfter time.Duration, behavior *Answer) error {
	captureEcho := behavior != nil && behavior.Echo != nil
	if err := s.establishOutboundDialog(invite, peer, dialog, captureEcho); err != nil {
		return err
	}
	media := s.startAnsweredMedia(dialog, behavior)
	var localHangup <-chan struct{}
	if behavior != nil && (behavior.Playback != nil || behavior.Echo != nil) {
		localHangup = media.done
	}
	termination, dialogErr := dialogNeedsLocalBYE, error(nil)
	if hangupAfter >= 0 {
		termination, dialogErr = s.runEstablishedDialog(dialog, hangupAfter, localHangup)
	}
	media.cancel()
	mediaErr := media.wait()
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

func (s *Session) establishOutboundDialog(invite *sip.Message, peer netip.AddrPort, dialog *callDialog, captureEcho bool) error {
	if captureEcho {
		// Enable live capture before sending 200 so RTP sent immediately after the
		// peer's ACK cannot race echo startup.
		_ = s.receiver.AudioFrames()
	}
	body, err := s.localAnswerSDP(dialog)
	if err != nil {
		return err
	}
	ok := responseFor(invite, 200, dialog.localTag, body)
	ok.Add("Contact", localContact(s.sipAddr, "sutel"))
	dialog.lastInviteOK = ok
	if err := s.sendSIP(ok, peer, false); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.InviteFinalStatus = 200 })
	if err := s.waitForInviteACK(invite, ok, peer); err != nil {
		return err
	}
	s.setOutcome(func(outcome *SIPOutcome) { outcome.Established = true })
	return nil
}

type answeredMedia struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (media *answeredMedia) wait() error {
	<-media.done
	return media.err
}

func (s *Session) startAnsweredMedia(dialog *callDialog, behavior *Answer) *answeredMedia {
	mediaCtx, cancel := context.WithCancel(s.ctx)
	media := &answeredMedia{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(media.done)
		if behavior == nil {
			return
		}
		switch {
		case behavior.Playback != nil:
			if !dialog.remoteSDP.CanSend() {
				media.err = fmt.Errorf("%w: remote SDP does not allow receiving media", ErrNegotiation)
				return
			}
			samples, err := mediaaudio.Normalize8kMono(behavior.Playback.File)
			if err != nil {
				media.err = err
				return
			}
			media.err = s.sender.Play(mediaCtx, dialog.remoteMedia, toRTPCodec(dialog.codec), dialog.payload, samples)
		case behavior.Echo != nil:
			if !dialog.remoteSDP.CanSend() || !dialog.remoteSDP.CanReceive() {
				media.err = fmt.Errorf("%w: remote SDP does not allow bidirectional echo media", ErrNegotiation)
				return
			}
			media.err = s.runAudioEcho(mediaCtx, dialog, behavior.Echo.Delay)
		}
	}()
	return media
}

type delayedAudioFrame struct {
	due     time.Time
	samples []int16
}

func (s *Session) runAudioEcho(ctx context.Context, dialog *callDialog, delay time.Duration) error {
	frames := s.receiver.AudioFrames()
	queue := make([]delayedAudioFrame, 0, 256)
	marker := true
	var lastDue time.Time
	appendFrame := func(frame rtpverifier.AudioFrame) {
		due := frame.ArrivalTime.Add(delay)
		if minimum := lastDue.Add(20 * time.Millisecond); !lastDue.IsZero() && due.Before(minimum) {
			due = minimum
		}
		lastDue = due
		queue = append(queue, delayedAudioFrame{due: due, samples: frame.Samples})
	}
	for {
		if len(queue) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case frame, ok := <-frames:
				if !ok {
					return nil
				}
				appendFrame(frame)
			}
			continue
		}
		wait := time.Until(queue[0].due)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case frame, ok := <-frames:
			if !timer.Stop() {
				<-timer.C
			}
			if !ok {
				frames = nil
				continue
			}
			appendFrame(frame)
		case <-timer.C:
			frame := queue[0]
			queue = queue[1:]
			if err := s.sender.SendAudioFrame(dialog.remoteMedia, toRTPCodec(dialog.codec), dialog.payload, frame.samples, marker); err != nil {
				return err
			}
			marker = false
		}
	}
}

func (s *Session) localAnswerSDP(dialog *callDialog) ([]byte, error) {
	body, err := sip.GenerateSDP(
		s.receiver.Addr().Addr(), s.receiver.Addr().Port(), []uint8{dialog.payload},
		map[uint8]sip.RTPMap{dialog.payload: {Encoding: dialog.codec.String(), ClockRate: 8000}},
		dialog.telephoneEvent, dialog.remoteSDP.AnswerDirection(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: generate SDP answer: %v", ErrNegotiation, err)
	}
	return body, nil
}

func (s *Session) waitForInviteACK(invite, final *sip.Message, peer netip.AddrPort) error {
	interval := s.carrier.config.SIPT1
	retransmitTimer := s.carrier.clock.NewTimer(interval)
	defer func() { stopTransactionTimer(retransmitTimer) }()
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, timerChannel(retransmitTimer), nil, nil)
		if err != nil {
			return err
		}
		if timeout == retransmitSIPReadTimeout {
			if err := s.sendSIP(final, peer, true); err != nil {
				return err
			}
			stopTransactionTimer(retransmitTimer)
			interval = min(interval*2, s.carrier.config.SIPT2)
			retransmitTimer = s.carrier.clock.NewTimer(interval)
			continue
		}
		if message.IsRequest() && strings.EqualFold(message.Method, "ACK") && isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "ACK") {
			fromMatches := sip.HeaderParameter(message.Get("From"), "tag") == sip.HeaderParameter(invite.Get("From"), "tag")
			toMatches := sip.HeaderParameter(message.Get("To"), "tag") == sip.HeaderParameter(final.Get("To"), "tag")
			branchMatches := final.StatusCode < 300 || sip.ViaBranch(message.Get("Via")) == sip.ViaBranch(invite.Get("Via"))
			if fromMatches && toMatches && branchMatches {
				return nil
			}
			s.ignoreMessage()
			continue
		}
		if message.IsRequest() && strings.EqualFold(message.Method, "INVITE") && isMatching(message, invite.Get("Call-ID"), mustCSeq(invite), "INVITE") && sip.ViaBranch(message.Get("Via")) == sip.ViaBranch(invite.Get("Via")) {
			if err := s.sendSIP(final, source, true); err != nil {
				return err
			}
			continue
		}
		if message.IsRequest() && (strings.EqualFold(message.Method, "CANCEL") || strings.EqualFold(message.Method, "BYE") || strings.EqualFold(message.Method, "INFO")) {
			if err := s.sendSIP(responseFor(message, 481, "", nil), source, false); err != nil {
				return err
			}
			continue
		}
		s.ignoreMessage()
	}
}

func mustCSeq(message *sip.Message) uint32 {
	cseq, _ := message.CSeq()
	return cseq.Number
}

func toRTPCodec(codec Codec) rtpverifier.Codec {
	if codec == PCMU {
		return rtpverifier.PCMU
	}
	return rtpverifier.PCMA
}
