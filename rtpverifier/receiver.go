package rtpverifier

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	mediaaudio "github.com/subiz/sutel/audio"

	pionrtp "github.com/pion/rtp"
)

var ErrCodecSwitch = errors.New("RTP codec changed without renegotiation")

type Codec uint8

const (
	PCMA Codec = iota
	PCMU
)

const maxRetainedRTPPackets = 10000

type Config struct {
	LocalIP netip.Addr

	AudioPayloads           map[uint8]Codec
	TelephoneEventPayload   int
	TelephoneEventClockRate int
	ExpectedSourceIP        netip.Addr

	ReorderWindow       int
	MaxSynthesizedGap   time.Duration
	ReadBufferSizeBytes int
}

type PacketEvent struct {
	Version        uint8     `json:"version"`
	Marker         bool      `json:"marker"`
	PayloadType    uint8     `json:"payload_type"`
	SequenceNumber uint16    `json:"sequence_number"`
	Timestamp      uint32    `json:"timestamp"`
	SSRC           uint32    `json:"ssrc"`
	ArrivalTime    time.Time `json:"arrival_time"`
	Source         string    `json:"source"`
	Attempted      bool      `json:"attempted,omitempty"`
	Successful     bool      `json:"successful,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type Stats struct {
	SSRC  uint32
	Codec Codec

	PacketsReceived int
	FirstSequence   uint16
	LastSequence    uint16
	FirstTimestamp  uint32
	LastTimestamp   uint32

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
}

type DTMFEvent struct {
	Digit       string
	Duration    time.Duration
	Volume      uint8
	Timestamp   uint32
	SSRC        uint32
	ArrivalTime time.Time
}

// AudioFrame is a decoded live RTP audio frame. Samples are mono 8 kHz PCM.
type AudioFrame struct {
	ArrivalTime time.Time
	Samples     []int16
}

type Result struct {
	Stats  Stats
	PCM    []int16
	DTMF   []DTMFEvent
	Events []PacketEvent
}

type packetRecord struct {
	packet            pionrtp.Packet
	extendedSequence  int64
	extendedTimestamp int64
	arrival           time.Time
	source            netip.AddrPort
}

type packetIdentity struct {
	ssrc     uint32
	sequence int64
}

type dtmfIdentity struct {
	ssrc      uint32
	timestamp uint32
	event     byte
}

type dtmfState struct {
	duration uint16
	volume   uint8
	ended    bool
	emitted  bool
	event    int
}

type Receiver struct {
	config Config
	conn   *net.UDPConn
	addr   netip.AddrPort

	mu sync.Mutex

	closed       bool
	selectedSSRC uint32
	selected     bool
	codec        Codec
	source       netip.AddrPort
	sourceLocked bool

	highestSequence    map[uint32]int64
	highestTimestamp   map[uint32]int64
	seen               map[packetIdentity]packetRecord
	packets            []packetRecord
	sequenceSpace      []int64
	dtmf               map[dtmfIdentity]*dtmfState
	dtmfEvents         []DTMFEvent
	events             []PacketEvent
	stats              Stats
	lastValidPacket    time.Time
	bufferLimitHit     bool
	haveLastAudio      bool
	lastAudioSequence  int64
	lastAudioTimestamp int64
	audioFrames        chan AudioFrame

	done chan struct{}
	err  error
}

func New(config Config) (*Receiver, error) {
	if !config.LocalIP.IsValid() {
		config.LocalIP = netip.MustParseAddr("127.0.0.1")
	}
	if config.LocalIP.IsUnspecified() {
		return nil, fmt.Errorf("unspecified LocalIP is not allowed")
	}
	if len(config.AudioPayloads) == 0 {
		config.AudioPayloads = map[uint8]Codec{8: PCMA, 0: PCMU}
	}
	if config.TelephoneEventPayload == 0 {
		config.TelephoneEventPayload = 101
	}
	if config.TelephoneEventPayload < -1 || config.TelephoneEventPayload > 127 {
		return nil, fmt.Errorf("invalid telephone-event payload type %d", config.TelephoneEventPayload)
	}
	if config.TelephoneEventClockRate == 0 {
		config.TelephoneEventClockRate = 8000
	}
	if config.ReorderWindow == 0 {
		config.ReorderWindow = 10
	}
	if config.MaxSynthesizedGap == 0 {
		config.MaxSynthesizedGap = 2 * time.Second
	}
	if config.ReadBufferSizeBytes == 0 {
		config.ReadBufferSizeBytes = 1 << 20
	}
	if config.ReorderWindow < 0 || config.MaxSynthesizedGap < 0 || config.TelephoneEventClockRate <= 0 {
		return nil, fmt.Errorf("invalid receiver configuration")
	}
	udpAddress := net.UDPAddrFromAddrPort(netip.AddrPortFrom(config.LocalIP, 0))
	connection, err := net.ListenUDP("udp", udpAddress)
	if err != nil {
		return nil, fmt.Errorf("listen RTP: %w", err)
	}
	_ = connection.SetReadBuffer(config.ReadBufferSizeBytes)
	local := connection.LocalAddr().(*net.UDPAddr).AddrPort()
	receiver := &Receiver{
		config:           config,
		conn:             connection,
		addr:             local,
		highestSequence:  make(map[uint32]int64),
		highestTimestamp: make(map[uint32]int64),
		seen:             make(map[packetIdentity]packetRecord),
		dtmf:             make(map[dtmfIdentity]*dtmfState),
		done:             make(chan struct{}),
	}
	go receiver.readLoop()
	return receiver, nil
}

func (r *Receiver) Addr() netip.AddrPort {
	return r.addr
}

// AudioFrames enables and streams accepted audio packets as decoded PCM for
// live media behaviors such as delayed echo. A slow consumer may miss frames.
func (r *Receiver) AudioFrames() <-chan AudioFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.audioFrames == nil {
		r.audioFrames = make(chan AudioFrame, 1024)
	}
	return r.audioFrames
}

// Configure applies negotiated payload types. Packets may arrive immediately
// after the peer sends its SDP, before signaling has configured the receiver;
// retain them when every accepted packet is compatible with the negotiation.
func (r *Receiver) Configure(audioPayloads map[uint8]Codec, telephoneEvent int, expectedSourceIP netip.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return net.ErrClosed
	}
	if len(audioPayloads) == 0 || telephoneEvent < -1 || telephoneEvent > 127 {
		return fmt.Errorf("invalid negotiated RTP payloads")
	}
	for _, record := range r.seen {
		codec, audio := audioPayloads[record.packet.PayloadType]
		dtmf := telephoneEvent >= 0 && int(record.packet.PayloadType) == telephoneEvent
		if !audio && !dtmf {
			return fmt.Errorf("received payload type %d before incompatible negotiation", record.packet.PayloadType)
		}
		if audio && r.selected && record.packet.SSRC == r.selectedSSRC && codec != r.codec {
			return fmt.Errorf("received codec before incompatible negotiation")
		}
		if expectedSourceIP.IsValid() && record.source.Addr().Unmap() != expectedSourceIP.Unmap() {
			return fmt.Errorf("received media from %s before incompatible negotiation", record.source.Addr())
		}
	}
	r.config.AudioPayloads = make(map[uint8]Codec, len(audioPayloads))
	for payload, codec := range audioPayloads {
		r.config.AudioPayloads[payload] = codec
	}
	r.config.TelephoneEventPayload = telephoneEvent
	r.config.ExpectedSourceIP = expectedSourceIP
	return nil
}

func (r *Receiver) writePacket(packet *pionrtp.Packet, target netip.AddrPort) error {
	data, err := packet.Marshal()
	if err != nil {
		return fmt.Errorf("marshal RTP: %w", err)
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return net.ErrClosed
	}
	if _, err := r.conn.WriteToUDPAddrPort(data, target); err != nil {
		return fmt.Errorf("write RTP: %w", err)
	}
	return nil
}

func (r *Receiver) LastError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Receiver) Stop(ctx context.Context, quiet time.Duration) (Result, error) {
	if quiet > 0 {
		ticker := time.NewTicker(min(quiet/4, 25*time.Millisecond))
		defer ticker.Stop()
		maximumDrain := time.NewTimer(max(4*quiet, time.Second))
		defer maximumDrain.Stop()
	drain:
		for {
			r.mu.Lock()
			last := r.lastValidPacket
			r.mu.Unlock()
			if last.IsZero() || time.Since(last) >= quiet {
				break
			}
			select {
			case <-ctx.Done():
				_ = r.Close()
				return Result{}, ctx.Err()
			case <-maximumDrain.C:
				break drain
			case <-ticker.C:
			}
		}
	}
	closeErr := r.Close()
	result, resultErr := r.Result()
	return result, errors.Join(closeErr, resultErr)
}

func (r *Receiver) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return r.err
	}
	r.closed = true
	r.mu.Unlock()
	err := r.conn.Close()
	<-r.done
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return r.LastError()
}

func (r *Receiver) Result() (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := r.resultLocked()
	return result, r.err
}

func (r *Receiver) readLoop() {
	defer close(r.done)
	buffer := make([]byte, 64*1024)
	for {
		length, source, err := r.conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.mu.Lock()
			r.err = fmt.Errorf("read RTP: %w", err)
			r.mu.Unlock()
			return
		}
		arrival := time.Now()
		var packet pionrtp.Packet
		if err := packet.Unmarshal(buffer[:length]); err != nil {
			r.mu.Lock()
			r.stats.MalformedPackets++
			r.stats.IgnoredPackets++
			r.mu.Unlock()
			continue
		}
		// Packet.Unmarshal aliases the supplied datagram buffer. Keep a deep
		// copy because reconstruction happens after subsequent reads reuse it.
		r.accept(*packet.Clone(), source, arrival)
	}
}

func (r *Receiver) accept(packet pionrtp.Packet, source netip.AddrPort, arrival time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event := PacketEvent{
		Version:        packet.Version,
		Marker:         packet.Marker,
		PayloadType:    packet.PayloadType,
		SequenceNumber: packet.SequenceNumber,
		Timestamp:      packet.Timestamp,
		SSRC:           packet.SSRC,
		ArrivalTime:    arrival,
		Source:         source.String(),
	}
	if len(r.events) < maxRetainedRTPPackets {
		r.events = append(r.events, event)
	}
	codec, audioPacket := r.config.AudioPayloads[packet.PayloadType]
	dtmfPacket := r.config.TelephoneEventPayload >= 0 && int(packet.PayloadType) == r.config.TelephoneEventPayload
	if packet.Version != 2 || !audioPacket && !dtmfPacket {
		r.stats.IgnoredPackets++
		return
	}
	if r.config.ExpectedSourceIP.IsValid() && source.Addr().Unmap() != r.config.ExpectedSourceIP.Unmap() {
		r.stats.IgnoredPackets++
		return
	}
	if r.sourceLocked && source != r.source {
		r.stats.IgnoredPackets++
		return
	}
	if audioPacket {
		if !r.selected {
			r.selected = true
			r.selectedSSRC = packet.SSRC
			r.codec = codec
			r.source = source
			r.sourceLocked = true
			r.stats.SSRC = packet.SSRC
			r.stats.Codec = codec
		} else if packet.SSRC != r.selectedSSRC {
			r.stats.ForeignSSRC++
			r.stats.IgnoredPackets++
			return
		} else if codec != r.codec {
			r.stats.CodecSwitches++
			r.stats.IgnoredPackets++
			if r.err == nil {
				r.err = ErrCodecSwitch
			}
			return
		}
	}
	if dtmfPacket && r.selected && packet.SSRC != r.selectedSSRC {
		r.stats.ForeignSSRC++
		r.stats.IgnoredPackets++
		return
	}
	highestSequence, sequenceInitialized := r.highestSequence[packet.SSRC]
	extendedSequence := unwrap16(packet.SequenceNumber, highestSequence, sequenceInitialized)
	identity := packetIdentity{ssrc: packet.SSRC, sequence: extendedSequence}
	if previous, exists := r.seen[identity]; exists {
		r.stats.DuplicatePackets++
		if !samePacket(previous.packet, packet) {
			r.stats.IgnoredPackets++
			r.stats.ConflictingPackets++
		}
		return
	}
	if highest, exists := r.highestSequence[packet.SSRC]; exists && extendedSequence < highest-int64(r.config.ReorderWindow) {
		r.stats.OutOfOrderPackets++
		r.stats.IgnoredPackets++
		return
	}
	if len(r.seen) >= maxRetainedRTPPackets {
		r.stats.IgnoredPackets++
		if !r.bufferLimitHit {
			r.bufferLimitHit = true
			r.stats.Discontinuities++
		}
		return
	}
	if highest, exists := r.highestSequence[packet.SSRC]; exists {
		if extendedSequence < highest {
			r.stats.OutOfOrderPackets++
		} else if extendedSequence > highest {
			r.highestSequence[packet.SSRC] = extendedSequence
		}
	} else {
		r.highestSequence[packet.SSRC] = extendedSequence
	}
	highestTimestamp, timestampInitialized := r.highestTimestamp[packet.SSRC]
	extendedTimestamp := unwrap32(packet.Timestamp, highestTimestamp, timestampInitialized)
	if highest, exists := r.highestTimestamp[packet.SSRC]; !exists || extendedTimestamp > highest {
		r.highestTimestamp[packet.SSRC] = extendedTimestamp
	}
	record := packetRecord{packet: packet, extendedSequence: extendedSequence, extendedTimestamp: extendedTimestamp, arrival: arrival, source: source}
	r.seen[identity] = record
	if r.selected && packet.SSRC == r.selectedSSRC {
		r.sequenceSpace = append(r.sequenceSpace, extendedSequence)
	}
	if audioPacket {
		r.packets = append(r.packets, record)
		r.stats.PacketsReceived++
		if r.stats.PacketsReceived == 1 {
			r.stats.FirstSequence = packet.SequenceNumber
			r.stats.FirstTimestamp = packet.Timestamp
		}
		if !r.haveLastAudio || extendedSequence > r.lastAudioSequence {
			r.lastAudioSequence = extendedSequence
			r.stats.LastSequence = packet.SequenceNumber
		}
		if !r.haveLastAudio || extendedTimestamp > r.lastAudioTimestamp {
			r.lastAudioTimestamp = extendedTimestamp
			r.stats.LastTimestamp = packet.Timestamp
		}
		r.haveLastAudio = true
		codecName := "PCMA"
		if codec == PCMU {
			codecName = "PCMU"
		}
		if r.audioFrames != nil {
			frame := AudioFrame{ArrivalTime: arrival, Samples: mediaaudio.DecodeG711(packet.Payload, codecName)}
			select {
			case r.audioFrames <- frame:
			default:
			}
		}
	}
	if dtmfPacket {
		r.acceptDTMF(packet, arrival)
	}
	r.lastValidPacket = arrival
}

func (r *Receiver) acceptDTMF(packet pionrtp.Packet, arrival time.Time) {
	if len(packet.Payload) < 4 {
		r.stats.MalformedPackets++
		r.stats.IgnoredPackets++
		return
	}
	eventID := packet.Payload[0]
	digit, ok := dtmfDigit(eventID)
	if !ok {
		r.stats.IgnoredPackets++
		return
	}
	end := packet.Payload[1]&0x80 != 0
	volume := packet.Payload[1] & 0x3f
	duration := uint16(packet.Payload[2])<<8 | uint16(packet.Payload[3])
	identity := dtmfIdentity{ssrc: packet.SSRC, timestamp: packet.Timestamp, event: eventID}
	state := r.dtmf[identity]
	if state == nil {
		state = &dtmfState{volume: volume, event: -1}
		r.dtmf[identity] = state
	}
	if duration > state.duration {
		state.duration = duration
	}
	if end {
		state.ended = true
		if !state.emitted {
			state.emitted = true
			state.event = len(r.dtmfEvents)
			r.dtmfEvents = append(r.dtmfEvents, DTMFEvent{
				Digit:       digit,
				Duration:    time.Duration(state.duration) * time.Second / time.Duration(r.config.TelephoneEventClockRate),
				Volume:      state.volume,
				Timestamp:   packet.Timestamp,
				SSRC:        packet.SSRC,
				ArrivalTime: arrival,
			})
		} else if state.event >= 0 {
			r.dtmfEvents[state.event].Duration = time.Duration(state.duration) * time.Second / time.Duration(r.config.TelephoneEventClockRate)
		}
	}
}

func (r *Receiver) resultLocked() Result {
	packets := slices.Clone(r.packets)
	sort.SliceStable(packets, func(left, right int) bool {
		if packets[left].extendedTimestamp == packets[right].extendedTimestamp {
			return packets[left].extendedSequence < packets[right].extendedSequence
		}
		return packets[left].extendedTimestamp < packets[right].extendedTimestamp
	})
	stats := r.stats
	stats.MissingPackets = missingPackets(r.sequenceSpace)
	for identity, state := range r.dtmf {
		if !state.ended && (!r.selected || identity.ssrc == r.selectedSSRC) {
			stats.IncompleteDTMF++
		}
	}
	pcm := make([]int16, 0, len(packets)*160)
	var baseTimestamp int64
	for index, record := range packets {
		if index == 0 {
			baseTimestamp = record.extendedTimestamp
		}
		start := record.extendedTimestamp - baseTimestamp
		if start > int64(len(pcm)) {
			gap := start - int64(len(pcm))
			maximum := int64(r.config.MaxSynthesizedGap * 8000 / time.Second)
			if gap > maximum {
				stats.Discontinuities++
				baseTimestamp = record.extendedTimestamp - int64(len(pcm))
				start = int64(len(pcm))
			} else {
				pcm = append(pcm, make([]int16, int(gap))...)
			}
		}
		if start < int64(len(pcm)) {
			decoded := decode(record.packet.Payload, r.codec)
			overlap := min(len(decoded), len(pcm)-int(start))
			if overlap > 0 && !slices.Equal(decoded[:overlap], pcm[int(start):int(start)+overlap]) {
				stats.IgnoredPackets++
				stats.Discontinuities++
				stats.ConflictingPackets++
			}
			if overlap < len(decoded) {
				pcm = append(pcm, decoded[overlap:]...)
			}
			continue
		}
		decoded := decode(record.packet.Payload, r.codec)
		pcm = append(pcm, decoded...)
	}
	stats.ReceivedDuration = time.Duration(len(pcm)) * time.Second / 8000
	dtmfEvents := slices.Clone(r.dtmfEvents)
	if r.selected {
		dtmfEvents = slices.DeleteFunc(dtmfEvents, func(event DTMFEvent) bool { return event.SSRC != r.selectedSSRC })
	}
	return Result{
		Stats:  stats,
		PCM:    slices.Clone(pcm),
		DTMF:   dtmfEvents,
		Events: slices.Clone(r.events),
	}
}

func decode(payload []byte, codec Codec) []int16 {
	name := "PCMA"
	if codec == PCMU {
		name = "PCMU"
	}
	return mediaaudio.DecodeG711(payload, name)
}

func unwrap16(value uint16, highest int64, initialized bool) int64 {
	if !initialized {
		return int64(value)
	}
	base := highest &^ 0xffff
	candidate := base | int64(value)
	if candidate-highest > 0x8000 {
		candidate -= 0x10000
	} else if highest-candidate > 0x8000 {
		candidate += 0x10000
	}
	return candidate
}

func unwrap32(value uint32, highest int64, initialized bool) int64 {
	if !initialized {
		return int64(value)
	}
	base := highest &^ 0xffffffff
	candidate := base | int64(value)
	if candidate-highest > 0x80000000 {
		candidate -= 0x100000000
	} else if highest-candidate > 0x80000000 {
		candidate += 0x100000000
	}
	return candidate
}

func samePacket(left, right pionrtp.Packet) bool {
	return left.Timestamp == right.Timestamp && left.PayloadType == right.PayloadType && slices.Equal(left.Payload, right.Payload)
}

func missingPackets(sequences []int64) int {
	if len(sequences) < 2 {
		return 0
	}
	values := slices.Clone(sequences)
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	missing := 0
	previous := values[0]
	for _, value := range values[1:] {
		if value > previous+1 {
			missing += int(value - previous - 1)
		}
		if value > previous {
			previous = value
		}
	}
	return missing
}

func dtmfDigit(event byte) (string, bool) {
	switch {
	case event <= 9:
		return string(rune('0' + event)), true
	case event == 10:
		return "*", true
	case event == 11:
		return "#", true
	case event >= 12 && event <= 15:
		return string(rune('A' + event - 12)), true
	default:
		return "", false
	}
}
