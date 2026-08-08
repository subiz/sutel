package sutel

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	mediaaudio "github.com/subiz/sutel/audio"
)

type AudioMatchResult struct {
	Similarity float64 `json:"similarity"`
	Coverage   float64 `json:"coverage"`

	AlignmentOffset  time.Duration `json:"alignment_offset"`
	ExpectedDuration time.Duration `json:"expected_duration"`
	ReceivedDuration time.Duration `json:"received_duration"`
	ComparedDuration time.Duration `json:"compared_duration"`
}

type RTPStats struct {
	SSRC  uint32
	Codec Codec

	PacketsReceived  int
	PacketsAttempted int
	PacketsSent      int
	FailedSends      int
	SkippedFrames    int

	FirstSequence  uint16
	LastSequence   uint16
	FirstTimestamp uint32
	LastTimestamp  uint32

	DuplicatePackets   int
	MissingPackets     int
	OutOfOrderPackets  int
	IgnoredPackets     int
	MalformedPackets   int
	ForeignSSRC        int
	Discontinuities    int
	ConflictingPackets int
	IncompleteDTMF     int
	CodecSwitches      int

	ReceivedDuration time.Duration
	SentDuration     time.Duration
}

type SIPStats struct {
	LocalAddr  netip.AddrPort
	RemoteAddr netip.AddrPort

	RequestsSent      int
	RequestsReceived  int
	ResponsesSent     int
	ResponsesReceived int
	Retransmissions   int
	MalformedMessages int
	IgnoredMessages   int
}

type SIPOutcome struct {
	InviteFinalStatus int
	Established       bool
	Canceled          bool
	TerminatedBy      CallParty
}

type NegotiatedMedia struct {
	AudioCodec Codec

	// TelephoneEventPayloadType is -1 when telephone-event was not negotiated.
	TelephoneEventPayloadType int
}

type DTMFEvent struct {
	Digit     string
	Duration  time.Duration
	Volume    uint8
	Timestamp uint32
}

type EventLayer string
type EventDirection string

const (
	SIPLayer EventLayer     = "SIP"
	RTPLayer EventLayer     = "RTP"
	Sent     EventDirection = "sent"
	Received EventDirection = "received"
)

type Event struct {
	Time      time.Time
	Layer     EventLayer
	Direction EventDirection
	Type      string
	Detail    string
}

type CallResult struct {
	Direction CallDirection
	SIP       SIPStats
	Outcome   SIPOutcome
	RTP       RTPStats
	Media     NegotiatedMedia
	Audio     *AudioMatchResult
	StartedAt time.Time
	EndedAt   time.Time

	dtmf        []DTMFEvent
	dtmfRecords []dtmfRecord
	receivedPCM []int16
	events      []Event
}

func (r CallResult) DTMFEvents() []DTMFEvent {
	return slices.Clone(r.dtmf)
}

func (r CallResult) ReceivedPCM() []int16 {
	return slices.Clone(r.receivedPCM)
}

func (r CallResult) Events() []Event { return slices.Clone(r.events) }

// ReceivedWAV returns received audio as a mono 8 kHz WAV file in memory.
func (r CallResult) ReceivedWAV() []byte {
	if len(r.receivedPCM) == 0 {
		return nil
	}
	var buffer bytes.Buffer
	// bytes.Buffer never returns a write error, and the sample rate is fixed and
	// valid, so WAV encoding cannot fail for an in-memory result.
	_ = mediaaudio.WriteWAV(&buffer, r.receivedPCM, 8000)
	return slices.Clone(buffer.Bytes())
}

// SIPTrace returns a human-readable relative-time trace of SIP events.
func (r CallResult) SIPTrace() string {
	var trace strings.Builder
	for _, event := range r.events {
		if event.Layer != SIPLayer {
			continue
		}
		offset := event.Time.Sub(r.StartedAt)
		if offset < 0 {
			offset = 0
		}
		direction := "<-"
		if event.Direction == Sent {
			direction = "->"
		}
		fmt.Fprintf(&trace, "%06.3f SIP %s %s\n", offset.Seconds(), direction, event.Detail)
	}
	return trace.String()
}

func (r CallResult) clone() CallResult {
	r.dtmf = slices.Clone(r.dtmf)
	r.dtmfRecords = slices.Clone(r.dtmfRecords)
	r.receivedPCM = slices.Clone(r.receivedPCM)
	r.events = slices.Clone(r.events)
	if r.Audio != nil {
		audio := *r.Audio
		r.Audio = &audio
	}
	return r
}
