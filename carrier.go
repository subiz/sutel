package sutel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/subiz/sutel/internal/sip"
	"github.com/subiz/sutel/rtpverifier"
)

type carrier struct {
	config  runtimeConfig
	conn    *net.UDPConn
	sipAddr netip.AddrPort
	clock   transactionClock
	ids     *sip.IDGenerator

	writeMu   sync.Mutex
	mu        sync.Mutex
	active    *Session
	closed    bool
	closeOnce sync.Once
	pumpDone  chan struct{}
}

func newCarrier(config runtimeConfig) (*carrier, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	localIP := netip.MustParseAddr(config.LocalIP)
	connection, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(localIP, config.LocalPort)))
	if err != nil {
		return nil, fmt.Errorf("%w: bind SIP UDP: %v", ErrTransport, err)
	}
	carrier := &carrier{
		config: config, conn: connection,
		sipAddr: connection.LocalAddr().(*net.UDPAddr).AddrPort(),
		clock:   realTransactionClock{}, ids: sip.NewIDGenerator(nil), pumpDone: make(chan struct{}),
	}
	go carrier.readPump(connection)
	return carrier, nil
}

// ExpectOutboundCall creates an isolated endpoint and waits for one call from
// the SUT matching scenario.
func ExpectOutboundCall(ctx context.Context, scenario OutboundScenario) (*Session, error) {
	config, err := newRuntimeConfig(scenario.LocalIP, scenario.LocalPort)
	if err != nil {
		return nil, fmt.Errorf("outbound scenario: %w", err)
	}
	owner, err := newCarrier(config)
	if err != nil {
		return nil, err
	}
	session, err := owner.ExpectOutboundCall(ctx, scenario)
	if err != nil {
		owner.shutdown()
		return nil, err
	}
	return session, nil
}

// Call creates an isolated endpoint and places one call to the SUT.
func Call(ctx context.Context, scenario InboundScenario) (*Session, error) {
	config, err := newRuntimeConfig(scenario.LocalIP, scenario.LocalPort)
	if err != nil {
		return nil, fmt.Errorf("inbound scenario: %w", err)
	}
	owner, err := newCarrier(config)
	if err != nil {
		return nil, err
	}
	session, err := owner.Call(ctx, scenario)
	if err != nil {
		owner.shutdown()
		return nil, err
	}
	return session, nil
}

func (c *carrier) ExpectOutboundCall(ctx context.Context, scenario OutboundScenario) (*Session, error) {
	if scenario.Behavior == nil {
		scenario.Behavior = Answer{}
	}
	if len(scenario.Codecs) == 0 {
		scenario.Codecs = []Codec{PCMA, PCMU}
	}
	if err := scenario.validate(); err != nil {
		return nil, fmt.Errorf("outbound scenario: %w", err)
	}
	session, err := c.beginSession(ctx, scenario.Timeout, Outbound)
	if err != nil {
		return nil, err
	}
	session.outbound = &scenario
	go session.execute()
	return session, nil
}

func (c *carrier) Call(ctx context.Context, scenario InboundScenario) (*Session, error) {
	if scenario.RingTimeout == 0 {
		scenario.RingTimeout = 5 * time.Second
	}
	if err := scenario.validate(); err != nil {
		return nil, fmt.Errorf("inbound scenario: %w", err)
	}
	session, err := c.beginSession(ctx, scenario.Timeout, Inbound)
	if err != nil {
		return nil, err
	}
	session.inbound = &scenario
	go session.execute()
	return session, nil
}

func (c *carrier) beginSession(parent context.Context, timeout time.Duration, direction CallDirection) (*Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("carrier is closed")
	}
	if c.active != nil {
		return nil, fmt.Errorf("endpoint already has a session")
	}
	if parent == nil {
		parent = context.Background()
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	receiver, err := rtpverifier.New(rtpverifier.Config{
		LocalIP:               netip.MustParseAddr(c.config.LocalIP),
		AudioPayloads:         map[uint8]rtpverifier.Codec{8: rtpverifier.PCMA, 0: rtpverifier.PCMU},
		TelephoneEventPayload: 101,
		ReorderWindow:         c.config.RTPReorderWindow,
		MaxSynthesizedGap:     c.config.MaxSynthesizedRTPGap,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	session := &Session{
		carrier: c, direction: direction, ctx: ctx, cancel: cancel,
		receiver: receiver, sender: rtpverifier.NewSender(receiver), sipAddr: c.sipAddr,
		startedAt: c.clock.Now(), done: make(chan struct{}), sipInput: make(chan sipDatagram, 256),
		media: NegotiatedMedia{TelephoneEventPayloadType: -1},
	}
	session.sipStats.LocalAddr = c.sipAddr
	c.active = session
	return session, nil
}

type sipPacketReader interface {
	ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error)
}

func (c *carrier) readPump(reader sipPacketReader) {
	defer close(c.pumpDone)
	buffer := make([]byte, c.config.MaxSIPMessageBytes+1)
	consecutiveReadErrors := 0
	for {
		length, source, err := reader.ReadFromUDPAddrPort(buffer)
		if err != nil {
			c.mu.Lock()
			closed, active := c.closed, c.active
			c.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			if active != nil {
				active.enqueueSIP(sipDatagram{err: fmt.Errorf("%w: read SIP UDP: %v", ErrTransport, err)})
			}
			// A UDP read may report a transient network error. Keep the sole Carrier
			// reader alive; it exits only when the socket is closed. Back off after
			// repeated errors so a permanent descriptor failure cannot busy-loop.
			consecutiveReadErrors++
			if consecutiveReadErrors > 1 {
				time.Sleep(min(time.Duration(consecutiveReadErrors-1)*time.Millisecond, 50*time.Millisecond))
			}
			continue
		}
		consecutiveReadErrors = 0
		message, parseErr := sip.Parse(buffer[:length], c.config.MaxSIPMessageBytes)
		if parseErr == nil && message.IsRequest() && strings.EqualFold(message.Method, "OPTIONS") {
			response := responseFor(message, 200, "", nil)
			applyResponseVia(response, source)
			if data, serializeErr := response.Bytes(); serializeErr == nil {
				_ = c.send(data, source)
			}
			continue
		}

		c.mu.Lock()
		active := c.active
		c.mu.Unlock()
		if active != nil {
			active.enqueueSIP(sipDatagram{message: message, source: source, parseErr: parseErr})
		}
	}
}

func (c *carrier) send(data []byte, target netip.AddrPort) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.conn.WriteToUDPAddrPort(data, target); err != nil {
		return fmt.Errorf("%w: write SIP UDP: %v", ErrTransport, err)
	}
	return nil
}

func (c *carrier) shutdown() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		_ = c.conn.Close()
		<-c.pumpDone
	})
}

// Close exists only on the unexported transport for internal tests. Public
// callers close the owning Session.
func (c *carrier) Close() error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active != nil {
		active.cancel()
		<-active.done
	} else {
		c.shutdown()
	}
	return nil
}
