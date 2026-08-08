package rtpverifier

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"
)

func TestSenderPacketizesAndPacesAudio(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	peer := newSender(t)
	target := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	sender := NewSender(receiver)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := sender.Play(ctx, target, PCMA, 8, make([]int16, 4000)); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < 480*time.Millisecond || elapsed > 560*time.Millisecond {
		t.Fatalf("audio pacing elapsed=%s, want [480ms,560ms]", elapsed)
	}
	packets := readRTPPackets(t, peer, 25)
	for index, packet := range packets {
		if packet.PayloadType != 8 || len(packet.Payload) != 160 {
			t.Fatalf("packet %d: %+v payload=%d", index, packet.Header, len(packet.Payload))
		}
		if index > 0 && (packet.SequenceNumber != packets[index-1].SequenceNumber+1 || packet.Timestamp != packets[index-1].Timestamp+160) {
			t.Fatalf("non-contiguous packets: prev=%+v current=%+v", packets[index-1].Header, packet.Header)
		}
	}
	if stats := sender.Stats(); stats.PacketsAttempted != 25 || stats.PacketsSent != 25 || stats.Duration != 500*time.Millisecond {
		t.Fatalf("stats=%+v", stats)
	}
	if events := sender.Events(); len(events) != 25 || events[0].PayloadType != 8 || !events[0].Attempted || !events[0].Successful {
		t.Fatalf("events=%+v", events)
	}
}

func TestSenderRecordsFailedAttempts(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	sender := NewSender(receiver)
	if err := receiver.Close(); err != nil {
		t.Fatal(err)
	}
	err := sender.SendAudioFrame(netip.MustParseAddrPort("127.0.0.1:9"), PCMA, 8, make([]int16, 160), true)
	if err == nil {
		t.Fatal("send to closed media socket succeeded")
	}
	stats := sender.Stats()
	events := sender.Events()
	if stats.PacketsAttempted != 1 || stats.PacketsSent != 0 || stats.FailedSends != 1 || len(events) != 1 || events[0].Successful || events[0].Error == "" {
		t.Fatalf("stats=%+v events=%+v", stats, events)
	}
}

type failedRandomReader struct{}

func (failedRandomReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSenderIdentifierFallbackDoesNotPanic(t *testing.T) {
	first := newSenderWithReader(nil, failedRandomReader{})
	second := newSenderWithReader(nil, failedRandomReader{})
	if first.ssrc == second.ssrc {
		t.Fatalf("fallback SSRC collision: %d", first.ssrc)
	}
}

func TestSenderRFC4733UsesDynamicPayloadAndRepeatsEnd(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	peer := newSender(t)
	target := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	sender := NewSender(receiver)
	if err := sender.SendDTMF(context.Background(), target, 112, "#", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	packets := readRTPPackets(t, peer, 6)
	timestamp := packets[0].Timestamp
	for index, packet := range packets {
		if packet.PayloadType != 112 || packet.Timestamp != timestamp || packet.Payload[0] != 11 {
			t.Fatalf("packet %d: %+v payload=%v", index, packet.Header, packet.Payload)
		}
	}
	for _, index := range []int{3, 4, 5} {
		if packets[index].Payload[1]&0x80 == 0 {
			t.Fatalf("packet %d does not have end bit", index)
		}
	}
}

func TestReceiverConfigureNegotiatedPayload(t *testing.T) {
	receiver := newTestReceiver(t, Config{})
	if err := receiver.Configure(map[uint8]Codec{96: PCMU}, 112, netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	peer := newSender(t)
	sendPacket(t, peer, receiver.Addr(), packet(1, 100, 8, 1, make([]byte, 160)))
	sendPacket(t, peer, receiver.Addr(), packet(2, 260, 96, 1, make([]byte, 160)))
	result := stopReceiver(t, receiver)
	if result.Stats.PacketsReceived != 1 || result.Stats.Codec != PCMU || result.Stats.IgnoredPackets != 1 {
		t.Fatalf("result=%+v", result.Stats)
	}
}

func readRTPPackets(t *testing.T, connection *net.UDPConn, count int) []pionrtp.Packet {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result := make([]pionrtp.Packet, 0, count)
	buffer := make([]byte, 2048)
	for len(result) < count {
		length, _, err := connection.ReadFromUDPAddrPort(buffer)
		if err != nil {
			t.Fatal(err)
		}
		var packet pionrtp.Packet
		if err := packet.Unmarshal(buffer[:length]); err != nil {
			t.Fatal(err)
		}
		result = append(result, packet)
	}
	return result
}
