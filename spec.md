## 1. Goal

Build **Sutel**, a local-only telecom carrier simulator for integration testing
SIP-based voice systems. This document calls the system being tested the
**system under test (SUT)**.

Sutel must emulate a real SIP trunk sufficiently to test:

* outbound calls: SUT → Sutel
* inbound calls: Sutel → SUT
* SIP signaling over UDP
* RTP audio
* PCMA / G.711 A-law
* PCMU / G.711 μ-law
* DTMF via RFC4733/2833
* DTMF via SIP INFO
* audio playback, receiving, recording, and content verification
* common carrier failures and timing behaviors

Sutel is implemented as a Go library. It contains a small, purpose-built SIP
user agent and media engine for this test scope. It must not require an
external executable, daemon, container, Internet connection, or cloud service
at runtime.

Sutel is not intended to become a general SIP stack, proxy, registrar, PBX, or
media server.

### 1.1 Requirement language and V1 scope

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are
normative. Declarative requirements are mandatory unless a section is
explicitly marked as future work or non-V1.

The V1 protocol boundary is deliberately narrow:

```text
SIP transport: UDP
network: local IPv4
dialogs per Session: one call
media sections: one audio m-line
audio codecs: PCMA and PCMU
DTMF: RFC4733 and SIP INFO
```

Unsupported protocol features must fail explicitly or be safely ignored as
defined below. They must never cause unbounded waiting, memory growth, or a
panic.

---

## 2. Design Principles

1. **Real network boundary.** The SUT must exchange real SIP and RTP UDP
   datagrams with Sutel; tests must not bypass the network using in-process
   mocks.
2. **Bounded SIP implementation.** Implement only the messages, transactions,
   dialogs, and SDP needed by the declared scenarios.
3. **Deterministic behavior.** Scenario timing, retransmission, media pacing,
   matching, cleanup, and in-memory results must have deterministic contracts.
4. **No runtime tooling dependency.** Normal test execution uses Go code and
   local sockets only.
5. **Useful failures.** A failure must explain the SIP and media state without
   requiring packet-capture expertise.
6. **Isolation by ownership.** Each Session owns its sockets, state, goroutines,
   and in-memory buffers.

---

## 3. Core Architecture

```text
                       Go integration test
                              |
                              v
                  +-----------------------+
                  |    sutel Go package   |
                  +-----------+-----------+
                              |
             +----------------+----------------+
             |                                 |
             v                                 v
      Go SIP test UA                    Go media engine
      UDP transport                     RTP sender/receiver
      parser/serializer                 PCMA/PCMU codec
      transactions                      RFC4733
      dialogs                           WAV playback/recording
      UAC/UAS scenarios                 audio matching
      SDP negotiation
             |                                 |
             +---------------+-----------------+
                             |
                         SIP + RTP
                             |
                             v
                            SUT
```

### SIP test UA responsibilities

* bind and own the SIP UDP socket
* parse and serialize the supported SIP message subset
* run UDP client and server transactions
* maintain one dialog and its route/target state
* implement the supported UAC and UAS call flows
* generate and parse SDP
* validate expected SIP headers and DTMF INFO bodies
* expose structured SIP events and diagnostics

### Media engine responsibilities

* bind and own RTP sockets
* send and receive RTP with 20 ms G.711 packetization
* encode and decode PCMA/PCMU
* send and receive RFC4733 telephone events
* normalize WAV input to PCM16/8000 Hz/mono
* reconstruct received audio from RTP timestamps
* expose received audio as in-memory PCM and WAV
* compare received audio against expected WAV

### Session responsibilities

* validate scenarios and runtime options
* own contexts, deadlines, and cleanup
* coordinate signaling and media state
* own exactly one isolated call endpoint
* return immutable results through the public API

---

## 4. Runtime Constraints

Default bind address: `127.0.0.1`

Everything must run locally. Normal test execution requires no Internet
connection.

SIP transport: `UDP` only

SIP trunk trust model: `trusted IP; no REGISTER or authentication`

The following are not required in V1:

```text
TCP or TLS SIP
SRTP
Digest authentication
REGISTER
DNS discovery
NAT traversal
STUN, TURN, or ICE
IPv6
OPUS
session timers
re-INVITE or UPDATE
PRACK / 100rel
forked dialogs
reusing one endpoint for multiple calls
```

If an incoming request requires an unsupported extension, Sutel should return
an appropriate response such as `420 Bad Extension` or
`501 Not Implemented`, record the reason, and keep the session bounded.

---

## 5. Supported SIP Subset

V1 must support these request methods:

```text
INVITE
ACK
CANCEL
BYE
INFO
OPTIONS
```

`OPTIONS` is supported only as a stateless health/interoperability response and
must not consume the single active-call slot.

V1 must generate or understand these response families as required by the
scenarios:

```text
100 Trying
180 Ringing
183 Session Progress
200 OK
400 Bad Request
404 Not Found
415 Unsupported Media Type
481 Call/Transaction Does Not Exist
486 Busy Here
487 Request Terminated
488 Not Acceptable Here
500 Server Internal Error
501 Not Implemented
503 Service Unavailable
603 Decline
```

The implementation is a direct trunk endpoint. It must support dialog remote
targets from `Contact` and route sets from `Record-Route`/`Route` for the
supported in-dialog requests. It does not act as a proxy and does not forward
requests.

---

## 7. Runtime Configuration

The package must not depend on `testing.T`. There is no public `Config` or
`Carrier`; every public call function creates one isolated endpoint.

Both `OutboundScenario` and `InboundScenario` expose:

```go
LocalIP   string
LocalPort uint16
Timeout   time.Duration
```

`LocalIP` defaults to `127.0.0.1`. It must be a safe local IPv4 address;
unspecified and multicast addresses are rejected. `LocalPort` is the SIP UDP
port; zero asks the OS to select one and is the default. RTP always uses an
OS-selected port advertised through SDP. `Timeout` is the overall call deadline
and defaults to 20 seconds. A negative timeout is invalid.

SIP T1/T2, RTP drain timeout, reorder window, synthesized-gap cap, and maximum
SIP message size are bounded implementation defaults. They are not public API.
Tests may inject an internal fake clock and internal timing configuration.
All signaling behavior deadlines, transaction retransmissions, dialog hangup
timers, and SIP INFO intervals use that clock.

Sutel does not create working or artifact directories and never writes output
files automatically.

---

## 8. Error Contract

Public errors must support `errors.Is`/`errors.As` for at least:

```text
ErrTransport
ErrProtocol
ErrNegotiation
ErrVerification
ErrInvalidExpectation
```

Context expiry must wrap `context.DeadlineExceeded` or `context.Canceled`.
Transport, protocol, negotiation, and verification errors must carry the
scenario name and multiline SIP/RTP/audio diagnostics.

An expected negative carrier behavior is not a Go error. For example, a
`Busy{}` scenario is successful when the expected INVITE receives `486` and
the transaction completes correctly. A mismatched flow is a verification
error.

Malformed or unsupported traffic unrelated to the active Call-ID is recorded
and ignored. Malformed traffic for the active transaction produces a protocol
error or explicit SIP response, depending on whether enough message identity
was parsed to respond safely.

---

## 9. Parallel Isolation and Lifecycle

Approximately ten SUT instances may run simultaneously on different local
ports. Each public call creates its own Session endpoint.

Every Session owns its own:

```text
SIP UDP socket
RTP UDP socket
transaction and dialog state
random Call-ID, tags, branches, SSRC, sequence, and timestamp state
audio buffers
trace and result state
goroutines
```

There must be:

* no global mutable call state
* no hard-coded SIP or RTP port
* no automatic recording or trace file
* no goroutine or socket leak after cleanup

By default the Go process binds SIP and RTP using port `0`; the OS selects the
ports without a reserve-release-bind race. A non-zero scenario `LocalPort`
requests that exact SIP port for manual testing or an externally fixed trunk;
failure to bind it is `ErrTransport`. RTP remains dynamic.

`ExpectOutboundCall` and `Call` bind SIP and RTP sockets before returning. A
session's `SIPAddr()` is therefore immediately ready for the SUT; callers must
not need an arbitrary sleep or a readiness probe. `Close` and `Wait` are
concurrency-safe and idempotent.

On cancellation or cleanup:

1. stop scenario timers and prevent new sends
2. finish any safe terminal response already being written
3. drain RTP for the configured bounded duration
4. close SIP/RTP sockets
5. wait for owned goroutines
6. finalize immutable in-memory results

---

## 10. SIP Message Parsing and Serialization

The parser must be bounded by `MaxSIPMessageBytes` and must never panic on
arbitrary UDP input.

Required parsing behavior:

* distinguish request and status start lines
* parse method, request URI, status code, and reason phrase
* treat header names case-insensitively
* preserve repeated header values and their order where protocol behavior
  depends on order
* accept CRLF line endings; MAY accept LF-only messages for diagnostics
* accept folded continuation lines for interoperability but never emit them
* honor `Content-Length` exactly when present; on UDP, an absent
  `Content-Length` makes the body consume the remainder of the datagram
* reject truncated or conflicting bodies
* recognize common compact names: `v`, `f`, `t`, `i`, `m`, `l`, and `c`
* parse parameters without changing their quoted values

The serializer must:

* always emit CRLF
* produce exactly one correct `Content-Length`
* reject CR/LF injection in caller-supplied values
* preserve required Via, route, dialog, and transaction identifiers
* produce deterministic header order for golden tests

V1 SIP URI support is bounded to direct UDP SIP URIs of the form:

```text
sip:[user@]ipv4[:port][;transport=udp][;parameters]
```

Display names, quoted display names, URI parameters, and header tag parameters
must be parsed sufficiently for normal trunk messages. `sips:`, telephone URIs,
DNS resolution, and arbitrary URI schemes are outside V1.

---

## 11. SIP UDP Transport and Demultiplexing

The transport owns one `net.UDPConn` per Session. It must:

* read complete UDP datagrams
* apply read deadlines so shutdown is prompt
* serialize writes per socket
* retain the actual source endpoint for responses
* demultiplex messages by branch, Call-ID, tags, CSeq, and method
* ignore and diagnose packets from unrelated calls
* expose structured sent/received events

Responses are sent to the received packet source according to Via `rport` and
`received` behavior for local UDP interoperability. Requests use the dialog
remote target and route set when present.

Random identifiers must be safe for parallel tests and unpredictable enough to
avoid collisions. Production code uses a concurrency-safe random source;
internal tests may inject deterministic identifier and clock sources. Failure
of the operating-system random source must not panic; a process-unique fallback
is acceptable for this failure path.

---

## 12. Transactions and Retransmissions

The implementation must provide the supported RFC3261 UDP transaction behavior
without attempting to expose a general transaction library.

Required behavior:

* outbound INVITE retransmits from `SIPT1` with exponential backoff until a
  provisional response, final response, or effective deadline
* a provisional response stops INVITE request retransmission but does not
  complete the transaction
* non-INVITE requests retransmit with bounded backoff up to `SIPT2`
* duplicate requests receive the cached latest response instead of executing
  scenario actions twice
* UAS non-2xx final INVITE responses retransmit until matching ACK or deadline
* UAS 2xx INVITE responses retransmit until the matching dialog ACK or deadline
* UAC sends the correct ACK for both 2xx and non-2xx final INVITE responses
* duplicate final responses cause ACK retransmission without duplicating the
  logical outcome
* CANCEL matches the INVITE branch, Call-ID, From/To tags, and CSeq number
* a valid CANCEL receives `200`; the pending INVITE receives `487`; the `487`
  transaction completes only after ACK
* CANCEL received after a final INVITE response receives `481`

V1 caps the outbound INVITE retransmission interval at `SIPT2`; this is an
intentional bounded simplification from RFC3261 Timer A. After ACKing a UAC
non-2xx final response, Sutel retains a short bounded completed state and
re-ACKs matching retransmissions.

All timer loops must select on the session context. There must be no timer or
goroutine leak after completion.

---

## 13. Dialog State

A dialog is identified by Call-ID plus local and remote tags. Store at least:

```text
Call-ID
local URI and tag
remote URI and tag
local CSeq
remote CSeq
remote Contact target
route set
local and remote signaling endpoints
negotiated SDP/media state
```

In-dialog requests must use monotonically increasing local CSeq except ACK and
CANCEL, which follow SIP transaction rules. An out-of-dialog BYE or INFO gets
`481`. A stale or duplicate in-dialog request must not execute its action
twice; return the cached response when possible.

V1 does not support dialog forking. If multiple distinct 2xx responses create
forked dialogs, fail explicitly and terminate only dialogs that can be safely
identified.

---

## 14. SDP Parsing and Generation

Use `github.com/pion/sdp/v3` for RFC 4566 parsing and serialization. Sutel
keeps a small validation and negotiation layer around it to enforce the V1
boundary below; the dependency does not replace Sutel's codec, direction, or
local-address policy.

V1 supports one `m=audio` section using `RTP/AVP`.

Parse and validate:

```text
v=
c= connection address at session or media level
m=audio <port> RTP/AVP <formats>
a=rtpmap
a=fmtp for telephone-event
a=sendrecv, sendonly, recvonly, inactive
a=ptime when present
```

Required rules:

* reject a missing or malformed audio section and reject additional media
  sections outside the single-audio-m-line V1 boundary
* reject `m=audio 0`
* reject a non-local/invalid media address according to the configured local
  test boundary
* reject no-common-codec offers with `488`
* select only one negotiated audio codec for media transmission
* static payload type `8` means PCMA and `0` means PCMU
* accept PCMA/8000 and PCMU/8000 under a valid dynamic payload type described
  by `rtpmap`; preserve the selected payload type and emit its `rtpmap` in
  answers; reject an invalid remapping of static payloads 8 or 0
* parse and validate telephone-event `fmtp` event lists
* dynamic telephone-event payload type comes from SDP, never from an
  unconditional `101` assumption
* Sutel-originated offers default telephone-event to payload type `101`
* respect the remote media direction; never send to `sendonly`/`inactive` or
  expect media from `recvonly`/`inactive`
* V1 sends 20 ms G.711 packets and advertises `a=ptime:20`

Generated SDP advertises the Go media engine's bound RTP address.

Example:

```text
v=0
o=sutel 1 1 IN IP4 127.0.0.1
s=Sutel
c=IN IP4 127.0.0.1
t=0 0
m=audio 43172 RTP/AVP 8 101
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=sendrecv
```

---

## 15. Public Session API

The effective session deadline is the earliest of the context passed at
creation and the non-zero scenario timeout. If neither provides a deadline,
use the 20-second default; a session may never be unbounded.

```go
type Session struct {
    // private state
}

func (s *Session) SIPAddr() netip.AddrPort
func (s *Session) Wait() (CallResult, error)
func (s *Session) Close()
```

`Wait` is concurrency-safe and idempotent. Every caller receives an independent
deep copy of the same logical result. The creation context controls the entire
call lifetime. `Close` cancels the call, closes owned sockets, joins owned
goroutines, and returns no error; final call errors are obtained from `Wait`.

---

## 16. Outbound Call API

“Outbound” means: SUT ---> Sutel

Sutel acts as the UAS.

```go
type OutboundScenario struct {
    LocalIP   string
    LocalPort uint16
    Timeout   time.Duration

    From string
    To   string

    Behavior OutboundBehavior
    Codecs   []Codec

    DTMF        *DTMFExpectation
    ExpectAudio *AudioExpectation
}

func ExpectOutboundCall(
    ctx context.Context,
    scenario OutboundScenario,
) (*Session, error)
```

An empty codec list defaults to `[]Codec{PCMA, PCMU}`. A nil behavior defaults
to `Answer{}`.

The function validates the scenario, prepares SIP/RTP sockets, installs the UAS
expectation, and returns only when `Session.SIPAddr()` is ready to receive an
INVITE.

---

## 17. Outbound Happy Path

```text
SUT                               Sutel
 |                                  |
 |------------ INVITE ------------->|
 |<----------- 100 Trying ----------|
 |<----------- 180 Ringing ---------|
 |<----------- 200 OK + SDP --------|
 |------------ ACK ---------------->|
 |                                  |
 |============ RTP ================>|
 |                                  |
 |------------ BYE ---------------->|
 |<----------- 200 OK --------------|
```

Required verification includes:

```text
Request-URI
From
To
Contact presence and validity
Call-ID presence
CSeq method and number
top Via branch
Content-Type
SDP presence and negotiation
transaction/dialog sequencing
```

`From` and `To` match the user part of the corresponding SIP URI exactly. An
empty string disables that check. `To` applies to both the `To` header and the
Request-URI. Sutel does not normalize phone numbers, so `091...`, `8491...`,
and `+8491...` are distinct values.

---

## 18. Outbound Behaviors

Mandatory behaviors:

```go
sutel.Answer{}
sutel.Busy{}
sutel.Reject{}
sutel.NotFound{}
sutel.ServiceUnavailable{}
sutel.NoAnswer{}
sutel.Timeout{}
sutel.NetworkLoss{}
sutel.EarlyMedia{}
sutel.EarlyFailure{}
```

`Answer`:

```go
type Answer struct {
    OmitTrying bool

    TryingAfter  time.Duration
    RingingAfter time.Duration
    AnswerAfter  time.Duration
    HangupAfter  time.Duration

    Playback *AudioPlayback
    Echo     *AudioEcho
}
```

All `*After` values are deadlines relative to receipt of the first valid
INVITE, not cumulative sleeps. They must be non-negative and nondecreasing for
enabled messages. `HangupAfter == 0` waits for the SUT to terminate unless
`Playback` is configured; completed playback terminates the dialog locally.

`Playback` sends one normalized WAV to the SUT after ACK, then sends BYE after
the final audio frame finishes its playout interval. A positive `HangupAfter`
remains an upper bound and may terminate playback earlier. `Echo` decodes audio
from the SUT and sends it back after the configured delay with normal RTP
pacing. Echo does not include DTMF. `Playback` and `Echo` are mutually exclusive.

```go
type AudioEcho struct {
    Delay time.Duration
}
```

Error behaviors:

```text
Busy                 -> 486 Busy Here
Reject               -> 603 Decline
NotFound             -> 404 Not Found
ServiceUnavailable   -> 503 Service Unavailable
```

`NoAnswer` sends `100` and `180`, accepts INVITE retransmissions, waits for
CANCEL, sends `200` to CANCEL and `487` to the original INVITE, then requires
the matching ACK.

`Timeout` remains silent after the first matching INVITE, tolerates
retransmissions of that transaction, and ends only through the bounded session
deadline.

`NetworkLoss{After: d}` completes `100 -> 180 -> 200 -> ACK`, then keeps the
established dialog alive for `d` measured from ACK. At that boundary Sutel
closes its SIP endpoint silently, sends no BYE or other terminal message, and
performs bounded RTP/resource cleanup. A completed network-loss scenario makes
`Wait` succeed with `Established == true` and `TerminatedBy == NoParty`, so the
test can independently verify client timeout and recovery behavior. A negative
`After` is invalid; zero drops immediately after ACK.

`EarlyMedia` supports:

```text
INVITE
100
183 + SDP
RTP early media
180 optional
200 + compatible SDP
ACK
```

```go
type EarlyMedia struct {
    File          string
    ProgressAfter time.Duration
    SendRinging   bool
    RingingAfter  time.Duration
    AnswerAfter   time.Duration
    HangupAfter   time.Duration
}
```

The Go media sender starts early media only after sending the valid 183 and
uses the media endpoint negotiated from the SUT's offer.

Some carriers provide early media before a non-2xx final response, for example:

```text
INVITE -> 100 -> 183 + SDP/RTP -> 486 + Reason -> ACK
```

Support this independently from `Busy{}` so the simple negative behaviors keep
their direct final-response semantics:

```go
type EarlyFailure struct {
    File string

    OmitTrying    bool
    TryingAfter   time.Duration
    ProgressAfter time.Duration
    FailureAfter  time.Duration

    FinalStatus  int    // zero defaults to 486; otherwise 400..699
    FinalReason  string // optional reason-phrase override
    ReasonHeader string // optional SIP Reason header
}
```

All timings are relative to the first valid INVITE. `FailureAfter` must not
precede `ProgressAfter`. Sutel sends `183` with compatible SDP, starts RTP only
after that response, stops early RTP before the final response, preserves the
same early-dialog To tag in the final response, and completes the non-2xx
INVITE transaction only after the matching ACK. `ReasonHeader` may represent a
Q.850 cause but must not contain CR or LF.

---

## 19. Codec Negotiation

```go
type Codec int

const (
    PCMA Codec = iota
    PCMU
)
```

Outbound answer rules:

1. Parse formats and `rtpmap` attributes in the SUT offer.
2. Intersect them with `OutboundScenario.Codecs`.
3. Choose the first intersection in scenario preference order.
4. Emit only the negotiated codec and telephone-event mapping in the answer;
   preserve a selected dynamic payload type and its `rtpmap`.
5. If there is no common codec, send `488`, do not start media, and return a
   typed negotiation result.

Inbound offer rules:

1. Offer `InboundScenario.Codec` and telephone-event when required.
2. Require the 200 answer to contain the offered codec.
3. Use the answer's media address, direction, and telephone-event mapping.
4. When a 2xx answer is expected, a malformed or codec-incompatible answer is
   a typed negotiation failure and sends no media.

A mid-call codec switch without a new offer/answer is outside V1 and must fail
explicitly rather than corrupting a recording.

---

## 20. Inbound Calls

“Inbound” means:

```text
Sutel ---> SUT
```

Sutel acts as the UAC.

```go
type InboundScenario struct {
    LocalIP   string
    LocalPort uint16
    Timeout   time.Duration

    TargetSIPAddr netip.AddrPort

    From string
    To   string

    Codec Codec

    Playback    *AudioPlayback
    ExpectAudio *AudioExpectation
    DTMF        []DTMFAction

    ExpectStatus int

    RingTimeout  time.Duration
    CallDuration time.Duration
}

func Call(
    ctx context.Context,
    scenario InboundScenario,
) (*Session, error)
```

Happy path:

```text
Sutel                              SUT
 |------------ INVITE ------------->|
 |<----------- 100 -----------------|
 |<----------- 180 -----------------|
 |<----------- 200 + SDP -----------|
 |------------ ACK ---------------->|
 |============ RTP ================>|
 |------------ BYE ---------------->|
 |<----------- 200 -----------------|
```

`ExpectStatus == 0` means 200. A matching non-2xx final response is a successful
scenario: Sutel sends the transaction ACK and `Wait` returns no error. A
different final status is `ErrVerification`.

`RingTimeout == 0` uses a five-second default capped by the effective session
deadline. `CallDuration == 0` waits for SUT BYE; a positive value causes Sutel
to send BYE that long after ACK. Audio and DTMF actions begin only after dialog
establishment unless an explicit early-media scenario says otherwise.

`Playback` is audio sent from Sutel to the SUT. `ExpectAudio` is audio expected
from the SUT and verified after media drain. Both may be configured for a
full-duplex call.

---

## 21. RTP Media Session

Before advertising SDP, bind a Go UDP media socket using port `0`.

```go
net.ListenUDP("udp", &net.UDPAddr{
    IP:   net.ParseIP(config.LocalIP),
    Port: 0,
})
```

One media session owns:

```text
local RTP socket
negotiated remote media endpoint
audio codec and payload type
telephone-event payload type
sender SSRC/sequence/timestamp
receiver SSRC/source lock
send and receive goroutines
RTP events and statistics
```

The media engine must not guess a remote endpoint. It starts transmission only
after valid SDP negotiation.

V1 does not require RTCP. RTCP-looking or unknown datagrams are counted and
ignored.

---

## 22. RTP Sender

The RTP sender is implemented in Go. It must:

* use RTP version 2
* generate one SSRC per media session
* use a random initial sequence number and timestamp
* packetize G.711 into 160 samples / 160 bytes per 20 ms packet
* increment sequence by one and audio timestamp by 160
* pace packets with a monotonic clock
* avoid unbounded catch-up bursts after scheduler delay
* stop promptly on context cancellation or dialog termination
* record every attempted/successful send in diagnostics

Media pacing may skip a badly overdue send deadline and record a discontinuity;
it must not emit a large burst that makes an integration test unrealistic.

Internal tests should use an injected fake clock and deterministic identifier
source. Public callers do not need to configure them.

---

## 23. Playing WAV Audio

The public API accepts WAV:

```go
type AudioPlayback struct {
    File string
}
```

Pipeline:

```text
uncompressed PCM WAV
        |
        v
decode and downmix
        |
        v
PCM16 mono
        |
        v
deterministic resample to 8000 Hz
        |
        v
PCMA or PCMU encode
        |
        v
Go RTP sender
```

Do not require developers to prepare `.alaw`, `.ulaw`, or packet-capture
fixtures. Do not require an external `ffmpeg` process for normal execution.

Playback completion, cancellation, and BYE ordering must be deterministic. A
configured hangup must not discard the final media packet silently.
Before sending a local BYE, cancel and join playback, echo, and DTMF senders so
no RTP packet can be emitted after dialog termination.

Inbound playback uses `InboundScenario.Playback`; answered outbound playback
uses `Answer.Playback`. `ExpectAudio` always means audio received from the SUT,
regardless of call direction.

For `Answer.Echo`, accepted audio RTP packets are decoded to PCM, queued by
arrival time plus `AudioEcho.Delay`, encoded with the negotiated codec, and
sent using an independent local SSRC/sequence/timestamp stream. The queue is
bounded by the session deadline and receiver packet cap. It must not burst to
compensate for scheduler delay.

---

## 24. RTP Receiver and Parsing

Track:

```text
Version
Marker
PayloadType
SequenceNumber
Timestamp
SSRC
Payload
ArrivalTime
Source
```

Only RTP version 2 packets whose payload type is present in negotiated SDP are
eligible. Malformed datagrams, unknown payload types, RTCP, unexpected sources,
and foreign SSRCs are counted and ignored.

Validate source IP against signaling/SDP expectations. V1 permits an
asymmetric UDP source port. Lock the complete source endpoint after the first
valid audio packet for the selected SSRC so two senders cannot be mixed.

The first valid negotiated audio packet selects the audio SSRC. Automatic SSRC
switching is outside V1.

Use wraparound-safe RTP arithmetic in production. A dedicated wraparound
acceptance suite is not required for V1; packet ordering, duplicates, gaps,
and bounded reconstruction remain required tests.

---

## 25. RTP Reconstruction and Recording

Received audio is reconstructed by RTP timestamp, not UDP arrival order.

Rules:

1. Identify packets by SSRC and extended sequence number.
2. An exact repeat is a duplicate; for conflicting contents, the first packet
   wins and diagnostics record the conflict.
3. Hold a bounded reorder window before classifying packets as missing.
4. Decode payloads to their actual sample count.
5. Fill a positive timestamp gap with PCM silence.
6. Discard exact overlaps and diagnose conflicting overlaps.
7. Cap one synthesized gap at `MaxSynthesizedRTPGap`; a larger jump begins a
   discontinuity without allocating an unbounded buffer.
8. Exclude telephone-event payloads from audio PCM.

Internal representation and exported in-memory WAV:

```text
PCM signed 16-bit
8000 Hz
mono
```

```go
func (r CallResult) ReceivedPCM() []int16
func (r CallResult) ReceivedWAV() []byte
```

Both accessors return independent copies. No received audio produces an empty
slice. The WAV bytes must be directly playable by a normal audio player.

---

## 26. RTP Diagnostics

```go
type RTPStats struct {
    SSRC  uint32
    Codec Codec

    PacketsReceived int
    PacketsAttempted int
    PacketsSent     int
    FailedSends     int
    SkippedFrames   int

    FirstSequence uint16
    LastSequence  uint16

    FirstTimestamp uint32
    LastTimestamp  uint32

    DuplicatePackets  int
    MissingPackets    int
    OutOfOrderPackets int
    IgnoredPackets    int
    MalformedPackets  int
    ForeignSSRC       int
    Discontinuities   int
    ConflictingPackets int
    IncompleteDTMF     int
    CodecSwitches      int

    ReceivedDuration time.Duration
    SentDuration     time.Duration
}
```

Missing packets are diagnostics and do not automatically fail a call unless a
specific expectation requires it. Final missing counts are computed only after
the reorder buffer closes. `LastSequence` and `LastTimestamp` describe the
latest accepted RTP position, not the last UDP arrival. Conflicting overlaps,
incomplete telephone events, and an unnegotiated codec switch remain explicit
diagnostics; a codec switch is also a negotiation failure.

---

## 27. G.711

Implement:

```go
func DecodeALaw(byte) int16
func EncodeALaw(int16) byte
func DecodeMuLaw(byte) int16
func EncodeMuLaw(int16) byte
```

Add exhaustive full-byte-domain decode/encode tests and deterministic signal
quality tests for PCMA and PCMU.

---

## 28. DTMF — RFC4733

### Receiving

Identify a logical telephone event by `(SSRC, RTP timestamp, event ID)`.

Support:

```text
0-9
*
#
A-D
```

Merge start, continuation, end, and repeated end packets. Use the greatest
valid duration and emit one logical event after the first end packet. An event
without an end packet is incomplete: keep it in diagnostics and fail a matching
expectation, but do not report a completed digit.

```go
type DTMFEvent struct {
    Digit     string
    Duration  time.Duration
    Volume    uint8
    Timestamp uint32
}
```

### Sending

The Go RTP sender creates telephone-event packets directly. It must:

* use the negotiated telephone-event payload type and clock rate
* use the media session SSRC and sequence space
* keep one RTP timestamp constant for the event
* send increasing duration values
* set the end bit and repeat the final end packet three times
* preserve configured digit order and interval
* finish or cancel DTMF before terminating the dialog

Sutel must never silently downgrade RFC4733 to SIP INFO.

---

## 29. DTMF — SIP INFO

Support:

```text
Content-Type: application/dtmf-relay

Signal=5
Duration=160
```

Parse field names case-insensitively with optional surrounding whitespace.
Reject missing, duplicated, or invalid fields.

Each valid in-dialog INFO receives `200 OK`. An unsupported content type
receives `415`; malformed content receives `400`; an INFO outside the active
dialog receives `481`. Unexpected digits fail the declared expectation after
the protocol response has been sent.

```go
type DTMFExpectation struct {
    Method DTMFMethod
    Digits string
}

type DTMFAction struct {
    Method   DTMFMethod
    Digits   string
    Interval time.Duration
}
```

Sending INFO uses increasing in-dialog CSeq values and waits for the matching
final response before sending the next digit. Internally each collected event
retains its method and arrival time: verification filters by method without
depending on append order, unexpected digits from another method fail the
expectation, and `CallResult.DTMFEvents()` is chronological.

---

## 30. Audio Input and Normalization

V1 accepts standard uncompressed PCM `.wav` files.

The decoder must support common PCM bit depths and mono/stereo input, reject
malformed chunks safely, and produce:

```text
PCM16
8000 Hz
mono
```

Use one deterministic pure-Go resampler for expected audio and playback audio.
The V1 linear resampler intentionally has no anti-alias low-pass filter, so
content above the destination Nyquist frequency may alias when downsampling.
No MP3 support or external transcoder is required.

---

## 31. Audio Expectation API

```go
type AudioExpectation struct {
    File string

    MinSimilarity float64
    MinCoverage   float64

    Match AudioMatchOptions
}

type AudioMatchOptions struct {
    MaxAlignmentOffset   time.Duration
    FrameDuration        time.Duration
    FrameMatchThreshold  float64
    SilenceThresholdDBFS float64
}

func VerifyAudio(expectation AudioExpectation, receivedWAV []byte) (AudioMatchResult, error)
```

Both call directions use the same field name:

```go
ExpectAudio *AudioExpectation
```

It always describes audio received from the SUT. Playback sent by Sutel is a
separate field or answer behavior.

`VerifyAudio` is the standalone form for tests running inside the SUT. The SUT
captures the audio decoded by its own receive path as an uncompressed PCM WAV
in memory and supplies its encoded bytes as `receivedWAV`; no temporary file is
required. The function must use exactly the same normalization, alignment,
scoring, and thresholds as scenario
`ExpectAudio`. It returns the populated result together with `ErrVerification`
when a threshold is missed; an invalid expected fixture returns
`ErrInvalidExpectation`.

Zero-valued options select documented defaults. Explicit similarity and
coverage thresholds must be finite in `[0,1]`; frame threshold in `(0,1]`;
silence threshold in `[-96,0)` dBFS; durations greater than zero.

Do not compare encoded bytes or unaligned PCM sample-by-sample. Matching must
tolerate initial silence, startup delay, modest gain differences, deterministic
resampling differences, and G.711 quantization. `VerifyAudio` input may come
from a SUT whose own receive path decoded a lossy codec Sutel does not
negotiate: matching must also tolerate Opus-at-VoIP-bitrate quantization and
the constant sub-grid decoder lookahead delay such a capture carries.

---

## 32. Audio Alignment and Matching

Normative pipeline:

```text
expected WAV -> PCM16/8k/mono -> normalize ---------+
                                                    +-> align -> windows
received RTP -> G.711 decode -> reconstructed PCM --+              |
                                                                   v
                                                        similarity + coverage
```

`normalize` converts PCM16 to deterministic floating point, removes DC mean,
and does not peak-normalize, compress, or clip.

Initial defaults:

```go
MaxAlignmentOffset    = 2 * time.Second
FrameDuration         = 100 * time.Millisecond
FrameMatchThreshold   = 0.80
SilenceThresholdDBFS  = -45.0
```

Alignment searches within `[-MaxAlignmentOffset,+MaxAlignmentOffset]`. A
positive offset means matching content starts later in received audio. The
search grid is 10 ms, followed by a deterministic one-sample refinement
within one grid step so a constant sub-grid delay — for example the ~6.5 ms
codec lookahead left in audio a SUT decoded from an Opus stream — cannot
destroy waveform correlation. The
alignment method must use signal content as well as a short-time energy
envelope so periodic material does not select a high-energy but phase-wrong
offset. Ties choose the smallest absolute offset, then the earlier offset.

If expected audio has no active content, return a typed invalid expectation.
If received audio has no comparable active content, similarity and coverage
are finite zero values.

Window algorithm:

1. Split expected audio into `FrameDuration` windows.
2. Ignore a final partial window smaller than half a frame.
3. Mark expected windows active using `SilenceThresholdDBFS`.
4. Map each active window to received audio using the alignment offset.
5. Independently remove per-window DC and RMS-normalize comparable windows.
6. Compute normalized correlation, clamped to `[0,1]`.
7. A frame matches when score is at least `FrameMatchThreshold`.

```text
Coverage   = matched active expected windows / all active expected windows
Similarity = mean score across comparable active windows
```

Missing expected windows lower coverage but are excluded from the similarity
mean. Extra received prefix/suffix audio does not increase either metric.

---

## 33. Audio Match Result and Diagnostics

```go
type AudioMatchResult struct {
    Similarity float64
    Coverage   float64

    AlignmentOffset  time.Duration
    ExpectedDuration time.Duration
    ReceivedDuration time.Duration
    ComparedDuration time.Duration
}
```

All metrics must be finite and clamped to `[0,1]`.

A failure must show:

```text
expected file
required and actual similarity
required and actual coverage
codec
expected and received duration
alignment offset
RTP packet counts
```

---

## 34. Call Result

```go
type SIPStats struct {
    LocalAddr  netip.AddrPort
    RemoteAddr netip.AddrPort

    RequestsSent      int
    RequestsReceived  int
    ResponsesSent     int
    ResponsesReceived int
    Retransmissions   int
    MalformedMessages int
    IgnoredMessages   int
}

type CallResult struct {
    Direction CallDirection

    SIP     SIPStats
    Outcome SIPOutcome
    RTP     RTPStats
    Media   NegotiatedMedia
    Audio   *AudioMatchResult

    StartedAt time.Time
    EndedAt   time.Time
}

type SIPOutcome struct {
    InviteFinalStatus int
    Established       bool
    Canceled          bool
    TerminatedBy      CallParty
}

type NegotiatedMedia struct {
    AudioCodec Codec

    // -1 when telephone-event was not negotiated.
    TelephoneEventPayloadType int
}

func (r CallResult) DTMFEvents() []DTMFEvent
func (r CallResult) ReceivedPCM() []int16
func (r CallResult) ReceivedWAV() []byte
func (r CallResult) Events() []Event
func (r CallResult) SIPTrace() string
```

`InviteFinalStatus` is zero when no final response was observed. Slice
accessors return copies. Repeated `Wait` calls return deep copies so callers
cannot mutate shared internal state.

---

## 35. Unified Event Timeline

Expose signaling and media events relative to Sutel:

```text
00.000 SIP <- INVITE
00.001 SIP -> 100 Trying
00.104 SIP -> 180 Ringing
00.308 SIP -> 200 OK
00.314 SIP <- ACK
00.327 RTP <- PT=8 seq=1193 ts=58240
03.442 SIP <- BYE
03.443 SIP -> 200 OK
```

```go
type Event struct {
    Time      time.Time
    Layer     EventLayer
    Direction EventDirection
    Type      string
    Detail    string
}
```

`EventDirection` is `Sent` or `Received` relative to Sutel and is distinct
from call direction. Events must not expose mutable payload buffers.

---

## 36. In-Memory Data

Sutel never creates artifact directories or writes output files automatically.
`CallResult` owns received PCM, WAV bytes, structured events, DTMF events, and
a human-readable SIP trace in memory. Slice accessors return deep copies.

Empty data returns an empty slice or string without an error. Callers that want
a file explicitly write `ReceivedWAV()` or serialize `Events()` themselves.
Raw audio and SIP data may be sensitive and must not be printed on successful
calls.

---

## 38. Required Scenarios — V1

### Outbound

```text
1. normal answer PCMA
2. normal answer PCMU
3. busy 486
4. reject 603
5. not found 404
6. service unavailable 503
7. no answer + CANCEL/487/ACK
8. Sutel hangs up with BYE
9. SUT hangs up with BYE
10. 183 early media
11. no common codec returns 488
12. timeout with INVITE retransmissions
13. 183 early media followed by 486 and non-2xx ACK
14. answer then play WAV to SUT without a receive-audio assertion
15. answer then echo received audio after a configured delay
```

### Inbound

```text
1. normal call PCMA
2. normal call PCMU
3. Sutel sends WAV audio
4. Sutel hangs up
5. SUT hangs up
6. SUT rejects the offered codec
7. provisional response followed by final answer
8. duplicate final response and ACK retransmission
9. expected non-2xx final status succeeds; a mismatched status fails
10. SUT sends expected WAV audio to Sutel
11. simultaneous playback and expected receive audio
```

### DTMF

```text
1. SUT -> Sutel RFC4733
2. SUT -> Sutel SIP INFO
3. Sutel -> SUT RFC4733
4. Sutel -> SUT SIP INFO
```

### Media

```text
1. RTP send and receive
2. PCMA encode/decode
3. PCMU encode/decode
4. WAV playback and recording
5. expected-WAV comparison
6. received WAV, PCM, events, and SIP trace exposed from memory
```

---

## 39. Test Strategy and Acceptance

Tests must validate independent layers rather than relying only on Sutel
talking to itself.

### SIP parser/serializer tests

Use literal packet fixtures and golden serialized messages. Required cases:

```text
request and response start lines
case-insensitive and compact headers
repeated Via/Route headers
quoted display names and tags
Content-Length/body boundaries
folded input headers
malformed and oversized datagrams
CR/LF injection rejection
round-trip semantic preservation
```

### Transaction/dialog tests

Use a fake clock and deterministic identifiers. Required cases:

```text
INVITE retransmission and provisional stop
2xx and non-2xx ACK behavior
duplicate request cached response
duplicate final response ACK
CANCEL/200/487/ACK
BYE in both directions
INFO sequencing
stale CSeq
wrong branch/tag/Call-ID ignored or rejected
context cancellation at every major state
```

### Black-box UDP scenario tests

Use a small scripted test peer whose SIP datagrams are literal test fixtures;
it must not reuse the production parser, transaction, or dialog implementation
to decide whether responses are correct.

Run the required UAC and UAS scenarios over real UDP sockets. Validate the raw
messages observed by the scripted peer and the public `CallResult`.
An established `NetworkLoss` case must prove that no BYE is emitted and that
the SIP socket and owned goroutines are closed after the configured boundary.

### RTP tests

Required deterministic cases:

```text
PCMA and PCMU packets
sequence/timestamp increment
duplicate and out-of-order packets
timestamp gap filled with silence
oversized discontinuity
reorder flush on shutdown
dynamic telephone-event payload
repeated and incomplete telephone events
foreign SSRC/source ignored
unknown payload and malformed RTP ignored
sender packetization and monotonic pacing
```

Dedicated wraparound tests are not required for V1, but the implementation
must use wrap-safe arithmetic.

### Audio tests

Required cases:

```text
exact audio
100 ms and 500 ms leading silence
gain -6 dB
G.711 round-trip
first 80% and first 50% only
wrong audio
silence and random noise
silence-only expected fixture
extra prefix/suffix
periodic audio alignment
no comparable windows
silence threshold boundary
sub-grid constant delay (Opus lookahead shape) aligned by refinement
Opus-degraded capture fixture through VerifyAudio
provided testdata/sample.wav through WAV -> G.711 -> RTP -> match
```

Do not tune thresholds against only one fixture.

---

## 40. Parallelism Acceptance

Launch at least ten isolated Session/peer pairs concurrently over real UDP.

Verify:

```text
no SIP or RTP port collisions
no cross-call signaling or media
no shared buffers
no leaked sockets or goroutines
all results identify their own endpoints
```

Run the full suite with:

```text
go test -race ./...
```

---

## 41. Logging and Failure Output

Successful tests should not spam stdout.

A failure should summarize:

```text
scenario and state
local/remote SIP addresses
last relevant SIP messages
final INVITE status
retransmission counts
negotiated SDP/media
RTP statistics
DTMF events
audio metrics
```

Example:

```text
Sutel verification failed

scenario: outbound-answer
SIP: INVITE received; 100/180/200 sent; ACK received
RTP: PCMA, 157 packets, 3.14s
audio: similarity 96.8%, coverage 63.4%
required: similarity >= 95%, coverage >= 80%
```

---

## 43. Non-Goals

Do not implement these in V1:

```text
a reusable/full RFC3261 stack
SIP proxy or registrar
REGISTER or Digest authentication
TCP, TLS, or WebSocket SIP
SRTP
DNS routing
IPv6
NAT simulation
STUN, TURN, or ICE
OPUS or media transcoding service
re-INVITE, UPDATE, PRACK, or session timers
forking
conference calls or queues
reusing one endpoint for multiple calls
load testing or thousands of calls
production carrier connectivity
GUI, HTTP API, database, or cloud service
speech recognition or AI audio comparison
packet-loss/jitter simulation
```

The internal SIP code exists only to drive the scenarios in this document.
Avoid generic extension points unless a real V1 behavior needs them.

---

## 44. Future-Friendly Areas — Not V1

The architecture may later support:

* observed carrier profiles with header/SDP/timing differences
* sanitized packet-capture import for regression fixtures
* TCP/TLS transport
* authentication
* additional codecs
* an explicit multi-dialog endpoint API
* explicit symmetric-RTP profiles

Do not invent carrier-specific behavior before observing it.

---

## 48. Engineering Principle

Sutel owns only the SIP and media behavior required to make integration tests
deterministic and realistic.

When adding a feature, ask:

```text
Is this required by a concrete SUT integration scenario?
Can it be implemented as a bounded transaction/dialog behavior?
Can it be tested with literal packets and a black-box UDP peer?
```

If not, it probably does not belong in Sutel V1.
