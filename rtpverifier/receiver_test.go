package rtpverifier

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	mediaaudio "github.com/subiz/sutel/audio"

	pionrtp "github.com/pion/rtp"
)

func TestReceiverReordersDeduplicatesAndFillsGap(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	payload := mediaaudio.EncodeG711(make([]int16, 160), "PCMA")
	sendPacket(t, sender, receiver.Addr(), packet(10, 1000, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(12, 1320, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(11, 1160, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(11, 1160, 8, 99, payload))

	result := stopReceiver(t, receiver)
	if result.Stats.PacketsReceived != 3 {
		t.Fatalf("packets=%d want 3", result.Stats.PacketsReceived)
	}
	if result.Stats.OutOfOrderPackets != 1 || result.Stats.DuplicatePackets != 1 || result.Stats.MissingPackets != 0 {
		t.Fatalf("unexpected stats: %+v", result.Stats)
	}
	if len(result.PCM) != 480 {
		t.Fatalf("PCM length=%d want 480", len(result.PCM))
	}
}

func TestReceiverAppliesReorderWindow(t *testing.T) {
	receiver := newTestReceiver(t, Config{ReorderWindow: 2})
	sender := newSender(t)
	payload := mediaaudio.EncodeG711(make([]int16, 160), "PCMA")
	sendPacket(t, sender, receiver.Addr(), packet(10, 1000, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(13, 1480, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(12, 1320, 8, 99, payload))
	sendPacket(t, sender, receiver.Addr(), packet(9, 840, 8, 99, payload))
	result := stopReceiver(t, receiver)
	if result.Stats.PacketsReceived != 3 || result.Stats.OutOfOrderPackets != 2 || result.Stats.IgnoredPackets != 1 {
		t.Fatalf("stats=%+v", result.Stats)
	}
}

func TestReceiverCapsRetainedPacketState(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	source := netip.MustParseAddrPort("127.0.0.1:5000")
	arrival := time.Now()
	for index := 0; index < maxRetainedRTPPackets+20; index++ {
		receiver.accept(packet(uint16(index+1), uint32(index*160), 8, 99, []byte{0xd5}), source, arrival)
	}
	receiver.mu.Lock()
	packets, events, seen := len(receiver.packets), len(receiver.events), len(receiver.seen)
	stats := receiver.stats
	receiver.mu.Unlock()
	if packets != maxRetainedRTPPackets || events != maxRetainedRTPPackets || seen != maxRetainedRTPPackets {
		t.Fatalf("packets=%d events=%d seen=%d", packets, events, seen)
	}
	if stats.IgnoredPackets != 20 || stats.Discontinuities != 1 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestReceiverTimestampGapAndDiscontinuity(t *testing.T) {
	receiver := newTestReceiver(t, Config{MaxSynthesizedGap: 100 * time.Millisecond})
	sender := newSender(t)
	payload := make([]byte, 160)
	sendPacket(t, sender, receiver.Addr(), packet(1, 100, 8, 7, payload))
	sendPacket(t, sender, receiver.Addr(), packet(2, 420, 8, 7, payload))
	sendPacket(t, sender, receiver.Addr(), packet(3, 10000, 8, 7, payload))
	result := stopReceiver(t, receiver)
	if len(result.PCM) != 640 {
		t.Fatalf("PCM length=%d want 640", len(result.PCM))
	}
	if result.Stats.Discontinuities != 1 {
		t.Fatalf("discontinuities=%d want 1", result.Stats.Discontinuities)
	}
}

func TestReceiverDiagnosesConflictingTimestampOverlap(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	first := mediaaudio.EncodeG711(make([]int16, 160), "PCMA")
	conflictingSamples := make([]int16, 160)
	for index := range conflictingSamples {
		conflictingSamples[index] = 12000
	}
	second := mediaaudio.EncodeG711(conflictingSamples, "PCMA")
	sendPacket(t, sender, receiver.Addr(), packet(1, 1000, 8, 99, first))
	sendPacket(t, sender, receiver.Addr(), packet(2, 1000, 8, 99, second))
	result := stopReceiver(t, receiver)
	if len(result.PCM) != 160 || result.Stats.Discontinuities != 1 || result.Stats.IgnoredPackets != 1 || result.Stats.ConflictingPackets != 1 {
		t.Fatalf("PCM=%d stats=%+v", len(result.PCM), result.Stats)
	}
	if want := mediaaudio.DecodeG711(first, "PCMA"); !slices.Equal(result.PCM, want) {
		t.Fatal("overlap replaced the first packet")
	}
}

func TestReceiverIgnoresForeignSSRCAndSource(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	first := newSender(t)
	second := newSender(t)
	payload := make([]byte, 160)
	sendPacket(t, first, receiver.Addr(), packet(1, 100, 8, 10, payload))
	sendPacket(t, first, receiver.Addr(), packet(2, 260, 8, 11, payload))
	sendPacket(t, second, receiver.Addr(), packet(3, 420, 8, 10, payload))
	result := stopReceiver(t, receiver)
	if result.Stats.PacketsReceived != 1 || result.Stats.ForeignSSRC != 1 || result.Stats.IgnoredPackets != 2 {
		t.Fatalf("unexpected stats: %+v", result.Stats)
	}
}

func TestReceiverRFC4733Deduplication(t *testing.T) {
	receiver := newTestReceiver(t, Config{TelephoneEventPayload: 110})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(1, 100, 8, 42, make([]byte, 160)))
	sequence := uint16(2)
	for _, item := range []struct {
		duration uint16
		end      bool
	}{{80, false}, {160, false}, {240, true}, {240, true}, {240, true}} {
		payload := []byte{11, 10, byte(item.duration >> 8), byte(item.duration)}
		if item.end {
			payload[1] |= 0x80
		}
		sendPacket(t, sender, receiver.Addr(), packet(sequence, 900, 110, 42, payload))
		sequence++
	}
	result := stopReceiver(t, receiver)
	if len(result.DTMF) != 1 {
		t.Fatalf("DTMF events=%d want 1: %+v", len(result.DTMF), result.DTMF)
	}
	if result.DTMF[0].Digit != "#" || result.DTMF[0].Duration != 30*time.Millisecond {
		t.Fatalf("unexpected DTMF: %+v", result.DTMF[0])
	}
}

func TestReceiverRFC4733KeepsGreatestRepeatedEndDuration(t *testing.T) {
	receiver := newTestReceiver(t, Config{TelephoneEventPayload: 110})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(1, 100, 8, 42, make([]byte, 160)))
	for index, duration := range []uint16{160, 240, 320} {
		payload := []byte{5, 0x80 | 10, byte(duration >> 8), byte(duration)}
		sendPacket(t, sender, receiver.Addr(), packet(uint16(index+2), 900, 110, 42, payload))
	}
	result := stopReceiver(t, receiver)
	if len(result.DTMF) != 1 || result.DTMF[0].Duration != 40*time.Millisecond {
		t.Fatalf("DTMF=%+v", result.DTMF)
	}
}

func TestReceiverIncompleteDTMFIsNotEmitted(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(1, 100, 8, 42, make([]byte, 160)))
	sendPacket(t, sender, receiver.Addr(), packet(2, 900, 101, 42, []byte{5, 10, 0, 160}))
	result := stopReceiver(t, receiver)
	if len(result.DTMF) != 0 || result.Stats.IncompleteDTMF != 1 {
		t.Fatalf("incomplete DTMF emitted: %+v", result.DTMF)
	}
}

func TestWrapArithmeticDoesNotUseZeroAsInitializationSentinel(t *testing.T) {
	if got := unwrap16(65535, 0, true); got != -1 {
		t.Fatalf("unwrap16=%d want -1", got)
	}
	if got := unwrap32(^uint32(0), 0, true); got != -1 {
		t.Fatalf("unwrap32=%d want -1", got)
	}
}

func TestReceiverRetainsNewTailOfPartialOverlap(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	first := mediaaudio.EncodeG711(make([]int16, 160), "PCMA")
	tailSamples := make([]int16, 160)
	for index := range tailSamples {
		tailSamples[index] = 1000
	}
	second := mediaaudio.EncodeG711(tailSamples, "PCMA")
	sendPacket(t, sender, receiver.Addr(), packet(1, 1000, 8, 42, first))
	sendPacket(t, sender, receiver.Addr(), packet(2, 1080, 8, 42, second))
	result := stopReceiver(t, receiver)
	if len(result.PCM) != 240 || result.Stats.ConflictingPackets != 1 {
		t.Fatalf("PCM=%d stats=%+v", len(result.PCM), result.Stats)
	}
}

func TestTelephoneEventBeforeAudioDoesNotPolluteAudioSequenceSpace(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(500, 900, 101, 99, []byte{5, 0x80 | 10, 0, 160}))
	sendPacket(t, sender, receiver.Addr(), packet(1, 1000, 8, 42, make([]byte, 160)))
	sendPacket(t, sender, receiver.Addr(), packet(2, 1160, 8, 42, make([]byte, 160)))
	result := stopReceiver(t, receiver)
	if result.Stats.MissingPackets != 0 || len(result.DTMF) != 0 {
		t.Fatalf("stats=%+v DTMF=%+v", result.Stats, result.DTMF)
	}
}

func TestReceiverLastValuesDoNotMoveBackwardOnReorder(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(10, 1000, 8, 42, make([]byte, 160)))
	sendPacket(t, sender, receiver.Addr(), packet(12, 1320, 8, 42, make([]byte, 160)))
	sendPacket(t, sender, receiver.Addr(), packet(11, 1160, 8, 42, make([]byte, 160)))
	result := stopReceiver(t, receiver)
	if result.Stats.LastSequence != 12 || result.Stats.LastTimestamp != 1320 {
		t.Fatalf("stats=%+v", result.Stats)
	}
}

func TestReceiverAllowsCompatibleNegotiationAfterEarlyMedia(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(1, 1000, 8, 42, make([]byte, 160)))
	time.Sleep(10 * time.Millisecond)
	if err := receiver.Configure(map[uint8]Codec{8: PCMA}, 101, netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	result := stopReceiver(t, receiver)
	if result.Stats.PacketsReceived != 1 {
		t.Fatalf("stats=%+v", result.Stats)
	}
}

func TestReceiverFailsExplicitlyOnMidCallCodecSwitch(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	sendPacket(t, sender, receiver.Addr(), packet(1, 1000, 8, 42, make([]byte, 160)))
	sendPacket(t, sender, receiver.Addr(), packet(2, 1160, 0, 42, make([]byte, 160)))
	time.Sleep(10 * time.Millisecond)
	result, err := receiver.Stop(context.Background(), 5*time.Millisecond)
	if !errors.Is(err, ErrCodecSwitch) || result.Stats.CodecSwitches != 1 {
		t.Fatalf("stats=%+v err=%v", result.Stats, err)
	}
}

func TestReceiverMalformedAndUnknownPackets(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	if _, err := sender.WriteToUDPAddrPort([]byte{1, 2, 3}, receiver.Addr()); err != nil {
		t.Fatal(err)
	}
	sendPacket(t, sender, receiver.Addr(), packet(1, 100, 96, 1, []byte{1, 2}))
	result := stopReceiver(t, receiver)
	if result.Stats.MalformedPackets != 1 || result.Stats.IgnoredPackets != 2 {
		t.Fatalf("unexpected stats: %+v", result.Stats)
	}
}

func TestReceiverStopIsBoundedWhileRTPContinues(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := newSender(t)
	payload := mediaaudio.EncodeG711(make([]int16, 160), "PCMA")
	sendPacket(t, sender, receiver.Addr(), packet(1, 160, 8, 42, payload))

	stopSending := make(chan struct{})
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		sequence := uint16(2)
		for {
			select {
			case <-stopSending:
				return
			case <-ticker.C:
				data, err := packet(sequence, uint32(sequence)*160, 8, 42, payload).Marshal()
				if err == nil {
					_, _ = sender.WriteToUDPAddrPort(data, receiver.Addr())
				}
				sequence++
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		current, err := receiver.Result()
		if err != nil {
			t.Fatal(err)
		}
		if current.Stats.PacketsReceived > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(stopSending)
			<-senderDone
			t.Fatal("receiver did not accept the initial RTP packet")
		}
		time.Sleep(time.Millisecond)
	}

	type stopResult struct {
		result Result
		err    error
	}
	resultChannel := make(chan stopResult, 1)
	go func() {
		result, err := receiver.Stop(context.Background(), 50*time.Millisecond)
		resultChannel <- stopResult{result: result, err: err}
	}()
	select {
	case stopped := <-resultChannel:
		close(stopSending)
		<-senderDone
		if stopped.err != nil || stopped.result.Stats.PacketsReceived == 0 {
			t.Fatalf("result=%+v err=%v", stopped.result.Stats, stopped.err)
		}
	case <-time.After(2 * time.Second):
		close(stopSending)
		<-senderDone
		_ = receiver.Close()
		t.Fatal("Receiver.Stop did not enforce a total drain bound")
	}
}

func newTestReceiver(t *testing.T, config Config) *Receiver {
	t.Helper()
	config.LocalIP = netip.MustParseAddr("127.0.0.1")
	receiver, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	return receiver
}

func newSender(t *testing.T) *net.UDPConn {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func packet(sequence uint16, timestamp uint32, payloadType uint8, ssrc uint32, payload []byte) pionrtp.Packet {
	return pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: payloadType, SequenceNumber: sequence, Timestamp: timestamp, SSRC: ssrc}, Payload: payload}
}

func sendPacket(t *testing.T, sender *net.UDPConn, target netip.AddrPort, packet pionrtp.Packet) {
	t.Helper()
	data, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.WriteToUDPAddrPort(data, target); err != nil {
		t.Fatal(err)
	}
}

func stopReceiver(t *testing.T, receiver *Receiver) Result {
	t.Helper()
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := receiver.Stop(ctx, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
