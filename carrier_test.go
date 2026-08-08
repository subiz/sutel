package sutel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestExpectOutboundCallBindsConfiguredLocalPort(t *testing.T) {
	reservation, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(reservation.LocalAddr().(*net.UDPAddr).Port)
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}

	call, err := ExpectOutboundCall(context.Background(), OutboundScenario{LocalPort: port})
	if err != nil {
		t.Fatal(err)
	}
	if got := call.SIPAddr().Port(); got != port {
		call.Close()
		t.Fatalf("SIP port=%d, want %d", got, port)
	}
	call.Close()
}

type scriptedSIPRead struct {
	data   string
	source netip.AddrPort
	err    error
}

type scriptedSIPReader struct {
	reads []scriptedSIPRead
	next  int
}

func (reader *scriptedSIPReader) ReadFromUDPAddrPort(buffer []byte) (int, netip.AddrPort, error) {
	read := reader.reads[reader.next]
	reader.next++
	return copy(buffer, read.data), read.source, read.err
}

func TestCarrierReadPumpContinuesAfterTransientReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &Session{ctx: ctx, sipInput: make(chan sipDatagram, 2)}
	source := netip.MustParseAddrPort("127.0.0.1:5060")
	reader := &scriptedSIPReader{reads: []scriptedSIPRead{
		{err: errors.New("transient UDP read failure")},
		{source: source, data: "INVITE sip:1900@127.0.0.1 SIP/2.0\r\nContent-Length: 0\r\n\r\n"},
		{err: net.ErrClosed},
	}}
	carrier := &carrier{
		config: runtimeConfig{MaxSIPMessageBytes: 4096}, active: session, pumpDone: make(chan struct{}),
	}
	go carrier.readPump(reader)

	first := <-session.sipInput
	if !errors.Is(first.err, ErrTransport) {
		t.Fatalf("first datagram error=%v, want ErrTransport", first.err)
	}
	second := <-session.sipInput
	if second.message == nil || second.message.Method != "INVITE" || second.source != source {
		t.Fatalf("pump did not continue to the next datagram: %+v", second)
	}
	<-carrier.pumpDone
}
