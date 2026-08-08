package rtpverifier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pionrtp "github.com/pion/rtp"
	mediaaudio "github.com/subiz/sutel/audio"
)

const (
	audioPacketSamples = 160
	audioPacketTime    = 20 * time.Millisecond
)

type SenderStats struct {
	PacketsAttempted int
	PacketsSent      int
	FailedSends      int
	SkippedFrames    int
	Discontinuities  int
	Duration         time.Duration
}

type Sender struct {
	receiver *Receiver

	mu        sync.Mutex
	ssrc      uint32
	sequence  uint16
	timestamp uint32
	stats     SenderStats
	events    []PacketEvent
}

func NewSender(receiver *Receiver) *Sender {
	return newSenderWithReader(receiver, rand.Reader)
}

func newSenderWithReader(receiver *Receiver, reader io.Reader) *Sender {
	return &Sender{
		receiver:  receiver,
		ssrc:      randomUint32(reader),
		sequence:  uint16(randomUint32(reader)),
		timestamp: randomUint32(reader),
	}
}

func (s *Sender) Stats() SenderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Sender) Events() []PacketEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PacketEvent(nil), s.events...)
}

func (s *Sender) Play(ctx context.Context, target netip.AddrPort, codec Codec, payloadType uint8, samples []int16) error {
	if !target.IsValid() {
		return fmt.Errorf("invalid RTP target")
	}
	if len(samples) == 0 {
		return nil
	}
	started := time.Now()
	frameCount := (len(samples) + audioPacketSamples - 1) / audioPacketSamples
	for frameIndex := 0; frameIndex < frameCount; frameIndex++ {
		deadline := started.Add(time.Duration(frameIndex) * audioPacketTime)
		now := time.Now()
		if late := now.Sub(deadline); late > 5*audioPacketTime {
			skipped := min(int(late/audioPacketTime), frameCount-frameIndex)
			s.mu.Lock()
			s.timestamp += uint32(skipped * audioPacketSamples)
			s.stats.SkippedFrames += skipped
			s.stats.Discontinuities++
			s.mu.Unlock()
			frameIndex += skipped - 1
			continue
		}
		if wait := deadline.Sub(now); wait > 0 {
			if err := waitContext(ctx, wait); err != nil {
				return err
			}
		}
		offset := frameIndex * audioPacketSamples
		end := min(offset+audioPacketSamples, len(samples))
		frame := make([]int16, audioPacketSamples)
		copy(frame, samples[offset:end])
		if err := s.SendAudioFrame(target, codec, payloadType, frame, offset == 0); err != nil {
			return err
		}
	}
	return waitUntilContext(ctx, started.Add(time.Duration(frameCount)*audioPacketTime))
}

// SendAudioFrame immediately packetizes and sends one mono 8 kHz PCM frame.
// Callers are responsible for pacing successive frames.
func (s *Sender) SendAudioFrame(target netip.AddrPort, codec Codec, payloadType uint8, samples []int16, marker bool) error {
	if !target.IsValid() {
		return fmt.Errorf("invalid RTP target")
	}
	frame := make([]int16, audioPacketSamples)
	copy(frame, samples)
	name := "PCMA"
	if codec == PCMU {
		name = "PCMU"
	}
	s.mu.Lock()
	packet := &pionrtp.Packet{Header: pionrtp.Header{
		Version: 2, PayloadType: payloadType, SequenceNumber: s.sequence,
		Timestamp: s.timestamp, SSRC: s.ssrc, Marker: marker,
	}, Payload: mediaaudio.EncodeG711(frame, name)}
	s.sequence++
	s.timestamp += audioPacketSamples
	s.mu.Unlock()
	s.mu.Lock()
	s.stats.PacketsAttempted++
	s.mu.Unlock()
	err := s.receiver.writePacket(packet, target)
	s.mu.Lock()
	if err != nil {
		s.stats.FailedSends++
	} else {
		s.stats.PacketsSent++
		s.stats.Duration += audioPacketTime
	}
	s.recordLocked(packet, target, err)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (s *Sender) SendDTMF(ctx context.Context, target netip.AddrPort, payloadType uint8, digits string, interval time.Duration) error {
	if payloadType > 127 {
		return fmt.Errorf("invalid telephone-event payload type")
	}
	if interval == 0 {
		interval = 100 * time.Millisecond
	}
	for digitIndex, digit := range strings.ToUpper(digits) {
		event, ok := dtmfEventID(digit)
		if !ok {
			return fmt.Errorf("invalid DTMF digit %q", digit)
		}
		s.mu.Lock()
		eventTimestamp := s.timestamp
		s.mu.Unlock()
		started := time.Now()
		for packetIndex, duration := range []uint16{160, 320, 480, 640, 640, 640} {
			if err := waitUntilContext(ctx, started.Add(time.Duration(packetIndex)*audioPacketTime)); err != nil {
				return err
			}
			end := packetIndex >= 3
			payload := []byte{event, 10, byte(duration >> 8), byte(duration)}
			if end {
				payload[1] |= 0x80
			}
			s.mu.Lock()
			packet := &pionrtp.Packet{Header: pionrtp.Header{
				Version: 2, PayloadType: payloadType, SequenceNumber: s.sequence,
				Timestamp: eventTimestamp, SSRC: s.ssrc, Marker: packetIndex == 0,
			}, Payload: payload}
			s.sequence++
			s.mu.Unlock()
			s.mu.Lock()
			s.stats.PacketsAttempted++
			s.mu.Unlock()
			err := s.receiver.writePacket(packet, target)
			s.mu.Lock()
			if err != nil {
				s.stats.FailedSends++
			} else {
				s.stats.PacketsSent++
			}
			s.recordLocked(packet, target, err)
			s.mu.Unlock()
			if err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.timestamp += 800
		s.stats.Duration += 100 * time.Millisecond
		s.mu.Unlock()
		if digitIndex < len([]rune(digits))-1 {
			if err := waitUntilContext(ctx, started.Add(5*audioPacketTime+interval)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Sender) recordLocked(packet *pionrtp.Packet, target netip.AddrPort, sendErr error) {
	event := PacketEvent{
		Version: packet.Version, Marker: packet.Marker, PayloadType: packet.PayloadType,
		SequenceNumber: packet.SequenceNumber, Timestamp: packet.Timestamp, SSRC: packet.SSRC,
		ArrivalTime: time.Now(), Source: target.String(), Attempted: true, Successful: sendErr == nil,
	}
	if sendErr != nil {
		event.Error = sendErr.Error()
	}
	s.events = append(s.events, event)
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitUntilContext(ctx context.Context, deadline time.Time) error {
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil
	}
	return waitContext(ctx, wait)
}

var fallbackRTPIdentifier atomic.Uint32

func randomUint32(reader io.Reader) uint32 {
	var data [4]byte
	if _, err := io.ReadFull(reader, data[:]); err != nil {
		return fallbackRTPIdentifier.Add(1)
	}
	return binary.BigEndian.Uint32(data[:])
}

func dtmfEventID(digit rune) (byte, bool) {
	switch {
	case digit >= '0' && digit <= '9':
		return byte(digit - '0'), true
	case digit == '*':
		return 10, true
	case digit == '#':
		return 11, true
	case digit >= 'A' && digit <= 'D':
		return byte(digit-'A') + 12, true
	default:
		return 0, false
	}
}
