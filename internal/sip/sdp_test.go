package sip

import (
	"net/netip"
	"strings"
	"testing"
)

func TestSDPGenerateAndParse(t *testing.T) {
	body, err := GenerateSDP(netip.MustParseAddr("127.0.0.1"), 40000, []uint8{8, 0}, nil, 110, RecvOnly)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSDP(body)
	if err != nil {
		t.Fatalf("parse generated SDP: %v\n%s", err, body)
	}
	if parsed.AudioPort != 40000 || !parsed.HasFormat(8) || !parsed.HasFormat(0) || parsed.TelephoneEvent != 110 || !parsed.CanSend() || parsed.CanReceive() {
		t.Fatalf("unexpected SDP: %+v", parsed)
	}
	if parsed.AnswerDirection() != SendOnly {
		t.Fatalf("answer direction=%v", parsed.AnswerDirection())
	}
}

func TestSDPGeneratesDynamicG711RTPMap(t *testing.T) {
	body, err := GenerateSDP(
		netip.MustParseAddr("127.0.0.1"), 40000, []uint8{96},
		map[uint8]RTPMap{96: {Encoding: "pcma", ClockRate: 8000}}, -1, SendRecv,
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSDP(body)
	if err != nil {
		t.Fatalf("parse generated SDP: %v\n%s", err, body)
	}
	if !parsed.HasFormat(96) || parsed.RTPMaps[96] != (RTPMap{Encoding: "PCMA", ClockRate: 8000}) {
		t.Fatalf("unexpected SDP: %+v\n%s", parsed, body)
	}
}

func TestSDPRejectsDynamicPayloadWithoutMapping(t *testing.T) {
	_, err := GenerateSDP(netip.MustParseAddr("127.0.0.1"), 40000, []uint8{96}, nil, -1, SendRecv)
	if err == nil {
		t.Fatal("generated dynamic payload without rtpmap")
	}
}

func TestSDPMediaConnectionOverridesSession(t *testing.T) {
	body := "v=0\r\no=x 1 1 IN IP4 127.0.0.1\r\ns=x\r\nc=IN IP4 127.0.0.2\r\nt=0 0\r\nm=audio 4567 RTP/AVP 8 112\r\nc=IN IP4 127.0.0.3\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:112 telephone-event/8000\r\na=sendonly\r\n"
	parsed, err := ParseSDP([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Connection.String() != "127.0.0.3" || parsed.TelephoneEvent != 112 || parsed.CanSend() || !parsed.CanReceive() {
		t.Fatalf("unexpected SDP: %+v", parsed)
	}
}

func TestSDPRejectsUnsupportedOrMalformedMedia(t *testing.T) {
	base := "v=0\r\no=x 1 1 IN IP4 127.0.0.1\r\ns=x\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n%s"
	for _, media := range []string{
		"m=video 4000 RTP/AVP 0\r\n",
		"m=audio 0 RTP/AVP 8\r\n",
		"m=audio 4000 RTP/SAVP 8\r\n",
		"m=audio 4000 RTP/AVP bad\r\n",
		"m=audio 4000 RTP/AVP 8\r\na=rtpmap:8 PCMU/8000\r\n",
	} {
		if _, err := ParseSDP([]byte(strings.ReplaceAll(base, "%s", media))); err == nil {
			t.Fatalf("accepted media %q", media)
		}
	}
}

func TestSDPParsesDynamicAudioAndValidatesTelephoneEventFMTP(t *testing.T) {
	body := "v=0\r\no=x 1 1 IN IP4 127.0.0.1\r\ns=x\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 96 112\r\na=rtpmap:96 PCMA/8000\r\na=rtpmap:112 telephone-event/8000\r\na=fmtp:112 0-16\r\n"
	parsed, err := ParseSDP([]byte(body))
	if err != nil || parsed.RTPMaps[96].Encoding != "PCMA" || parsed.TelephoneEvent != 112 || parsed.FMTP[112] != "0-16" {
		t.Fatalf("SDP=%+v err=%v", parsed, err)
	}
	malformed := strings.Replace(body, "0-16", "16-0", 1)
	if _, err := ParseSDP([]byte(malformed)); err == nil {
		t.Fatal("accepted malformed telephone-event fmtp")
	}
}

func TestSDPRejectsAdditionalMediaSectionsInV1(t *testing.T) {
	body := "v=0\r\no=x 1 1 IN IP4 127.0.0.1\r\ns=x\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 8\r\nm=video 5000 RTP/AVP 96\r\n"
	if _, err := ParseSDP([]byte(body)); err == nil {
		t.Fatal("accepted an additional media section outside V1")
	}
}
