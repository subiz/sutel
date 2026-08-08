package sutel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"
	mediaaudio "github.com/subiz/sutel/audio"
)

// The scripted peer is deliberately independent of internal/sip. These black-box
// tests decide behavior from literal start lines and a minimal test-only
// header splitter.
type rawSIPMessage struct {
	start   string
	headers map[string][]string
	body    string
	raw     string
}

func TestUDPOutboundAnswerPCMAAndPCMU(t *testing.T) {
	for _, codec := range []Codec{PCMA, PCMU} {
		t.Run(codec.String(), func(t *testing.T) {
			carrier := newUDPTestCarrier(t, Config{})
			session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
				From: "100", To: "1900", Codecs: []Codec{codec}, Behavior: Answer{}, Timeout: 2 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			peer := listenUDP(t)
			rtpPeer := listenUDP(t)
			invite := outboundInvite(session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{codec.PayloadType()}, 101, "call-answer")
			writeRaw(t, peer, session.SIPAddr(), invite)
			responses := readUntilStatus(t, peer, 200)
			if !containsStatus(responses, 100) || !containsStatus(responses, 180) {
				t.Fatalf("missing provisional responses: %+v", starts(responses))
			}
			ok := responses[len(responses)-1]
			if !strings.Contains(ok.body, fmt.Sprintf("m=audio %d RTP/AVP %d", sdpPort(ok.body), codec.PayloadType())) {
				t.Fatalf("unexpected SDP answer:\n%s", ok.body)
			}
			writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), "call-answer", "from-tag", header(ok, "to"), 1, ""))
			writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), "call-answer", "from-tag", header(ok, "to"), 2, ""))
			if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
				t.Fatalf("BYE response=%s", response.start)
			}
			result, err := session.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if !result.Outcome.Established || result.Outcome.TerminatedBy != SystemUnderTest || result.Media.AudioCodec != codec || result.SIP.RequestsReceived < 3 {
				t.Fatalf("result=%+v SIP=%+v media=%+v", result.Outcome, result.SIP, result.Media)
			}
		})
	}
}

func TestUDPOutboundAnswerUsesDynamicG711Payload(t *testing.T) {
	for _, item := range []struct {
		codec   Codec
		payload uint8
	}{{PCMA, 96}, {PCMU, 97}} {
		t.Run(item.codec.String(), func(t *testing.T) {
			playbackFile := writeShortWAV(t, make([]int16, 3*160))
			carrier := newUDPTestCarrier(t, Config{})
			session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
				Behavior: Answer{Playback: &AudioPlayback{File: playbackFile}},
				Codecs:   []Codec{item.codec}, Timeout: 2 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			peer, rtpPeer := listenUDP(t), listenUDP(t)
			peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
			invite := outboundInviteWithMappings(
				session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(),
				[]uint8{item.payload}, map[uint8]Codec{item.payload: item.codec}, -1, "dynamic-answer-"+item.codec.String(),
			)
			writeRaw(t, peer, session.SIPAddr(), invite)
			responses := readUntilStatus(t, peer, 200)
			ok := responses[len(responses)-1]
			mapping := fmt.Sprintf("a=rtpmap:%d %s/8000", item.payload, item.codec)
			if !strings.Contains(ok.body, fmt.Sprintf("RTP/AVP %d", item.payload)) || !strings.Contains(ok.body, mapping) {
				t.Fatalf("dynamic mapping missing from SDP answer:\n%s", ok.body)
			}
			callID := "dynamic-answer-" + item.codec.String()
			writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, callID, "from-tag", header(ok, "to"), 1, ""))
			packets := readRTPFromPeer(t, rtpPeer, 3, time.Second)
			for _, packet := range packets {
				if packet.PayloadType != item.payload {
					t.Fatalf("RTP payload=%d want %d", packet.PayloadType, item.payload)
				}
			}
			bye := readRaw(t, peer, time.Second)
			if !strings.HasPrefix(bye.start, "BYE ") {
				t.Fatalf("expected BYE, got %s", bye.start)
			}
			writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
			result, err := session.Wait()
			if err != nil || !result.Outcome.Established || result.Media.AudioCodec != item.codec || result.RTP.PacketsSent == 0 {
				t.Fatalf("outcome=%+v media=%+v RTP=%+v err=%v", result.Outcome, result.Media, result.RTP, err)
			}
		})
	}
}

func TestUDPIdleCarrierAnswersOPTIONSWithoutConsumingCallSlot(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	peer := listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	to := fmt.Sprintf("<sip:sutel@%s>", carrier.sipAddr)
	options := baseRequest("OPTIONS", carrier.sipAddr, peerAddress, "idle-options", "from-tag", to, 1, "OPTIONS", "z9hG4bK-options")
	options = strings.Replace(options, fmt.Sprintf("Via: SIP/2.0/UDP %s;", peerAddress), "Via: SIP/2.0/UDP 127.0.0.2:9999;", 1)
	writeRaw(t, peer, carrier.sipAddr, options)
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 200 OK" || header(response, "call-id") != "idle-options" {
		t.Fatalf("response=%s Call-ID=%q", response.start, header(response, "call-id"))
	}
	via := header(response, "via")
	if !strings.Contains(via, fmt.Sprintf("rport=%d", peerAddress.Port())) || !strings.Contains(via, "received=127.0.0.1") {
		t.Fatalf("Via=%q", via)
	}

	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	rtpPeer := listenUDP(t)
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "after-options"))
	busy := readRaw(t, peer, time.Second)
	if busy.start != "SIP/2.0 486 Busy Here" {
		t.Fatalf("response=%s", busy.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "after-options", "from-tag", header(busy, "to"), 1, "ACK", "z9hG4bK-test"))
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPackageExpectOutboundCallCompletesCall(t *testing.T) {
	call, err := ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(call.Close)
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, call.SIPAddr(), outboundInvite(call.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "root-test-helper"))
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 486 Busy Here" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, call.SIPAddr(), baseRequest("ACK", call.SIPAddr(), peerAddress, "root-test-helper", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := call.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.InviteFinalStatus != 486 {
		t.Fatalf("outcome=%+v", result.Outcome)
	}
}

func TestUDPCloseEstablishedCarrierStopsOwnedResources(t *testing.T) {
	carrier, err := NewCarrier(Config{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "close-established"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "close-established", "from-tag", header(ok, "to"), 1, ""))
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("INFO", session.SIPAddr(), peerAddress, "close-established", "from-tag", header(ok, "to"), 2, "Signal=5\r\nDuration=160\r\n"))
	if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
		t.Fatalf("INFO response=%s", response.start)
	}

	closed := make(chan error, 1)
	go func() { closed <- carrier.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Carrier.Close did not finish")
	}
	if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err=%v, want context.Canceled", err)
	}
	if _, err := carrier.conn.WriteToUDPAddrPort([]byte("closed"), peerAddress); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SIP socket write err=%v, want net.ErrClosed", err)
	}
}

func TestUDPOutboundSutelHangsUp(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{HangupAfter: 100 * time.Millisecond}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "call-sutel-bye"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "call-sutel-bye", "from-tag", header(ok, "to"), 1, ""))
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(bye.start, "BYE ") {
		t.Fatalf("expected BYE, got %s", bye.start)
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil || result.Outcome.TerminatedBy != Sutel {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPOutboundNetworkLossSilentlyCleansUp(t *testing.T) {
	const lossAfter = time.Hour
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{}, clock)
	call, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: NetworkLoss{After: lossAfter}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(call.Close)
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, call.SIPAddr(), outboundInvite(call.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "network-loss"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, call.SIPAddr(), dialogRequest("ACK", call.SIPAddr(), peerAddr, "network-loss", "from-tag", header(ok, "to"), 1, ""))
	clock.waitForActiveTimer(t, lossAfter)
	clock.Advance(lossAfter)

	result, err := call.Wait()
	if err != nil || result.Outcome.InviteFinalStatus != 200 || !result.Outcome.Established || result.Outcome.TerminatedBy != NoParty {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
	for _, event := range result.Events() {
		if event.Layer == SIPLayer && event.Direction == Sent && event.Type == "BYE" {
			t.Fatal("network loss sent BYE")
		}
	}
	if message, readErr := readRawError(peer, 100*time.Millisecond); readErr == nil {
		t.Fatalf("network loss sent unexpected SIP: %s", message.start)
	}
	if _, writeErr := call.carrier.conn.WriteToUDPAddrPort([]byte("closed"), peerAddr); !errors.Is(writeErr, net.ErrClosed) {
		t.Fatalf("SIP socket write error=%v, want net.ErrClosed", writeErr)
	}
}

func TestUDPOutboundResponseAndDialogPreserveRecordRoute(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{HangupAfter: 100 * time.Millisecond}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	routes := []string{
		fmt.Sprintf("<sip:proxy@%s;lr>", peerAddress),
		fmt.Sprintf("<sip:edge@%s;lr>", peerAddress),
	}
	invite := outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "record-route")
	invite = strings.Replace(invite, "Max-Forwards: 70\r\n", "Record-Route: "+routes[0]+"\r\nRecord-Route: "+routes[1]+"\r\nMax-Forwards: 70\r\n", 1)
	writeRaw(t, peer, session.SIPAddr(), invite)
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	if got := ok.headers["record-route"]; !slices.Equal(got, routes) {
		t.Fatalf("Record-Route=%v want %v", got, routes)
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "record-route", "from-tag", header(ok, "to"), 1, ""))
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(bye.start, "BYE ") || !slices.Equal(bye.headers["route"], routes) {
		t.Fatalf("BYE=%s Route=%v", bye.start, bye.headers["route"])
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPOutboundNegativeBehaviors(t *testing.T) {
	tests := []struct {
		name     string
		behavior OutboundBehavior
		status   int
	}{{"busy", Busy{}, 486}, {"reject", Reject{}, 603}, {"not-found", NotFound{}, 404}, {"unavailable", ServiceUnavailable{}, 503}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			carrier := newUDPTestCarrier(t, Config{})
			session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: test.behavior, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			peer, rtpPeer := listenUDP(t), listenUDP(t)
			callID := "call-" + test.name
			writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, callID))
			response := readRaw(t, peer, time.Second)
			if response.start != fmt.Sprintf("SIP/2.0 %d %s", test.status, statusReason(test.status)) {
				t.Fatalf("response=%s", response.start)
			}
			writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), callID, "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
			result, err := session.Wait()
			if err != nil || result.Outcome.InviteFinalStatus != test.status || result.Outcome.Established {
				t.Fatalf("result=%+v err=%v", result.Outcome, err)
			}
		})
	}
}

func TestUDPUASFinalResponseRetransmitsUntilACK(t *testing.T) {
	const (
		t1 = 10 * time.Second
		t2 = 25 * time.Second
	)
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: t2}, clock)
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "final-backoff"))
	responses := []rawSIPMessage{readRaw(t, peer, time.Second)}
	for _, interval := range []time.Duration{t1, 2 * t1, t2, t2} {
		clock.waitForActiveTimer(t, interval)
		clock.Advance(interval)
		responses = append(responses, readRaw(t, peer, time.Second))
	}
	for index := 1; index < len(responses); index++ {
		if responses[index].raw != responses[0].raw {
			t.Fatalf("response %d changed across retransmission", index)
		}
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "final-backoff", "from-tag", header(responses[0], "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || result.SIP.Retransmissions != 4 {
		t.Fatalf("SIP=%+v err=%v", result.SIP, err)
	}
	wantSchedule := []time.Duration{t1, 2 * t1, t2, t2, t2}
	if got := clock.scheduledDurations(); !slices.Equal(got, wantSchedule) {
		t.Fatalf("retransmission schedule=%v want %v", got, wantSchedule)
	}
}

func TestUDPOutboundBusyDoesNotNegotiateCodec(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Busy{}, Codecs: []Codec{PCMA}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{0}, -1, "busy-with-pcmu"))
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 486 Busy Here" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "busy-with-pcmu", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || result.Outcome.InviteFinalStatus != 486 || result.Outcome.Established {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPOutboundNoAnswerCancel(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: NoAnswer{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "call-cancel"))
	readUntilStatus(t, peer, 180)
	inviteTo := fmt.Sprintf("<sip:1900@%s:%d>", session.SIPAddr().Addr(), session.SIPAddr().Port())
	cancel := baseRequest("CANCEL", session.SIPAddr(), peerAddr, "call-cancel", "from-tag", inviteTo, 1, "CANCEL", "z9hG4bK-test")
	writeRaw(t, peer, session.SIPAddr(), cancel)
	finals := []rawSIPMessage{readRaw(t, peer, time.Second), readRaw(t, peer, time.Second)}
	var terminated rawSIPMessage
	for _, response := range finals {
		if strings.HasPrefix(response.start, "SIP/2.0 487 ") {
			terminated = response
		}
	}
	if terminated.start == "" {
		t.Fatalf("missing 487: %+v", starts(finals))
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "call-cancel", "from-tag", header(terminated, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || !result.Outcome.Canceled || result.Outcome.InviteFinalStatus != 487 {
		t.Fatalf("result=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPOutboundTimeoutAcceptsRetransmission(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Timeout{}, Timeout: 220 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	invite := outboundInvite(session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "call-timeout")
	writeRaw(t, peer, session.SIPAddr(), invite)
	writeRaw(t, peer, session.SIPAddr(), invite)
	result, err := session.Wait()
	if err != nil || result.Outcome.InviteFinalStatus != 0 || result.SIP.ResponsesSent != 0 || result.SIP.RequestsReceived < 2 {
		t.Fatalf("result=%+v SIP=%+v err=%v", result.Outcome, result.SIP, err)
	}
}

func TestUDPOutboundNoCommonCodec(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Codecs: []Codec{PCMA}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{0}, -1, "call-488"))
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 488 Not Acceptable Here" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "call-488", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if !errors.Is(err, ErrNegotiation) || result.Outcome.InviteFinalStatus != 488 {
		t.Fatalf("result=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPOutboundIgnoresMalformedTrafficAndRejectsOutOfDialogINFO(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{MaxSIPMessageBytes: 1024})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), "not a SIP datagram")
	writeRaw(t, peer, session.SIPAddr(), strings.Repeat("x", 1025))
	info := dialogRequest("INFO", session.SIPAddr(), peerAddr, "outside", "from-tag", fmt.Sprintf("<sip:1900@%s>;tag=none", session.SIPAddr()), 1, "Signal=5\r\nDuration=160\r\n")
	writeRaw(t, peer, session.SIPAddr(), info)
	if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 481 Call/Transaction Does Not Exist" {
		t.Fatalf("INFO response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "after-malformed"))
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 486 Busy Here" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "after-malformed", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || result.SIP.MalformedMessages != 2 {
		t.Fatalf("SIP=%+v err=%v", result.SIP, err)
	}
}

func TestUDPInboundAnswerSutelAndSUTHangup(t *testing.T) {
	t.Run("Sutel sends BYE", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMU,
			RingTimeout: time.Second, CallDuration: 80 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		invite := answerInboundInvite(t, peer, rtpPeer, PCMU, false)
		ack := readRaw(t, peer, time.Second)
		if !strings.HasPrefix(ack.start, "ACK ") {
			t.Fatalf("expected ACK, got %s", ack.start)
		}
		bye := readRaw(t, peer, time.Second)
		if !strings.HasPrefix(bye.start, "BYE ") {
			t.Fatalf("expected BYE, got %s", bye.start)
		}
		writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
		result, err := session.Wait()
		if err != nil || !result.Outcome.Established || result.Outcome.TerminatedBy != Sutel || invite.start == "" {
			t.Fatalf("result=%+v err=%v", result.Outcome, err)
		}
	})

	t.Run("SUT sends BYE", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
		_ = readRaw(t, peer, time.Second)
		to := header(invite, "from")
		from := header(invite, "to") + ";tag=peer-tag"
		bye := baseRequestWithHeaders("BYE", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), header(invite, "call-id"), from, to, 2, "BYE", "z9hG4bK-peer-bye")
		writeRaw(t, peer, session.SIPAddr(), bye)
		if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
			t.Fatalf("response=%s", response.start)
		}
		result, err := session.Wait()
		if err != nil || result.Outcome.TerminatedBy != SystemUnderTest {
			t.Fatalf("result=%+v err=%v", result.Outcome, err)
		}
	})
}

func TestUDPInboundPlaysProvidedSampleWAV(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		Playback: &AudioPlayback{File: "testdata/sample.wav"}, RingTimeout: time.Second, CallDuration: 180 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	_ = readRaw(t, peer, time.Second)
	packetsDone := make(chan int, 1)
	go func() { packetsDone <- countRTP(rtpPeer, 150*time.Millisecond) }()
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	packets := <-packetsDone
	hasSentRTPEvent := false
	for _, event := range result.Events() {
		if event.Layer == RTPLayer && event.Direction == Sent {
			hasSentRTPEvent = true
			break
		}
	}
	if err != nil || packets < 3 || result.RTP.PacketsSent < 3 || result.RTP.SentDuration == 0 || !hasSentRTPEvent {
		t.Fatalf("packets=%d RTP=%+v err=%v", packets, result.RTP, err)
	}
}

func TestUDPInboundVerifiesAudioSentBySUT(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		ExpectAudio: &AudioExpectation{File: "testdata/sample.wav", MinSimilarity: 0.90, MinCoverage: 0.95},
		RingTimeout: time.Second, CallDuration: 120 * time.Millisecond, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
		t.Fatalf("expected ACK, got %s", ack.start)
	}
	target := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(invite.body)), uint16(sdpPort(invite.body)))
	samples, err := mediaaudio.Normalize8kMono("testdata/sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	sendAudioRTP(t, rtpPeer, target, PCMA, samples)
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil || result.Audio == nil || result.Audio.Similarity < 0.90 || result.Audio.Coverage < 0.95 {
		t.Fatalf("audio=%+v RTP=%+v err=%v", result.Audio, result.RTP, err)
	}
}

func TestUDPInboundPlaybackAndExpectedAudioAreFullDuplex(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		Playback:    &AudioPlayback{File: "testdata/sample.wav"},
		ExpectAudio: &AudioExpectation{File: "testdata/sample.wav", MinSimilarity: 0.90, MinCoverage: 0.95},
		RingTimeout: time.Second, CallDuration: 300 * time.Millisecond, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
		t.Fatalf("expected ACK, got %s", ack.start)
	}
	target := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(invite.body)), uint16(sdpPort(invite.body)))
	samples, err := mediaaudio.Normalize8kMono("testdata/sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	playbackPackets := make(chan int, 1)
	go func() { playbackPackets <- countRTP(rtpPeer, 250*time.Millisecond) }()
	sendAudioRTP(t, rtpPeer, target, PCMA, samples)
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if packets := <-playbackPackets; err != nil || packets == 0 || result.RTP.PacketsSent == 0 || result.RTP.PacketsReceived == 0 || result.Audio == nil || result.Audio.Coverage < 0.95 {
		t.Fatalf("playback=%d RTP=%+v audio=%+v err=%v", packets, result.RTP, result.Audio, err)
	}
}

func TestUDPInboundSendsSIPInfoAndRFC4733(t *testing.T) {
	t.Run("SIP INFO", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
			DTMF:        []DTMFAction{{Method: SIPInfo, Digits: "5#", Interval: 10 * time.Millisecond}},
			RingTimeout: time.Second, CallDuration: 180 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		answerInboundInvite(t, peer, rtpPeer, PCMA, false)
		_ = readRaw(t, peer, time.Second)
		for _, digit := range []string{"5", "#"} {
			info := readRaw(t, peer, time.Second)
			if !strings.HasPrefix(info.start, "INFO ") || !strings.Contains(info.body, "Signal="+digit) {
				t.Fatalf("unexpected INFO: %s body=%q", info.start, info.body)
			}
			writeRaw(t, peer, session.SIPAddr(), responseTo(info, 200, header(info, "to"), ""))
		}
		bye := readRaw(t, peer, time.Second)
		writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
		result, err := session.Wait()
		if err != nil || result.SIP.RequestsSent < 4 {
			t.Fatalf("SIP=%+v err=%v", result.SIP, err)
		}
	})

	t.Run("RFC4733 dynamic payload", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
			DTMF:        []DTMFAction{{Method: RFC4733, Digits: "5#", Interval: 10 * time.Millisecond}},
			RingTimeout: time.Second, CallDuration: 220 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		answerInboundInviteWithTelephone(t, peer, rtpPeer, PCMA, 112)
		_ = readRaw(t, peer, time.Second)
		packets := readRTPFromPeer(t, rtpPeer, 12, time.Second)
		for index, packet := range packets {
			if packet.PayloadType != 112 {
				t.Fatalf("packet %d payload type=%d", index, packet.PayloadType)
			}
		}
		for _, index := range []int{3, 4, 5, 9, 10, 11} {
			if packets[index].Payload[1]&0x80 == 0 {
				t.Fatalf("packet %d missing end bit", index)
			}
		}
		bye := readRaw(t, peer, time.Second)
		writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
		result, err := session.Wait()
		if err != nil || result.RTP.PacketsSent != 12 {
			t.Fatalf("RTP=%+v err=%v", result.RTP, err)
		}
	})
}

func TestUDPInboundCodecRejectionAndInviteRetransmission(t *testing.T) {
	t.Run("486 is a completed call result", func(t *testing.T) {
		peer := listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Second, ExpectStatus: 486,
		})
		if err != nil {
			t.Fatal(err)
		}
		invite := readRaw(t, peer, time.Second)
		to := header(invite, "to") + ";tag=peer-tag"
		writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 486, to, ""))
		if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
			t.Fatalf("expected ACK, got %s", ack.start)
		}
		result, err := session.Wait()
		if err != nil || result.Outcome.InviteFinalStatus != 486 || result.Outcome.Established {
			t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
		}
	})

	t.Run("488 can be the expected final status", func(t *testing.T) {
		peer := listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Second, ExpectStatus: 488,
		})
		if err != nil {
			t.Fatal(err)
		}
		invite := readRaw(t, peer, time.Second)
		to := header(invite, "to") + ";tag=peer-tag"
		writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 488, to, ""))
		ack := readRaw(t, peer, time.Second)
		if !strings.HasPrefix(ack.start, "ACK ") {
			t.Fatalf("expected ACK, got %s", ack.start)
		}
		result, err := session.Wait()
		if err != nil || result.Outcome.InviteFinalStatus != 488 {
			t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
		}
	})

	t.Run("unexpected final status fails verification", func(t *testing.T) {
		peer := listenUDP(t)
		carrier := newUDPTestCarrier(t, Config{})
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
			RingTimeout: time.Second, ExpectStatus: 486,
		})
		if err != nil {
			t.Fatal(err)
		}
		invite := readRaw(t, peer, time.Second)
		to := header(invite, "to") + ";tag=peer-tag"
		writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 503, to, ""))
		if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
			t.Fatalf("expected ACK, got %s", ack.start)
		}
		result, err := session.Wait()
		if !errors.Is(err, ErrVerification) || result.Outcome.InviteFinalStatus != 503 || !strings.Contains(err.Error(), "got 503, want 486") {
			t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
		}
	})

	t.Run("INVITE retransmits before provisional", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		const t1 = 10 * time.Second
		clock := newFakeTransactionClock()
		carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
		session, err := carrier.Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
			RingTimeout: time.Hour, CallDuration: time.Nanosecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		first := readRaw(t, peer, time.Second)
		clock.waitForActiveTimer(t, t1)
		clock.Advance(t1)
		second := readRaw(t, peer, time.Second)
		if first.start != second.start || header(first, "call-id") != header(second, "call-id") {
			t.Fatalf("INVITE retransmission changed transaction")
		}
		clock.waitForActiveTimer(t, 2*t1)
		clock.Advance(2 * t1)
		third := readRaw(t, peer, time.Second)
		if first.raw != third.raw {
			t.Fatalf("second INVITE retransmission changed transaction")
		}
		to := header(first, "to") + ";tag=peer-tag"
		rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
		body := fmt.Sprintf("v=0\r\no=p 1 1 IN IP4 %s\r\ns=p\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
		writeRaw(t, peer, session.SIPAddr(), responseTo(first, 200, to, body))
		_ = readRaw(t, peer, time.Second)
		clock.waitForActiveTimer(t, time.Nanosecond)
		clock.Advance(time.Nanosecond)
		bye := readRaw(t, peer, time.Second)
		writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
		result, err := session.Wait()
		if err != nil || result.SIP.Retransmissions != 2 {
			t.Fatalf("SIP=%+v err=%v", result.SIP, err)
		}
	})
}

func TestUDPInboundProvisionalStopsINVITERetransmission(t *testing.T) {
	peer := listenUDP(t)
	const t1 = 10 * time.Second
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Hour, ExpectStatus: 486,
	})
	if err != nil {
		t.Fatal(err)
	}
	invite := readRaw(t, peer, time.Second)
	clock.waitForActiveTimer(t, t1)
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 100, header(invite, "to"), ""))
	clock.waitForNoActiveTimer(t, t1)
	clock.Advance(t1)
	if message, err := readRawError(peer, 20*time.Millisecond); err == nil {
		t.Fatalf("INVITE retransmitted after provisional response: %s", message.start)
	}
	to := header(invite, "to") + ";tag=peer-tag"
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 486, to, ""))
	if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
		t.Fatalf("expected ACK, got %s", ack.start)
	}
	result, err := session.Wait()
	if err != nil || result.SIP.Retransmissions != 0 || result.Outcome.InviteFinalStatus != 486 {
		t.Fatalf("outcome=%+v SIP=%+v err=%v", result.Outcome, result.SIP, err)
	}
}

func TestUDPInboundRingTimeoutReturnsDeadlineExceeded(t *testing.T) {
	peer := listenUDP(t)
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: 2 * time.Hour, SIPT2: 2 * time.Hour}, clock)
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		RingTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if invite := readRaw(t, peer, time.Second); !strings.HasPrefix(invite.start, "INVITE ") {
		t.Fatalf("expected INVITE, got %s", invite.start)
	}
	clock.waitForActiveTimer(t, time.Hour)
	clock.Advance(time.Hour)
	result, err := session.Wait()
	if !errors.Is(err, context.DeadlineExceeded) || result.Outcome.InviteFinalStatus != 0 {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPInboundBYERetransmitsWithBoundedBackoff(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	const t1 = 10 * time.Second
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		RingTimeout: time.Hour, CallDuration: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	_ = readRaw(t, peer, time.Second)
	clock.waitForActiveTimer(t, time.Nanosecond)
	clock.Advance(time.Nanosecond)
	requests := []rawSIPMessage{readRaw(t, peer, time.Second)}
	for _, interval := range []time.Duration{t1, 2 * t1} {
		clock.waitForActiveTimer(t, interval)
		clock.Advance(interval)
		requests = append(requests, readRaw(t, peer, time.Second))
	}
	for index, request := range requests {
		if !strings.HasPrefix(request.start, "BYE ") || request.raw != requests[0].raw {
			t.Fatalf("BYE %d changed: %s", index, request.start)
		}
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(requests[0], 200, header(requests[0], "to"), ""))
	result, err := session.Wait()
	if err != nil || result.SIP.Retransmissions != 2 {
		t.Fatalf("SIP=%+v err=%v", result.SIP, err)
	}
}

func TestUDPInboundDuplicateFinalResponseResendsACK(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	firstACK := readRaw(t, peer, time.Second)
	rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8 101\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\na=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
	to := header(invite, "to") + ";tag=peer-tag"
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 200, to, body))
	secondACK := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(firstACK.start, "ACK ") || firstACK.start != secondACK.start || header(firstACK, "call-id") != header(secondACK, "call-id") {
		t.Fatalf("ACKs differ: first=%s second=%s", firstACK.start, secondACK.start)
	}
	from := header(invite, "to") + ";tag=peer-tag"
	remoteTo := header(invite, "from")
	bye := baseRequestWithHeaders("BYE", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), header(invite, "call-id"), from, remoteTo, 2, "BYE", "z9hG4bK-duplicate-bye")
	writeRaw(t, peer, session.SIPAddr(), bye)
	_ = readRaw(t, peer, time.Second)
	result, err := session.Wait()
	if err != nil || result.SIP.Retransmissions == 0 {
		t.Fatalf("SIP=%+v err=%v", result.SIP, err)
	}
}

func TestUDPInboundForkedSuccessResponseFailsExplicitly(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA, RingTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
		t.Fatalf("expected ACK, got %s", ack.start)
	}
	rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	body := fmt.Sprintf("v=0\r\no=fork 1 1 IN IP4 %s\r\ns=Fork\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
	forkTo := header(invite, "to") + ";tag=fork-tag"
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 200, forkTo, body))
	requests := []rawSIPMessage{
		readRaw(t, peer, time.Second),
		readRaw(t, peer, time.Second),
		readRaw(t, peer, time.Second),
	}
	if _, err := session.Wait(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Wait err=%v, want ErrProtocol", err)
	}
	if !strings.HasPrefix(requests[0].start, "ACK ") || !strings.HasPrefix(requests[1].start, "BYE ") || !strings.HasPrefix(requests[2].start, "BYE ") {
		t.Fatalf("fork cleanup requests=%v", starts(requests))
	}
}

func TestUDPOutboundProvidedSampleWAVAudioMatch(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{RTPDrainTimeout: 20 * time.Millisecond})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{}, Codecs: []Codec{PCMA}, Timeout: 3 * time.Second,
		ExpectAudio: &AudioExpectation{File: "testdata/sample.wav", MinSimilarity: 0.90, MinCoverage: 0.95},
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "call-sample"))
	ok := readUntilStatus(t, peer, 200)
	answer := ok[len(ok)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "call-sample", "from-tag", header(answer, "to"), 1, ""))
	rtpTarget := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(answer.body)), uint16(sdpPort(answer.body)))
	samples, err := mediaaudio.Normalize8kMono("testdata/sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	sendAudioRTP(t, rtpPeer, rtpTarget, PCMA, samples)
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddr, "call-sample", "from-tag", header(answer, "to"), 2, ""))
	_ = readRaw(t, peer, time.Second)
	result, err := session.Wait()
	if err != nil || result.Audio == nil || result.Audio.Similarity < 0.90 || result.Audio.Coverage < 0.95 || result.RTP.PacketsReceived == 0 {
		received := result.ReceivedPCM()
		preview := min(20, len(received))
		t.Fatalf("audio=%+v RTP=%+v err=%v expected=%v received=%v", result.Audio, result.RTP, err, samples[:20], received[:preview])
	}
	if _, err := mediaaudio.DecodeWAV(result.ReceivedWAV()); err != nil {
		t.Fatalf("in-memory recording is not playable WAV: %v", err)
	}
	trace := result.SIPTrace()
	if !strings.Contains(trace, "SIP <- INVITE") || !strings.Contains(trace, "SIP -> 200 OK") || !strings.Contains(trace, "SIP <- BYE") {
		t.Fatalf("unexpected SIP trace:\n%s", trace)
	}
}

func TestUDPOutboundAnswerPlaybackHangsUpWhenAudioFinishes(t *testing.T) {
	playbackFile := writeShortWAV(t, make([]int16, 3*160))
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{Playback: &AudioPlayback{File: playbackFile}},
		Codecs:   []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "answer-playback"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "answer-playback", "from-tag", header(ok, "to"), 1, ""))
	packets := readRTPFromPeer(t, rtpPeer, 3, time.Second)
	for _, packet := range packets {
		if packet.PayloadType != PCMA.PayloadType() {
			t.Fatalf("payload type=%d", packet.PayloadType)
		}
	}
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil || result.RTP.PacketsSent != 3 || result.Outcome.TerminatedBy != Sutel {
		t.Fatalf("outcome=%+v RTP=%+v err=%v", result.Outcome, result.RTP, err)
	}
}

func TestUDPOutboundAnswerEchoesAudioAfterDelay(t *testing.T) {
	const echoDelay = 60 * time.Millisecond
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{Echo: &AudioEcho{Delay: echoDelay}}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "answer-echo"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "answer-echo", "from-tag", header(ok, "to"), 1, ""))
	target := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(ok.body)), uint16(sdpPort(ok.body)))
	frame := make([]int16, 160)
	for index := range frame {
		frame[index] = int16(index*80 - 6400)
	}
	sentAt := time.Now()
	sendAudioRTP(t, rtpPeer, target, PCMA, frame)
	echoed := readRTPFromPeer(t, rtpPeer, 1, time.Second)[0]
	if elapsed := time.Since(sentAt); elapsed < echoDelay-10*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("echo delay=%s", elapsed)
	}
	wantPayload := mediaaudio.EncodeG711(frame, PCMA.String())
	if !slices.Equal(echoed.Payload, wantPayload) {
		t.Fatal("echoed payload differs from sent audio")
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddr, "answer-echo", "from-tag", header(ok, "to"), 2, ""))
	_ = readRaw(t, peer, time.Second)
	result, err := session.Wait()
	if err != nil || result.RTP.PacketsReceived != 1 || result.RTP.PacketsSent != 1 {
		t.Fatalf("RTP=%+v err=%v", result.RTP, err)
	}
}

func TestUDPOutboundSIPInfoAndRFC4733(t *testing.T) {
	for _, method := range []DTMFMethod{SIPInfo, RFC4733} {
		name := "RFC4733"
		if method == SIPInfo {
			name = "SIP-INFO"
		}
		t.Run(name, func(t *testing.T) {
			carrier := newUDPTestCarrier(t, Config{})
			session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
				Behavior: Answer{}, Timeout: 2 * time.Second, DTMF: &DTMFExpectation{Method: method, Digits: "5#"},
			})
			if err != nil {
				t.Fatal(err)
			}
			peer, rtpPeer := listenUDP(t), listenUDP(t)
			peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
			callID := "call-dtmf-" + name
			writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, 110, callID))
			responses := readUntilStatus(t, peer, 200)
			ok := responses[len(responses)-1]
			writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, callID, "from-tag", header(ok, "to"), 1, ""))
			if method == SIPInfo {
				for index, digit := range []string{"5", "#"} {
					body := "Signal=" + digit + "\r\nDuration=160\r\n"
					writeRaw(t, peer, session.SIPAddr(), dialogRequest("INFO", session.SIPAddr(), peerAddr, callID, "from-tag", header(ok, "to"), uint32(index+2), body))
					if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
						t.Fatalf("INFO response=%s", response.start)
					}
				}
			} else {
				target := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(ok.body)), uint16(sdpPort(ok.body)))
				sendTelephoneEvents(t, rtpPeer, target, 110, "5#")
			}
			writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddr, callID, "from-tag", header(ok, "to"), 10, ""))
			_ = readRaw(t, peer, time.Second)
			result, err := session.Wait()
			if err != nil || len(result.DTMFEvents()) != 2 {
				t.Fatalf("events=%+v err=%v", result.DTMFEvents(), err)
			}
		})
	}
}

func TestUDPOutboundEarlyMedia(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: EarlyMedia{
			File: "testdata/sample.wav", ProgressAfter: 10 * time.Millisecond,
			SendRinging: true, RingingAfter: 80 * time.Millisecond,
			AnswerAfter: 160 * time.Millisecond, HangupAfter: 240 * time.Millisecond,
		},
		Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInviteWithMappings(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{96}, map[uint8]Codec{96: PCMA}, -1, "call-early"))
	responses := readUntilStatus(t, peer, 183)
	progress := responses[len(responses)-1]
	if progress.start != "SIP/2.0 183 Session Progress" || !strings.Contains(progress.body, "a=rtpmap:96 PCMA/8000") {
		t.Fatalf("responses=%+v", starts(responses))
	}
	packets := make(chan int, 1)
	go func() { packets <- countRTP(rtpPeer, 120*time.Millisecond) }()
	ringing := readRaw(t, peer, time.Second)
	if ringing.start != "SIP/2.0 180 Ringing" {
		t.Fatalf("response=%s", ringing.start)
	}
	ok := readRaw(t, peer, time.Second)
	if ok.start != "SIP/2.0 200 OK" {
		t.Fatalf("response=%s", ok.start)
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "call-early", "from-tag", header(ok, "to"), 1, ""))
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(bye.start, "BYE ") {
		t.Fatalf("expected BYE, got %s", bye.start)
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil || <-packets == 0 || result.RTP.PacketsSent == 0 || result.Outcome.TerminatedBy != Sutel {
		t.Fatalf("outcome=%+v RTP=%+v err=%v", result.Outcome, result.RTP, err)
	}
}

func TestUDPOutboundRejectsInvalidInDialogINFO(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Answer{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "invalid-info"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "invalid-info", "from-tag", header(ok, "to"), 1, ""))

	tests := []struct {
		cseq        uint32
		body        string
		contentType string
		status      string
	}{
		{2, `{"signal":"5"}`, "application/json", "SIP/2.0 415 Unsupported Media Type"},
		{3, "Signal=5\r\n", "application/dtmf-relay", "SIP/2.0 400 Bad Request"},
		{4, "Signal=5\r\nSignal=6\r\nDuration=160\r\n", "application/dtmf-relay", "SIP/2.0 400 Bad Request"},
	}
	for _, test := range tests {
		request := dialogRequest("INFO", session.SIPAddr(), peerAddress, "invalid-info", "from-tag", header(ok, "to"), test.cseq, test.body)
		request = strings.Replace(request, "Content-Type: application/dtmf-relay", "Content-Type: "+test.contentType, 1)
		writeRaw(t, peer, session.SIPAddr(), request)
		if response := readRaw(t, peer, time.Second); response.start != test.status {
			t.Fatalf("CSeq=%d response=%s", test.cseq, response.start)
		}
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddress, "invalid-info", "from-tag", header(ok, "to"), 5, ""))
	_ = readRaw(t, peer, time.Second)
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPOutboundStaleINFOCSeqDoesNotRepeatAction(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{}, Timeout: 2 * time.Second, DTMF: &DTMFExpectation{Method: SIPInfo, Digits: "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "stale-info"))
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "stale-info", "from-tag", header(ok, "to"), 1, ""))
	for _, digit := range []string{"5", "6"} {
		body := "Signal=" + digit + "\r\nDuration=160\r\n"
		writeRaw(t, peer, session.SIPAddr(), dialogRequest("INFO", session.SIPAddr(), peerAddress, "stale-info", "from-tag", header(ok, "to"), 2, body))
		if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
			t.Fatalf("INFO response=%s", response.start)
		}
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddress, "stale-info", "from-tag", header(ok, "to"), 3, ""))
	_ = readRaw(t, peer, time.Second)
	result, err := session.Wait()
	if err != nil || len(result.DTMFEvents()) != 1 || result.DTMFEvents()[0].Digit != "5" {
		t.Fatalf("DTMF=%+v err=%v", result.DTMFEvents(), err)
	}
}

func TestUDPSessionCancellationAtMajorStates(t *testing.T) {
	t.Run("waiting for INVITE", func(t *testing.T) {
		carrier := newUDPTestCarrier(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		session, err := carrier.ExpectOutboundCall(ctx, OutboundScenario{Behavior: Answer{}, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait err=%v", err)
		}
	})

	t.Run("ringing", func(t *testing.T) {
		carrier := newUDPTestCarrier(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		session, err := carrier.ExpectOutboundCall(ctx, OutboundScenario{
			Behavior: Answer{RingingAfter: time.Second, AnswerAfter: time.Second}, Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
		writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "cancel-ringing"))
		if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 100 Trying" {
			t.Fatalf("response=%s", response.start)
		}
		cancel()
		if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait err=%v", err)
		}
	})

	t.Run("waiting for ACK", func(t *testing.T) {
		carrier := newUDPTestCarrier(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		session, err := carrier.ExpectOutboundCall(ctx, OutboundScenario{Behavior: Answer{}, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
		writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "cancel-ack"))
		_ = readUntilStatus(t, peer, 200)
		cancel()
		if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait err=%v", err)
		}
	})

	t.Run("established", func(t *testing.T) {
		carrier := newUDPTestCarrier(t, Config{})
		ctx, cancel := context.WithCancel(context.Background())
		session, err := carrier.ExpectOutboundCall(ctx, OutboundScenario{Behavior: Answer{}, Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
		writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "cancel-established"))
		responses := readUntilStatus(t, peer, 200)
		ok := responses[len(responses)-1]
		writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "cancel-established", "from-tag", header(ok, "to"), 1, ""))
		writeRaw(t, peer, session.SIPAddr(), dialogRequest("INFO", session.SIPAddr(), peerAddress, "cancel-established", "from-tag", header(ok, "to"), 2, "Signal=5\r\nDuration=160\r\n"))
		if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
			t.Fatalf("INFO response=%s", response.start)
		}
		cancel()
		if _, err := session.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait err=%v", err)
		}
	})
}

func TestUDPSessionConcurrentWaitReturnsIndependentResults(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "concurrent-wait"))
	response := readRaw(t, peer, time.Second)

	const waiters = 8
	results := make(chan CallResult, waiters)
	errorsChannel := make(chan error, waiters)
	var wait sync.WaitGroup
	for range waiters {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, waitErr := session.Wait()
			results <- result
			errorsChannel <- waitErr
		}()
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "concurrent-wait", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	wait.Wait()
	close(results)
	close(errorsChannel)
	for waitErr := range errorsChannel {
		if waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	allResults := make([]CallResult, 0, waiters)
	for result := range results {
		allResults = append(allResults, result)
	}
	if len(allResults) != waiters || len(allResults[0].events) == 0 || len(allResults[1].events) == 0 {
		t.Fatalf("results do not contain independent event data: %+v", allResults)
	}
	original := allResults[1].events[0]
	allResults[0].events[0].Detail = "mutated by another waiter"
	if allResults[1].events[0] != original {
		t.Fatal("mutating one Wait result changed another result")
	}
}

func TestUDPOutboundDuplicateEstablishedINVITEResendsCachedOK(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Answer{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	invite := outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "duplicate-established")
	writeRaw(t, peer, session.SIPAddr(), invite)
	responses := readUntilStatus(t, peer, 200)
	ok := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddress, "duplicate-established", "from-tag", header(ok, "to"), 1, ""))
	writeRaw(t, peer, session.SIPAddr(), invite)
	retransmitted := readRaw(t, peer, time.Second)
	if retransmitted.raw != ok.raw {
		t.Fatalf("cached 200 changed:\nfirst:\n%s\nsecond:\n%s", ok.raw, retransmitted.raw)
	}
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("BYE", session.SIPAddr(), peerAddress, "duplicate-established", "from-tag", header(ok, "to"), 2, ""))
	_ = readRaw(t, peer, time.Second)
	result, err := session.Wait()
	if err != nil || result.SIP.Retransmissions == 0 {
		t.Fatalf("SIP=%+v err=%v", result.SIP, err)
	}
}

func TestUDPOutboundNumberMismatchReturns404AndVerificationError(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		From: "100", To: "1900", Behavior: Answer{}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	invite := outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "number-mismatch")
	invite = strings.Replace(invite, "From: <sip:100@", "From: <sip:999@", 1)
	writeRaw(t, peer, session.SIPAddr(), invite)
	response := readRaw(t, peer, time.Second)
	if response.start != "SIP/2.0 404 Not Found" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "number-mismatch", "from-tag", header(response, "to"), 1, "ACK", "z9hG4bK-test"))
	if _, err := session.Wait(); !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), `From number got "999", want "100"`) {
		t.Fatalf("Wait err=%v", err)
	}
}

func TestUDPOutboundEarlyMediaThenBusy(t *testing.T) {
	const t1 = 10 * time.Second
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: EarlyFailure{
			File: "testdata/sample.wav", ProgressAfter: 10 * time.Millisecond, FailureAfter: 100 * time.Millisecond,
			FinalReason: "Busy Here", ReasonHeader: `Q.850;cause=17;text="USER_BUSY"`,
		},
		Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInviteWithMappings(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{96}, map[uint8]Codec{96: PCMA}, 101, "call-early-busy"))
	clock.waitForActiveTimer(t, 10*time.Millisecond)
	clock.Advance(10 * time.Millisecond)
	responses := readUntilStatus(t, peer, 183)
	progress := responses[len(responses)-1]
	if !containsStatus(responses, 100) || header(progress, "content-disposition") != "session" || sdpPort(progress.body) == 0 || !strings.Contains(progress.body, "a=rtpmap:96 PCMA/8000") {
		t.Fatalf("unexpected provisional responses: %+v SDP=%q", starts(responses), progress.body)
	}
	if packets := countRTP(rtpPeer, 50*time.Millisecond); packets == 0 {
		t.Fatal("no early RTP before failure")
	}
	clock.waitForActiveTimer(t, 90*time.Millisecond)
	clock.Advance(90 * time.Millisecond)
	final := readRaw(t, peer, time.Second)
	if final.start != "SIP/2.0 486 Busy Here" || header(final, "reason") != `Q.850;cause=17;text="USER_BUSY"` || header(final, "to") != header(progress, "to") {
		t.Fatalf("unexpected final response: %s Reason=%q To=%q", final.start, header(final, "reason"), header(final, "to"))
	}
	clock.waitForActiveTimer(t, t1)
	clock.Advance(t1)
	retransmitted := readRaw(t, peer, time.Second)
	if retransmitted.start != final.start || header(retransmitted, "call-id") != "call-early-busy" {
		t.Fatalf("final was not retransmitted: %+v", retransmitted)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "call-early-busy", "from-tag", header(final, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || result.Outcome.InviteFinalStatus != 486 || result.Outcome.Established || result.RTP.PacketsSent == 0 || result.SIP.Retransmissions == 0 {
		t.Fatalf("outcome=%+v RTP=%+v SIP=%+v err=%v", result.Outcome, result.RTP, result.SIP, err)
	}
}

func TestUDPOutboundEarlyFailureUsesConfiguredFinalStatus(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: EarlyFailure{File: "testdata/sample.wav", FinalStatus: 503},
		Codecs:   []Codec{PCMA}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddress := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddress, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "early-503"))
	responses := readUntilStatus(t, peer, 183)
	progress := responses[len(responses)-1]
	final := readRaw(t, peer, time.Second)
	if final.start != "SIP/2.0 503 Service Unavailable" || header(final, "to") != header(progress, "to") {
		t.Fatalf("final=%s To=%q, progress To=%q", final.start, header(final, "to"), header(progress, "to"))
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddress, "early-503", "from-tag", header(final, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || result.Outcome.InviteFinalStatus != 503 || result.Outcome.Established {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestTenUDPCallPairsRunInParallel(t *testing.T) {
	const pairs = 10
	type pairResult struct {
		sip      netip.AddrPort
		rtp      netip.AddrPort
		incoming uint32
		result   CallResult
	}
	var wait sync.WaitGroup
	errorsChannel := make(chan error, pairs)
	results := make(chan pairResult, pairs)
	for index := 0; index < pairs; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			session, err := ExpectOutboundCall(context.Background(), OutboundScenario{
				Behavior: Answer{Echo: &AudioEcho{}, HangupAfter: 300 * time.Millisecond}, Codecs: []Codec{PCMA}, Timeout: 3 * time.Second,
			})
			if err != nil {
				errorsChannel <- err
				return
			}
			defer session.Close()
			peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				errorsChannel <- err
				return
			}
			defer peer.Close()
			rtpPeer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				errorsChannel <- err
				return
			}
			defer rtpPeer.Close()
			peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
			callID := fmt.Sprintf("parallel-%d", index)
			if _, err := peer.WriteToUDPAddrPort([]byte(outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, callID)), session.SIPAddr()); err != nil {
				errorsChannel <- err
				return
			}
			var answer rawSIPMessage
			for answer.start != "SIP/2.0 200 OK" {
				answer, err = readRawError(peer, time.Second)
				if err != nil {
					errorsChannel <- fmt.Errorf("pair %d read answer: %w", index, err)
					return
				}
			}
			if _, err := peer.WriteToUDPAddrPort([]byte(dialogRequest("ACK", session.SIPAddr(), peerAddr, callID, "from-tag", header(answer, "to"), 1, "")), session.SIPAddr()); err != nil {
				errorsChannel <- err
				return
			}
			rtpTarget := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(answer.body)), uint16(sdpPort(answer.body)))
			incomingSSRC := uint32(1000 + index)
			samples := make([]int16, 160)
			for sampleIndex := range samples {
				samples[sampleIndex] = int16(500 + index*500)
			}
			payload := mediaaudio.EncodeG711(samples, "PCMA")
			packet := pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 1, Timestamp: 160, SSRC: incomingSSRC}, Payload: payload}
			data, marshalErr := packet.Marshal()
			if marshalErr != nil {
				errorsChannel <- marshalErr
				return
			}
			if _, err := rtpPeer.WriteToUDPAddrPort(data, rtpTarget); err != nil {
				errorsChannel <- err
				return
			}
			if err := rtpPeer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				errorsChannel <- err
				return
			}
			buffer := make([]byte, 2048)
			length, _, err := rtpPeer.ReadFromUDPAddrPort(buffer)
			if err != nil {
				errorsChannel <- fmt.Errorf("pair %d read echo: %w", index, err)
				return
			}
			var echoed pionrtp.Packet
			if err := echoed.Unmarshal(buffer[:length]); err != nil || !slices.Equal(echoed.Payload, payload) {
				errorsChannel <- fmt.Errorf("pair %d invalid echo: %v", index, err)
				return
			}
			bye, err := readRawError(peer, time.Second)
			if err != nil || !strings.HasPrefix(bye.start, "BYE ") {
				errorsChannel <- fmt.Errorf("pair %d BYE=%s err=%v", index, bye.start, err)
				return
			}
			if _, err := peer.WriteToUDPAddrPort([]byte(responseTo(bye, 200, header(bye, "to"), "")), session.SIPAddr()); err != nil {
				errorsChannel <- err
				return
			}
			result, err := session.Wait()
			if err != nil || result.RTP.SSRC != incomingSSRC || result.RTP.PacketsReceived != 1 || result.RTP.PacketsSent == 0 {
				errorsChannel <- fmt.Errorf("pair %d result=%+v err=%v", index, result.RTP, err)
				return
			}
			results <- pairResult{sip: session.SIPAddr(), rtp: rtpTarget, incoming: incomingSSRC, result: result}
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	close(results)
	seenSIP := map[netip.AddrPort]bool{}
	seenRTP := map[netip.AddrPort]bool{}
	seenSSRC := map[uint32]bool{}
	for pair := range results {
		if seenSIP[pair.sip] || seenRTP[pair.rtp] || seenSSRC[pair.incoming] || pair.result.SIP.LocalAddr != pair.sip {
			t.Fatalf("cross-call or endpoint collision: %+v", pair)
		}
		seenSIP[pair.sip], seenRTP[pair.rtp], seenSSRC[pair.incoming] = true, true, true
	}
	if len(seenSIP) != pairs || len(seenRTP) != pairs || len(seenSSRC) != pairs {
		t.Fatalf("SIP=%d RTP=%d SSRC=%d", len(seenSIP), len(seenRTP), len(seenSSRC))
	}
}

func newUDPTestCarrier(t *testing.T, config Config) *Carrier {
	t.Helper()
	return newUDPTestCarrierWithClock(t, config, nil)
}

func newUDPTestCarrierWithClock(t *testing.T, config Config, clock transactionClock) *Carrier {
	t.Helper()
	carrier, err := NewCarrier(config)
	if err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		carrier.clock = clock
	}
	t.Cleanup(func() {
		if err := carrier.Close(); err != nil {
			t.Errorf("close carrier: %v", err)
		}
	})
	return carrier
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func outboundInvite(target, peer, rtp netip.AddrPort, formats []uint8, telephoneEvent int, callID string) string {
	return outboundInviteWithMappings(target, peer, rtp, formats, nil, telephoneEvent, callID)
}

func outboundInviteWithMappings(target, peer, rtp netip.AddrPort, formats []uint8, mappings map[uint8]Codec, telephoneEvent int, callID string) string {
	formatStrings := make([]string, 0, len(formats)+1)
	var attributes strings.Builder
	for _, payload := range formats {
		formatStrings = append(formatStrings, strconv.Itoa(int(payload)))
		codec, mapped := mappings[payload]
		if !mapped {
			switch payload {
			case 8:
				codec, mapped = PCMA, true
			case 0:
				codec, mapped = PCMU, true
			}
		}
		if mapped {
			fmt.Fprintf(&attributes, "a=rtpmap:%d %s/8000\r\n", payload, codec)
		}
	}
	if telephoneEvent >= 0 {
		formatStrings = append(formatStrings, strconv.Itoa(telephoneEvent))
		fmt.Fprintf(&attributes, "a=rtpmap:%d telephone-event/8000\r\na=fmtp:%d 0-16\r\n", telephoneEvent, telephoneEvent)
	}
	body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP %s\r\n%sa=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port(), strings.Join(formatStrings, " "), attributes.String())
	return fmt.Sprintf("INVITE sip:1900@%s:%d SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=z9hG4bK-test;rport\r\nMax-Forwards: 70\r\nFrom: <sip:100@%s:%d>;tag=from-tag\r\nTo: <sip:1900@%s:%d>\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\nContact: <sip:100@%s:%d;transport=udp>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", target.Addr(), target.Port(), peer.Addr(), peer.Port(), peer.Addr(), peer.Port(), target.Addr(), target.Port(), callID, peer.Addr(), peer.Port(), len(body), body)
}

func baseRequest(method string, target, peer netip.AddrPort, callID, fromTag, to string, cseq uint32, cseqMethod, branch string) string {
	from := fmt.Sprintf("<sip:100@%s:%d>;tag=%s", peer.Addr(), peer.Port(), fromTag)
	return baseRequestWithHeaders(method, target, peer, callID, from, to, cseq, cseqMethod, branch)
}

func baseRequestWithHeaders(method string, target, peer netip.AddrPort, callID, from, to string, cseq uint32, cseqMethod, branch string) string {
	return fmt.Sprintf("%s sip:1900@%s:%d SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=%s;rport\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nContact: <sip:100@%s:%d;transport=udp>\r\nContent-Length: 0\r\n\r\n", method, target.Addr(), target.Port(), peer.Addr(), peer.Port(), branch, from, to, callID, cseq, cseqMethod, peer.Addr(), peer.Port())
}

func dialogRequest(method string, target, peer netip.AddrPort, callID, fromTag, to string, cseq uint32, body string) string {
	content := ""
	if body != "" {
		content = "Content-Type: application/dtmf-relay\r\n"
	}
	return fmt.Sprintf("%s sip:1900@%s:%d SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=z9hG4bK-%s-%d;rport\r\nFrom: <sip:100@%s:%d>;tag=%s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %d %s\r\nContact: <sip:100@%s:%d;transport=udp>\r\n%sContent-Length: %d\r\n\r\n%s", method, target.Addr(), target.Port(), peer.Addr(), peer.Port(), strings.ToLower(method), cseq, peer.Addr(), peer.Port(), fromTag, to, callID, cseq, method, peer.Addr(), peer.Port(), content, len(body), body)
}

func answerInboundInvite(t *testing.T, peer, rtpPeer *net.UDPConn, codec Codec, omitTelephoneEvent bool) rawSIPMessage {
	telephoneEvent := 101
	if omitTelephoneEvent {
		telephoneEvent = -1
	}
	return answerInboundInviteWithTelephone(t, peer, rtpPeer, codec, telephoneEvent)
}

func answerInboundInviteWithTelephone(t *testing.T, peer, rtpPeer *net.UDPConn, codec Codec, telephoneEvent int) rawSIPMessage {
	t.Helper()
	invite := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(invite.start, "INVITE ") {
		t.Fatalf("expected INVITE, got %s", invite.start)
	}
	writeRaw(t, peer, sourceFromVia(invite), responseTo(invite, 100, header(invite, "to"), ""))
	to := header(invite, "to") + ";tag=peer-tag"
	writeRaw(t, peer, sourceFromVia(invite), responseTo(invite, 180, to, ""))
	rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	telephone := " " + strconv.Itoa(telephoneEvent)
	telephoneAttributes := fmt.Sprintf("a=rtpmap:%d telephone-event/8000\r\na=fmtp:%d 0-16\r\n", telephoneEvent, telephoneEvent)
	if telephoneEvent < 0 {
		telephone, telephoneAttributes = "", ""
	}
	body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP %d%s\r\na=rtpmap:%d %s/8000\r\n%sa=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port(), codec.PayloadType(), telephone, codec.PayloadType(), codec.String(), telephoneAttributes)
	writeRaw(t, peer, sourceFromVia(invite), responseTo(invite, 200, to, body))
	return invite
}

func readRTPFromPeer(t *testing.T, connection *net.UDPConn, count int, timeout time.Duration) []pionrtp.Packet {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(timeout))
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
		result = append(result, *packet.Clone())
	}
	return result
}

func responseTo(request rawSIPMessage, status int, to, body string) string {
	extra := ""
	if status >= 180 {
		extra += fmt.Sprintf("Contact: <sip:1900@%s;transport=udp>\r\n", requestTargetHost(request))
	}
	if body != "" {
		extra += "Content-Type: application/sdp\r\n"
	}
	return fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s", status, statusReason(status), header(request, "via"), header(request, "from"), to, header(request, "call-id"), header(request, "cseq"), extra, len(body), body)
}

func requestTargetHost(message rawSIPMessage) string {
	fields := strings.Fields(message.start)
	if len(fields) < 2 {
		return "127.0.0.1:5060"
	}
	uri := strings.TrimPrefix(fields[1], "sip:")
	if at := strings.LastIndex(uri, "@"); at >= 0 {
		uri = uri[at+1:]
	}
	return strings.Split(uri, ";")[0]
}

func sourceFromVia(message rawSIPMessage) netip.AddrPort {
	host := headerHost(message, "via")
	address, _ := netip.ParseAddrPort(host)
	return address
}

func headerHost(message rawSIPMessage, name string) string {
	value := header(message, name)
	if name == "via" {
		parts := strings.Fields(value)
		if len(parts) >= 2 {
			return strings.Split(parts[1], ";")[0]
		}
	}
	return value
}

func writeRaw(t *testing.T, connection *net.UDPConn, target netip.AddrPort, message string) {
	t.Helper()
	if _, err := connection.WriteToUDPAddrPort([]byte(message), target); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, connection *net.UDPConn, timeout time.Duration) rawSIPMessage {
	t.Helper()
	message, err := readRawError(connection, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func readRawError(connection *net.UDPConn, timeout time.Duration) (rawSIPMessage, error) {
	if err := connection.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return rawSIPMessage{}, err
	}
	buffer := make([]byte, 65536)
	length, _, err := connection.ReadFromUDPAddrPort(buffer)
	if err != nil {
		return rawSIPMessage{}, err
	}
	raw := string(buffer[:length])
	parts := strings.SplitN(raw, "\r\n\r\n", 2)
	lines := strings.Split(parts[0], "\r\n")
	message := rawSIPMessage{start: lines[0], headers: make(map[string][]string), raw: raw}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			key := strings.ToLower(strings.TrimSpace(name))
			message.headers[key] = append(message.headers[key], strings.TrimSpace(value))
		}
	}
	if len(parts) == 2 {
		message.body = parts[1]
	}
	return message, nil
}

func readUntilStatus(t *testing.T, connection *net.UDPConn, status int) []rawSIPMessage {
	t.Helper()
	var result []rawSIPMessage
	wanted := fmt.Sprintf("SIP/2.0 %d ", status)
	for len(result) < 10 {
		message := readRaw(t, connection, time.Second)
		result = append(result, message)
		if strings.HasPrefix(message.start, wanted) {
			return result
		}
	}
	t.Fatalf("status %d not received: %+v", status, starts(result))
	return nil
}

func header(message rawSIPMessage, name string) string {
	values := message.headers[strings.ToLower(name)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func starts(messages []rawSIPMessage) []string {
	result := make([]string, len(messages))
	for index := range messages {
		result[index] = messages[index].start
	}
	return result
}

func containsStatus(messages []rawSIPMessage, status int) bool {
	prefix := fmt.Sprintf("SIP/2.0 %d ", status)
	for _, message := range messages {
		if strings.HasPrefix(message.start, prefix) {
			return true
		}
	}
	return false
}

func statusReason(status int) string {
	return map[int]string{100: "Trying", 180: "Ringing", 183: "Session Progress", 200: "OK", 404: "Not Found", 486: "Busy Here", 487: "Request Terminated", 488: "Not Acceptable Here", 503: "Service Unavailable", 603: "Decline"}[status]
}

func sdpPort(body string) int {
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			port, _ := strconv.Atoi(fields[1])
			return port
		}
	}
	return 0
}

func sdpAddress(body string) string {
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, "c=IN IP4 ") {
			return strings.TrimPrefix(line, "c=IN IP4 ")
		}
	}
	return ""
}

func writeShortWAV(t *testing.T, samples []int16) string {
	t.Helper()
	var contents bytes.Buffer
	if err := mediaaudio.WriteWAV(&contents, samples, 8000); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "playback.wav")
	if err := os.WriteFile(path, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sendAudioRTP(t *testing.T, connection *net.UDPConn, target netip.AddrPort, codec Codec, samples []int16) {
	t.Helper()
	sequence, timestamp := uint16(1000), uint32(8000)
	for offset := 0; offset < len(samples); offset += 160 {
		frame := make([]int16, 160)
		copy(frame, samples[offset:min(offset+160, len(samples))])
		packet := pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: codec.PayloadType(), SequenceNumber: sequence, Timestamp: timestamp, SSRC: 0x12345678}, Payload: mediaaudio.EncodeG711(frame, codec.String())}
		data, err := packet.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.WriteToUDPAddrPort(data, target); err != nil {
			t.Fatal(err)
		}
		sequence++
		timestamp += 160
	}
}

func sendTelephoneEvents(t *testing.T, connection *net.UDPConn, target netip.AddrPort, payloadType uint8, digits string) {
	t.Helper()
	sequence := uint16(2000)
	timestamp := uint32(16000)
	for _, digit := range digits {
		event := byte(digit - '0')
		if digit == '#' {
			event = 11
		}
		for _, item := range []struct {
			duration uint16
			end      bool
		}{{160, false}, {320, false}, {480, true}, {480, true}, {480, true}} {
			payload := []byte{event, 10, byte(item.duration >> 8), byte(item.duration)}
			if item.end {
				payload[1] |= 0x80
			}
			packet := pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: payloadType, SequenceNumber: sequence, Timestamp: timestamp, SSRC: 99}, Payload: payload}
			data, _ := packet.Marshal()
			if _, err := connection.WriteToUDPAddrPort(data, target); err != nil {
				t.Fatal(err)
			}
			sequence++
		}
		timestamp += 800
	}
}

func countRTP(connection *net.UDPConn, duration time.Duration) int {
	deadline := time.Now().Add(duration)
	_ = connection.SetReadDeadline(deadline)
	buffer := make([]byte, 2048)
	count := 0
	for {
		if _, _, err := connection.ReadFromUDPAddrPort(buffer); err != nil {
			return count
		}
		count++
	}
}
