package sip

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseRequestCompactRepeatedAndFoldedHeaders(t *testing.T) {
	raw := "INVITE sip:1900@127.0.0.1:5060 SIP/2.0\r\n" +
		"v: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK-a\r\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5080;branch=z9hG4bK-b\r\n" +
		"f: \"Alice Smith\" <sip:100@127.0.0.1>;tag=from-a\r\n" +
		"t: <sip:1900@127.0.0.1>\r\n" +
		"i: call-a\r\nCSeq: 7 INVITE\r\n" +
		"X-Long: first\r\n\tsecond\r\nContent-Length: 4\r\n\r\ntest"
	message, err := Parse([]byte(raw), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if message.Method != "INVITE" || len(message.Values("Via")) != 2 || message.Get("Call-ID") != "call-a" || message.Get("X-Long") != "first second" || string(message.Body) != "test" {
		t.Fatalf("unexpected message: %+v", message)
	}
	from, err := ParseAddress(message.Get("From"))
	if err != nil || from.DisplayName != "Alice Smith" || from.Tag != "from-a" || from.URI.User != "100" {
		t.Fatalf("unexpected From: %+v err=%v", from, err)
	}
}

func TestParseResponseAndRoundTrip(t *testing.T) {
	raw := []byte("SIP/2.0 486 Busy Here\r\nVia: SIP/2.0/UDP 127.0.0.1;branch=z9hG4bK-a\r\nCall-ID: c\r\nCSeq: 1 INVITE\r\nContent-Length: 0\r\n\r\n")
	message, err := Parse(raw, len(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := message.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, encoded) {
		t.Fatalf("round trip mismatch:\n%s", encoded)
	}
}

func TestParseRejectsBadBoundariesAndOversize(t *testing.T) {
	tests := []string{
		"INVITE sip:a@127.0.0.1 SIP/2.0\r\nContent-Length: 4\r\n\r\nabc",
		"INVITE sip:a@127.0.0.1 SIP/2.0\r\nContent-Length: 0\r\nContent-Length: 1\r\n\r\n",
		"INVITE sip:a@127.0.0.1 SIP/2.0\r\nBad\r\n\r\n",
	}
	for _, raw := range tests {
		if _, err := Parse([]byte(raw), 4096); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Parse(%q) err=%v", raw, err)
		}
	}
	if _, err := Parse([]byte(strings.Repeat("x", 20)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize err=%v", err)
	}
}

func TestSerializerRejectsInjection(t *testing.T) {
	message := NewRequest("OPTIONS", "sip:a@127.0.0.1")
	message.Add("X-Test", "ok\r\nInjected: true")
	if _, err := message.Bytes(); !errors.Is(err, ErrMalformed) {
		t.Fatalf("injection err=%v", err)
	}
}

func TestURIAddressCSeqAndTarget(t *testing.T) {
	address, err := ParseAddress("\"A B\" <sip:100@127.0.0.1:5070;transport=udp>;tag=t1;x=y")
	if err != nil || address.URI.Port != 5070 || address.Tag != "t1" {
		t.Fatalf("address=%+v err=%v", address, err)
	}
	if _, err := ParseURI("sip:a@example.com"); err == nil {
		t.Fatal("DNS URI accepted")
	}
	message := NewRequest("BYE", address.RawURI)
	message.Add("Contact", "<sip:100@127.0.0.1:5070>")
	message.Add("CSeq", "42 BYE")
	cseq, err := message.CSeq()
	if err != nil || cseq.Number != 42 || cseq.Method != "BYE" {
		t.Fatalf("cseq=%+v err=%v", cseq, err)
	}
	if got := RequestTarget(message, netip.MustParseAddrPort("127.0.0.1:1")); got.String() != "127.0.0.1:5070" {
		t.Fatalf("target=%s", got)
	}
}

func TestParserAcceptsUDPBodyWithoutContentLengthAndLFOnly(t *testing.T) {
	message, err := Parse([]byte("OPTIONS sip:127.0.0.1:5060 SIP/2.0\nContent-Type: text/plain\n\nbody"), 1024)
	if err != nil || string(message.Body) != "body" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestCSeqAcceptsRFCBoundary(t *testing.T) {
	for _, value := range []string{"0 INVITE", "2147483647 BYE"} {
		if _, err := ParseCSeq(value); err != nil {
			t.Fatalf("ParseCSeq(%q): %v", value, err)
		}
	}
	if _, err := ParseCSeq("2147483648 INVITE"); err == nil {
		t.Fatal("accepted CSeq above RFC range")
	}
}

func TestUserlessURIAndSIPQuotedPair(t *testing.T) {
	uri, err := ParseURI("sip:127.0.0.1:5062;lr")
	if err != nil || uri.User != "" || uri.Host != "127.0.0.1" || uri.Port != 5062 {
		t.Fatalf("URI=%+v err=%v", uri, err)
	}
	address, err := ParseAddress(`"A\qB" <sip:127.0.0.1:5062>;tag=t`)
	if err != nil || address.DisplayName != "AqB" {
		t.Fatalf("address=%+v err=%v", address, err)
	}
}

func TestCommaSeparatedRouteValuesAreExpanded(t *testing.T) {
	raw := []byte("OPTIONS sip:127.0.0.1 SIP/2.0\r\nRoute: <sip:127.0.0.1:5070;lr>, <sip:127.0.0.1:5080;lr>\r\nContent-Length: 0\r\n\r\n")
	message, err := Parse(raw, 1024)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"<sip:127.0.0.1:5070;lr>", "<sip:127.0.0.1:5080;lr>"}
	if got := message.Values("Route"); !slices.Equal(got, want) {
		t.Fatalf("Route=%v want %v", got, want)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestIdentifierGeneratorIsInjectableAndNeverPanics(t *testing.T) {
	generator := NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0xab}, 24)))
	if got := generator.Tag(); got != "t-"+strings.Repeat("ab", 12) {
		t.Fatalf("deterministic tag=%q", got)
	}
	fallback := NewIDGenerator(failingReader{})
	if first, second := fallback.ID("x-"), fallback.ID("x-"); first == second {
		t.Fatalf("fallback identifiers collided: %q", first)
	}
}

func TestParseAddressHandlesQuotedDelimitersAndEscapes(t *testing.T) {
	address, err := ParseAddress(`"Bob; the \"Builder\" <lead>" <sip:100@127.0.0.1:5070>;tag="tag;one";x="a;\"b=c"`)
	if err != nil {
		t.Fatal(err)
	}
	if address.DisplayName != `Bob; the "Builder" <lead>` || address.URI.User != "100" {
		t.Fatalf("address=%+v", address)
	}
	if address.Tag != `"tag;one"` || address.Parameters["x"] != `"a;\"b=c"` {
		t.Fatalf("parameters=%v", address.Parameters)
	}
}

func TestSerializeBuiltMessageGolden(t *testing.T) {
	message := NewRequest("OPTIONS", "sip:1900@127.0.0.1:5060")
	message.Add("Via", "SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK-golden")
	message.Add("Call-ID", "golden")
	message.Add("CSeq", "1 OPTIONS")
	message.Body = []byte("ping")
	encoded, err := message.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := "OPTIONS sip:1900@127.0.0.1:5060 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5070;branch=z9hG4bK-golden\r\nCall-ID: golden\r\nCSeq: 1 OPTIONS\r\nContent-Length: 4\r\n\r\nping"
	if string(encoded) != want {
		t.Fatalf("serialized message:\n%s\nwant:\n%s", encoded, want)
	}
}

func TestRepeatedRoutesPreserveOrderAndSizeBoundary(t *testing.T) {
	raw := []byte("OPTIONS sip:1900@127.0.0.1:5060 SIP/2.0\r\nRoute: <sip:a@127.0.0.1:5070;lr>\r\nRoute: <sip:b@127.0.0.1:5080;lr>\r\nRecord-Route: <sip:c@127.0.0.1:5090;lr>\r\nRecord-Route: <sip:d@127.0.0.1:5100;lr>\r\nContent-Length: 0\r\n\r\n")
	message, err := Parse(raw, len(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Values("Route"); !slices.Equal(got, []string{"<sip:a@127.0.0.1:5070;lr>", "<sip:b@127.0.0.1:5080;lr>"}) {
		t.Fatalf("Route=%v", got)
	}
	if got := message.Values("Record-Route"); !slices.Equal(got, []string{"<sip:c@127.0.0.1:5090;lr>", "<sip:d@127.0.0.1:5100;lr>"}) {
		t.Fatalf("Record-Route=%v", got)
	}
	encoded, err := message.Bytes()
	if err != nil || !bytes.Equal(encoded, raw) {
		t.Fatalf("round trip err=%v:\n%s", err, encoded)
	}
	if _, err := Parse(raw, len(raw)-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size boundary err=%v", err)
	}
}
