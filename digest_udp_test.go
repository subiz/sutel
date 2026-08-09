package sutel

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func digestChallenge401(request rawSIPMessage, to, challenge string) string {
	return fmt.Sprintf(
		"SIP/2.0 401 Unauthorized\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\nWWW-Authenticate: %s\r\nContent-Length: 0\r\n\r\n",
		header(request, "via"), header(request, "from"), to, header(request, "call-id"), header(request, "cseq"), challenge,
	)
}

// testAuthParam extracts one parameter from a Digest Authorization header with
// plain string handling; it deliberately does not reuse the production parser.
func testAuthParam(authorization, name string) string {
	rest := strings.TrimPrefix(strings.TrimSpace(authorization), "Digest ")
	for _, field := range strings.Split(rest, ", ") {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return ""
}

func testViaBranch(message rawSIPMessage) string {
	for _, part := range strings.Split(header(message, "via"), ";") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(part), "branch="); ok {
			return value
		}
	}
	return ""
}

func TestUDPInboundAnswersDigestChallengeAndRetriesOnce(t *testing.T) {
	peer, rtpPeer := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		DigestCredentials: &DigestCredentials{Username: "alice", Password: "wonder land"},
		RingTimeout:       time.Second, CallDuration: 120 * time.Millisecond, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)

	first := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(first.start, "INVITE ") || header(first, "cseq") != "1 INVITE" || header(first, "authorization") != "" {
		t.Fatalf("first INVITE start=%q cseq=%q authorization=%q", first.start, header(first, "cseq"), header(first, "authorization"))
	}
	challengedTo := header(first, "to") + ";tag=challenge-tag"
	challenge := `Digest realm="sutel-trunk", nonce="test-nonce-1", qop="auth", opaque="opaque-7", algorithm=MD5`
	writeRaw(t, peer, sourceFromVia(first), digestChallenge401(first, challengedTo, challenge))

	ack := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(ack.start, "ACK ") || header(ack, "cseq") != "1 ACK" || testViaBranch(ack) != testViaBranch(first) {
		t.Fatalf("challenge ACK start=%q cseq=%q branch=%q want branch %q", ack.start, header(ack, "cseq"), testViaBranch(ack), testViaBranch(first))
	}

	second := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(second.start, "INVITE ") || header(second, "cseq") != "2 INVITE" {
		t.Fatalf("second INVITE start=%q cseq=%q", second.start, header(second, "cseq"))
	}
	if testViaBranch(second) == testViaBranch(first) || testViaBranch(second) == "" {
		t.Fatalf("retried INVITE must use a new branch, got %q", testViaBranch(second))
	}
	authorization := header(second, "authorization")
	requestURI := strings.Fields(second.start)[1]
	hash := func(value string) string { return fmt.Sprintf("%x", md5.Sum([]byte(value))) }
	ha1 := hash("alice:sutel-trunk:wonder land")
	ha2 := hash("INVITE:" + requestURI)
	cnonce := testAuthParam(authorization, "cnonce")
	expected := hash(ha1 + ":test-nonce-1:00000001:" + cnonce + ":auth:" + ha2)
	if cnonce == "" || testAuthParam(authorization, "response") != expected ||
		testAuthParam(authorization, "username") != "alice" ||
		testAuthParam(authorization, "realm") != "sutel-trunk" ||
		testAuthParam(authorization, "uri") != requestURI ||
		testAuthParam(authorization, "opaque") != "opaque-7" {
		t.Fatalf("bad Authorization (want response %s):\n%s", expected, authorization)
	}

	// A retransmitted 401 from the challenge transaction must be re-ACKed even
	// though the retried transaction is already running.
	writeRaw(t, peer, sourceFromVia(first), digestChallenge401(first, challengedTo, challenge))
	staleACK := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(staleACK.start, "ACK ") || header(staleACK, "cseq") != "1 ACK" {
		t.Fatalf("stale 401 was not re-ACKed: start=%q cseq=%q", staleACK.start, header(staleACK, "cseq"))
	}

	answeredTo := header(second, "to") + ";tag=answer-tag"
	writeRaw(t, peer, sourceFromVia(second), responseTo(second, 180, answeredTo, ""))
	rtp := rtpPeer.LocalAddr().(*net.UDPAddr).AddrPort()
	body := fmt.Sprintf("v=0\r\no=peer 1 1 IN IP4 %s\r\ns=Peer\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n", rtp.Addr(), rtp.Addr(), rtp.Port())
	writeRaw(t, peer, sourceFromVia(second), responseTo(second, 200, answeredTo, body))

	dialogACK := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(dialogACK.start, "ACK ") || header(dialogACK, "cseq") != "2 ACK" {
		t.Fatalf("dialog ACK start=%q cseq=%q", dialogACK.start, header(dialogACK, "cseq"))
	}
	bye := readRaw(t, peer, time.Second)
	if !strings.HasPrefix(bye.start, "BYE ") || header(bye, "cseq") != "3 BYE" {
		t.Fatalf("BYE after authenticated INVITE must continue the CSeq space: start=%q cseq=%q", bye.start, header(bye, "cseq"))
	}
	writeRaw(t, peer, session.SIPAddr(), responseTo(bye, 200, header(bye, "to"), ""))

	result, err := session.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.InviteFinalStatus != 200 || !result.Outcome.Established || result.Outcome.TerminatedBy != Sutel {
		t.Fatalf("outcome=%+v", result.Outcome)
	}
}

func TestUDPInboundDoesNotRetrySecondDigestChallenge(t *testing.T) {
	peer, _ := listenUDP(t), listenUDP(t)
	session, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		DigestCredentials: &DigestCredentials{Username: "alice", Password: "secret"},
		RingTimeout:       time.Second, Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)

	challenge := `Digest realm="sutel-trunk", nonce="n1", qop="auth"`
	first := readRaw(t, peer, time.Second)
	writeRaw(t, peer, sourceFromVia(first), digestChallenge401(first, header(first, "to")+";tag=t1", challenge))
	if ack := readRaw(t, peer, time.Second); header(ack, "cseq") != "1 ACK" {
		t.Fatalf("ack=%q", ack.start)
	}
	second := readRaw(t, peer, time.Second)
	if header(second, "cseq") != "2 INVITE" {
		t.Fatalf("second=%q cseq=%q", second.start, header(second, "cseq"))
	}
	writeRaw(t, peer, sourceFromVia(second), digestChallenge401(second, header(second, "to")+";tag=t2", challenge))
	if ack := readRaw(t, peer, time.Second); header(ack, "cseq") != "2 ACK" {
		t.Fatalf("second ack=%q", ack.start)
	}
	if extra, err := readRawError(peer, 300*time.Millisecond); err == nil {
		t.Fatalf("unexpected third request after second 401: %q", extra.start)
	}
	if _, err := session.Wait(); !errors.Is(err, ErrVerification) {
		t.Fatalf("err=%v want ErrVerification", err)
	}
}

func TestUDPInboundRejectsExpectStatus401WithCredentials(t *testing.T) {
	peer := listenUDP(t)
	_, err := Call(context.Background(), InboundScenario{
		TargetSIPAddr: peer.LocalAddr().(*net.UDPAddr).AddrPort(), From: "100", To: "1900", Codec: PCMA,
		DigestCredentials: &DigestCredentials{Username: "alice", Password: "secret"},
		ExpectStatus:      401,
	})
	if err == nil || !strings.Contains(err.Error(), "ExpectStatus 401") {
		t.Fatalf("err=%v", err)
	}
}
