package sutel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/subiz/sutel/rtpverifier"
)

type Session struct {
	carrier   *carrier
	direction CallDirection
	ctx       context.Context
	cancel    context.CancelFunc

	receiver *rtpverifier.Receiver
	sender   *rtpverifier.Sender
	sipAddr  netip.AddrPort

	outbound *OutboundScenario
	inbound  *InboundScenario

	startedAt time.Time
	done      chan struct{}
	sipInput  chan sipDatagram

	stateMu  sync.Mutex
	outcome  SIPOutcome
	media    NegotiatedMedia
	sipStats SIPStats
	events   []Event
	infoDTMF []dtmfRecord

	resultMu sync.Mutex
	result   CallResult
	err      error
}

type dtmfRecord struct {
	event  DTMFEvent
	method DTMFMethod
	at     time.Time
}

func (s *Session) SIPAddr() netip.AddrPort { return s.sipAddr }

func (s *Session) Wait() (CallResult, error) {
	<-s.done
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	return s.result.clone(), s.err
}

// Close cancels the session and waits for its resources to stop. Call Wait to
// inspect the final result and call error.
func (s *Session) Close() {
	s.cancel()
	<-s.done
}

func (s *Session) execute() {
	defer close(s.done)
	defer s.carrier.shutdown()
	defer s.cancel()
	var runErr error
	if s.outbound != nil {
		runErr = s.runOutbound()
	} else {
		runErr = s.runInbound()
	}
	rtpResult, rtpErr := s.receiver.Stop(context.Background(), s.carrier.config.RTPDrainTimeout)
	if errors.Is(rtpErr, rtpverifier.ErrCodecSwitch) {
		rtpErr = fmt.Errorf("%w: %v", ErrNegotiation, rtpErr)
	}
	senderStats := s.sender.Stats()
	senderEvents := s.sender.Events()
	s.stateMu.Lock()
	result := CallResult{
		Direction:   s.direction,
		SIP:         s.sipStats,
		Outcome:     s.outcome,
		Media:       s.media,
		RTP:         convertRTPStats(rtpResult.Stats, senderStats),
		StartedAt:   s.startedAt,
		EndedAt:     time.Now(),
		receivedPCM: slices.Clone(rtpResult.PCM),
		events:      slices.Clone(s.events),
	}
	dtmfRecords := slices.Clone(s.infoDTMF)
	s.stateMu.Unlock()
	for _, event := range rtpResult.DTMF {
		dtmfRecords = append(dtmfRecords, dtmfRecord{
			event:  DTMFEvent{Digit: event.Digit, Duration: event.Duration, Volume: event.Volume, Timestamp: event.Timestamp},
			method: RFC4733, at: event.ArrivalTime,
		})
	}
	sort.SliceStable(dtmfRecords, func(i, j int) bool { return dtmfRecords[i].at.Before(dtmfRecords[j].at) })
	result.dtmfRecords = slices.Clone(dtmfRecords)
	for _, record := range dtmfRecords {
		result.dtmf = append(result.dtmf, record.event)
	}
	for _, event := range rtpResult.Events {
		result.events = append(result.events, Event{
			Time: event.ArrivalTime, Layer: RTPLayer, Direction: Received,
			Type: "packet", Detail: fmt.Sprintf("PT=%d seq=%d ts=%d", event.PayloadType, event.SequenceNumber, event.Timestamp),
		})
	}
	for _, event := range senderEvents {
		detail := fmt.Sprintf("PT=%d seq=%d ts=%d", event.PayloadType, event.SequenceNumber, event.Timestamp)
		if !event.Successful {
			detail += " failed=" + event.Error
		}
		result.events = append(result.events, Event{
			Time: event.ArrivalTime, Layer: RTPLayer, Direction: Sent,
			Type: "packet", Detail: detail,
		})
	}
	sort.SliceStable(result.events, func(i, j int) bool { return result.events[i].Time.Before(result.events[j].Time) })
	verificationErr := s.verify(&result)
	combined := errors.Join(runErr, rtpErr, verificationErr)
	s.resultMu.Lock()
	s.result = result
	if combined != nil {
		s.err = &Error{
			Kind: classifyError(combined), Scenario: scenarioName(s),
			Detail: diagnostics(s, result), Cause: combined,
		}
	}
	s.resultMu.Unlock()
}

func (s *Session) verify(result *CallResult) error {
	var expectation *AudioExpectation
	if s.outbound != nil {
		expectation = s.outbound.ExpectAudio
	} else if s.inbound != nil {
		expectation = s.inbound.ExpectAudio
	}
	if expectation != nil {
		match, err := verifyAudioSamples(*expectation, result.receivedPCM)
		if err == nil || errors.Is(err, ErrVerification) {
			result.Audio = &match
		}
		if err != nil {
			return err
		}
	}
	if s.outbound != nil && s.outbound.DTMF != nil {
		var digits strings.Builder
		var unexpected strings.Builder
		for _, record := range result.dtmfRecords {
			if record.method == s.outbound.DTMF.Method {
				digits.WriteString(record.event.Digit)
			} else {
				unexpected.WriteString(record.event.Digit)
			}
		}
		if unexpected.Len() > 0 {
			return fmt.Errorf("%w: unexpected DTMF method supplied digits %q", ErrVerification, unexpected.String())
		}
		if digits.String() != strings.ToUpper(s.outbound.DTMF.Digits) {
			return fmt.Errorf("%w: DTMF got %q want %q", ErrVerification, digits.String(), strings.ToUpper(s.outbound.DTMF.Digits))
		}
	}
	return nil
}

func (s *Session) addEvent(layer EventLayer, direction EventDirection, eventType, detail string) {
	s.stateMu.Lock()
	s.events = append(s.events, Event{Time: time.Now(), Layer: layer, Direction: direction, Type: eventType, Detail: detail})
	s.stateMu.Unlock()
}

func convertRTPStats(stats rtpverifier.Stats, sent rtpverifier.SenderStats) RTPStats {
	codec := PCMA
	if stats.Codec == rtpverifier.PCMU {
		codec = PCMU
	}
	return RTPStats{
		SSRC: stats.SSRC, Codec: codec, PacketsReceived: stats.PacketsReceived,
		PacketsAttempted: sent.PacketsAttempted, PacketsSent: sent.PacketsSent,
		FailedSends: sent.FailedSends, SkippedFrames: sent.SkippedFrames,
		FirstSequence: stats.FirstSequence, LastSequence: stats.LastSequence,
		FirstTimestamp: stats.FirstTimestamp, LastTimestamp: stats.LastTimestamp,
		DuplicatePackets: stats.DuplicatePackets, MissingPackets: stats.MissingPackets,
		OutOfOrderPackets: stats.OutOfOrderPackets, IgnoredPackets: stats.IgnoredPackets,
		MalformedPackets: stats.MalformedPackets, ForeignSSRC: stats.ForeignSSRC,
		Discontinuities: stats.Discontinuities + sent.Discontinuities, ConflictingPackets: stats.ConflictingPackets,
		IncompleteDTMF: stats.IncompleteDTMF, CodecSwitches: stats.CodecSwitches,
		ReceivedDuration: stats.ReceivedDuration, SentDuration: sent.Duration,
	}
}

func classifyError(err error) error {
	for _, kind := range []error{ErrInvalidExpectation, ErrNegotiation, ErrTransport, ErrProtocol, ErrVerification, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	return ErrVerification
}

func scenarioName(session *Session) string {
	if session.outbound != nil {
		return "outbound-" + session.outbound.Behavior.behaviorName()
	}
	return "inbound"
}

func diagnostics(session *Session, result CallResult) string {
	var detail strings.Builder
	fmt.Fprintf(&detail, "scenario: %s\n", scenarioName(session))
	fmt.Fprintf(&detail, "state: final=%d established=%t canceled=%t terminated-by=%s\n", result.Outcome.InviteFinalStatus, result.Outcome.Established, result.Outcome.Canceled, result.Outcome.TerminatedBy)
	fmt.Fprintf(&detail, "SIP: local=%s remote=%s sent=%d received=%d retransmissions=%d malformed=%d ignored=%d\n",
		result.SIP.LocalAddr, result.SIP.RemoteAddr,
		result.SIP.RequestsSent+result.SIP.ResponsesSent, result.SIP.RequestsReceived+result.SIP.ResponsesReceived,
		result.SIP.Retransmissions, result.SIP.MalformedMessages, result.SIP.IgnoredMessages)
	var sipEvents []Event
	for _, event := range result.events {
		if event.Layer == SIPLayer {
			sipEvents = append(sipEvents, event)
		}
	}
	if len(sipEvents) > 5 {
		sipEvents = sipEvents[len(sipEvents)-5:]
	}
	for _, event := range sipEvents {
		direction := "<-"
		if event.Direction == Sent {
			direction = "->"
		}
		fmt.Fprintf(&detail, "SIP %s %s\n", direction, event.Detail)
	}
	fmt.Fprintf(&detail, "media: codec=%s telephone-event=%d\n", result.Media.AudioCodec, result.Media.TelephoneEventPayloadType)
	fmt.Fprintf(&detail, "RTP: received=%d attempted=%d sent=%d failed=%d ignored=%d missing=%d out-of-order=%d conflicts=%d incomplete-dtmf=%d codec-switches=%d discontinuities=%d duration=%s\n",
		result.RTP.PacketsReceived, result.RTP.PacketsAttempted, result.RTP.PacketsSent, result.RTP.FailedSends,
		result.RTP.IgnoredPackets, result.RTP.MissingPackets, result.RTP.OutOfOrderPackets, result.RTP.ConflictingPackets,
		result.RTP.IncompleteDTMF, result.RTP.CodecSwitches, result.RTP.Discontinuities, result.RTP.ReceivedDuration)
	var digits strings.Builder
	for _, event := range result.dtmf {
		digits.WriteString(event.Digit)
	}
	fmt.Fprintf(&detail, "DTMF: events=%d digits=%q\n", len(result.dtmf), digits.String())
	if result.Audio != nil {
		requiredSimilarity, requiredCoverage, expectedFile := 0.0, 0.0, ""
		var expectation *AudioExpectation
		if session.outbound != nil {
			expectation = session.outbound.ExpectAudio
		} else if session.inbound != nil {
			expectation = session.inbound.ExpectAudio
		}
		if expectation != nil {
			requiredSimilarity = expectation.MinSimilarity
			requiredCoverage = expectation.MinCoverage
			expectedFile = expectation.File
		}
		fmt.Fprintf(&detail, "audio: expected=%q similarity=%.3f required=%.3f coverage=%.3f required=%.3f expected-duration=%s received-duration=%s offset=%s\n",
			expectedFile, result.Audio.Similarity, requiredSimilarity, result.Audio.Coverage, requiredCoverage,
			result.Audio.ExpectedDuration, result.Audio.ReceivedDuration, result.Audio.AlignmentOffset)
	}
	return strings.TrimSuffix(detail.String(), "\n")
}
