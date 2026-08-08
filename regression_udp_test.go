package sutel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"
)

func TestUDPInboundAcceptsRTPBeforeAnswerNegotiationCompletes(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		RingTimeout: time.Second, CallDuration: 50 * time.Millisecond, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := readRaw(t, peer, time.Second)
	target := netip.AddrPortFrom(netip.MustParseAddr(sdpAddress(invite.body)), uint16(sdpPort(invite.body)))
	packet := pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 1, Timestamp: 160, SSRC: 77}, Payload: make([]byte, 160)}
	data, _ := packet.Marshal()
	if _, err := rtpPeer.WriteToUDPAddrPort(data, target); err != nil {
		t.Fatal(err)
	}
	rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 200, header(invite, "to")+";tag=peer-tag", body))
	_ = readRaw(t, peer, time.Second)
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil || result.RTP.PacketsReceived != 1 || result.RTP.SSRC != 77 {
		t.Fatalf("RTP=%+v err=%v", result.RTP, err)
	}
}

func TestUDPInboundSendsBYEAfterACKWhenAnswerValidationFails(t *testing.T) {
	peer := listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		RingTimeout: time.Second, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 200, header(invite, "to")+";tag=peer-tag", ""))
	ack := readRaw(t, peer, time.Second)
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(ack.start, "ACK ") || !strings.HasPrefix(bye.start, "BYE ") {
		t.Fatalf("ACK=%s BYE=%s", ack.start, bye.start)
	}
	if _, err := session.Wait(); !errors.Is(err, ErrNegotiation) {
		t.Fatalf("Wait err=%v", err)
	}
}

func TestUDPValidationFinalRetransmitsUntilACK(t *testing.T) {
	const t1 = 10 * time.Second
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{From: "100", Behavior: Answer{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	invite := outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "validation-final")
	invite = strings.Replace(invite, "From: <sip:100@", "From: <sip:999@", 1)
	writeRaw(t, peer, session.SIPAddr(), invite)
	first := readRaw(t, peer, time.Second)
	clock.waitForActiveTimer(t, t1)
	clock.Advance(t1)
	second := readRaw(t, peer, time.Second)
	if first.raw != second.raw || first.start != "SIP/2.0 404 Not Found" {
		t.Fatalf("first=%s second=%s", first.start, second.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "validation-final", "from-tag", header(first, "to"), 1, "ACK", "z9hG4bK-test"))
	if _, err := session.Wait(); !errors.Is(err, ErrVerification) {
		t.Fatalf("Wait err=%v", err)
	}
}

func TestUDPInboundPeerBYEDuringINFOStopsNestedTransaction(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		DTMF: []DTMFAction{{Method: SIPInfo, Digits: "55"}}, RingTimeout: time.Second, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	_ = readRaw(t, peer, time.Second)
	info := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(info.start, "INFO ") {
		t.Fatalf("request=%s", info.start)
	}
	from := header(invite, "to") + ";tag=peer-tag"
	bye := baseRequestWithHeaders("BYE", session.SIPAddr(), peer.LocalAddr().(*net.UDPAddr).AddrPort(), header(invite, "call-id"), from, header(invite, "from"), 2, "BYE", "z9hG4bK-peer-bye-info")
	writeRaw(t, peer, session.SIPAddr(), bye)
	if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 200 OK" {
		t.Fatalf("BYE response=%s", response.start)
	}
	if message, err := readRawError(peer, 100*time.Millisecond); err == nil {
		t.Fatalf("request sent after peer BYE: %s", message.start)
	}
	result, err := session.Wait()
	if err != nil || result.Outcome.TerminatedBy != SystemUnderTest {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPUserlessContactAndRouteSupportLocalBYE(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	session, err := ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{HangupAfter: 60 * time.Millisecond}, Codecs: []Codec{PCMA}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	invite := outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "userless-route")
	invite = strings.Replace(invite, fmt.Sprintf("Contact: <sip:100@%s", peerAddr.Addr()), fmt.Sprintf("Contact: <sip:%s", peerAddr.Addr()), 1)
	invite = strings.Replace(invite, "Max-Forwards: 70\r\n", fmt.Sprintf("Record-Route: <sip:%s:%d;lr>\r\nMax-Forwards: 70\r\n", peerAddr.Addr(), peerAddr.Port()), 1)
	writeRaw(t, peer, session.SIPAddr(), invite)
	responses := readUntilStatus(t, peer, 200)
	answer := responses[len(responses)-1]
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("ACK", session.SIPAddr(), peerAddr, "userless-route", "from-tag", header(answer, "to"), 1, ""))
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(bye.start, "BYE ") || !strings.Contains(header(bye, "route"), "sip:"+peerAddr.String()) {
		t.Fatalf("BYE=%s Route=%q", bye.start, header(bye, "route"))
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPInboundNon2xxDuplicateIsReACKed(t *testing.T) {
	const t1 = 10 * time.Second
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: t1, SIPT2: 25 * time.Second}, clock)
	peer := listenUDP(t)
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		ExpectStatus: 486, RingTimeout: time.Hour, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	invite := readRaw(t, peer, time.Second)
	final := responseTo(invite, 486, header(invite, "to")+";tag=peer-tag", "")
	writeRaw(t, peer, session.SIPAddr(), final)
	firstACK := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), final)
	secondACK := readRaw(t, peer, time.Second)
	if firstACK.raw != secondACK.raw {
		t.Fatalf("ACK changed:\n%s\n%s", firstACK.raw, secondACK.raw)
	}
	clock.waitForActiveTimer(t, 20*time.Second)
	clock.Advance(20 * time.Second)
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPLocalBYEIsAfterLastMediaSend(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		Playback: &AudioPlayback{File: "testdata/sample.wav"}, RingTimeout: time.Second, CallDuration: 80 * time.Millisecond, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	answerInboundInvite(t, peer, rtpPeer, PCMA, false)
	_ = readRaw(t, peer, time.Second)
	bye := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))
	result, err := session.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var byeTime time.Time
	for _, event := range result.Events() {
		if event.Layer == SIPLayer && event.Direction == Sent && event.Type == "BYE" {
			byeTime = event.Time
		}
	}
	if byeTime.IsZero() {
		t.Fatal("missing BYE event")
	}
	for _, event := range result.Events() {
		if event.Layer == RTPLayer && event.Direction == Sent && event.Time.After(byeTime) {
			t.Fatalf("RTP event after BYE: %+v", event)
		}
	}
}

func TestUDPAnswerDelayHandlesCANCELAndCachedProvisional(t *testing.T) {
	carrier := newUDPTestCarrier(t, Config{})
	session, err := carrier.ExpectOutboundCall(context.Background(), OutboundScenario{
		Behavior: Answer{RingingAfter: time.Hour, AnswerAfter: time.Hour}, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	inviteText := outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "cancel-during-delay")
	writeRaw(t, peer, session.SIPAddr(), inviteText)
	trying := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), inviteText)
	if cached := readRaw(t, peer, time.Second); cached.raw != trying.raw {
		t.Fatalf("cached provisional changed:\n%s\n%s", trying.raw, cached.raw)
	}
	inviteTo := fmt.Sprintf("<sip:1900@%s:%d>", session.SIPAddr().Addr(), session.SIPAddr().Port())
	cancel := baseRequest("CANCEL", session.SIPAddr(), peerAddr, "cancel-during-delay", "from-tag", inviteTo, 1, "CANCEL", "z9hG4bK-test")
	writeRaw(t, peer, session.SIPAddr(), cancel)
	cancelOK := readRaw(t, peer, time.Second)
	terminated := readRaw(t, peer, time.Second)
	if cancelOK.start != "SIP/2.0 200 OK" || terminated.start != "SIP/2.0 487 Request Terminated" {
		t.Fatalf("CANCEL=%s INVITE=%s", cancelOK.start, terminated.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "cancel-during-delay", "from-tag", header(terminated, "to"), 1, "ACK", "z9hG4bK-test"))
	result, err := session.Wait()
	if err != nil || !result.Outcome.Canceled {
		t.Fatalf("outcome=%+v err=%v", result.Outcome, err)
	}
}

func TestUDPInboundRingTimeoutCancelsAfterProvisional(t *testing.T) {
	clock := newFakeTransactionClock()
	carrier := newUDPTestCarrierWithClock(t, Config{SIPT1: 10 * time.Second, SIPT2: 25 * time.Second}, clock)
	peer := listenUDP(t)
	session, err := carrier.Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		RingTimeout: time.Hour, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	invite := readRaw(t, peer, time.Second)
	clock.waitForActiveTimer(t, 10*time.Second)
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 180, header(invite, "to")+";tag=peer-tag", ""))
	clock.waitForNoActiveTimer(t, 10*time.Second)
	clock.waitForActiveTimer(t, time.Hour)
	clock.Advance(time.Hour)
	cancel := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(cancel.start, "CANCEL ") {
		t.Fatalf("request=%s", cancel.start)
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(cancel, 200, header(cancel, "to"), ""))
	writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 487, header(invite, "to")+";tag=peer-tag", ""))
	if ack := readRaw(t, peer, time.Second); !strings.HasPrefix(ack.start, "ACK ") {
		t.Fatalf("ACK=%s", ack.start)
	}
	if _, err := session.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait err=%v", err)
	}
}

func TestUDPOutOfDialogRequestsReceive481WhileWaitingForInviteACK(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := ExpectOutboundCall(context.Background(), OutboundScenario{Behavior: Busy{}, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	writeRaw(t, peer, session.SIPAddr(), outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "wait-ack-481"))
	final := readRaw(t, peer, time.Second)
	writeRaw(t, peer, session.SIPAddr(), dialogRequest("INFO", session.SIPAddr(), peerAddr, "outside", "from-tag", header(final, "to"), 2, "Signal=5\r\nDuration=160\r\n"))
	if response := readRaw(t, peer, time.Second); response.start != "SIP/2.0 481 Call/Transaction Does Not Exist" {
		t.Fatalf("response=%s", response.start)
	}
	writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "wait-ack-481", "from-tag", header(final, "to"), 1, "ACK", "z9hG4bK-test"))
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPExpectAudioRejectsRemoteDirectionThatCannotSend(t *testing.T) {
	t.Run("outbound offer recvonly", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		session, err := ExpectOutboundCall(context.Background(), OutboundScenario{
			Behavior: Answer{}, Codecs: []Codec{PCMA},
			ExpectAudio: &AudioExpectation{File: "testdata/sample.wav", MinSimilarity: 0.9, MinCoverage: 0.9}, Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(session.Close)
		peerAddr := peer.LocalAddr().(*net.UDPAddr).AddrPort()
		invite := outboundInvite(session.SIPAddr(), peerAddr, rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort(), []uint8{8}, -1, "direction-outbound")
		invite = strings.Replace(invite, "a=sendrecv", "a=recvonly", 1)
		writeRaw(t, peer, session.SIPAddr(), invite)
		final := readRaw(t, peer, time.Second)
		if final.start != "SIP/2.0 488 Not Acceptable Here" {
			t.Fatalf("response=%s", final.start)
		}
		writeRaw(t, peer, session.SIPAddr(), baseRequest("ACK", session.SIPAddr(), peerAddr, "direction-outbound", "from-tag", header(final, "to"), 1, "ACK", "z9hG4bK-test"))
		if _, err := session.Wait(); !errors.Is(err, ErrNegotiation) {
			t.Fatalf("Wait err=%v", err)
		}
	})

	t.Run("inbound answer recvonly", func(t *testing.T) {
		peer, rtpPeer := listenUDP(t), listenUDP(t)
		session, err := Call(context.Background(), InboundScenario{
			TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
			ExpectAudio: &AudioExpectation{File: "testdata/sample.wav", MinSimilarity: 0.9, MinCoverage: 0.9}, RingTimeout: time.Second, Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(session.Close)
		invite := readRaw(t, peer, time.Second)
		rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
		body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=recvonly\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
		writeRaw(t, peer, session.SIPAddr(), responseTo(invite, 200, header(invite, "to")+";tag=peer-tag", body))
		ack := readRaw(t, peer, time.Second)
		bye := readRaw(t, peer, time.Second)
		if !strings.HasPrefix(ack.start, "ACK ") || !strings.HasPrefix(bye.start, "BYE ") {
			t.Fatalf("ACK=%s BYE=%s", ack.start, bye.start)
		}
		if _, err := session.Wait(); !errors.Is(err, ErrNegotiation) {
			t.Fatalf("Wait err=%v", err)
		}
	})
}
