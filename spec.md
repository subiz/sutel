# Sutel — Local Telecom Carrier Integration Test Framework

## 1. Goal

Build **Sutel**, a local-only telecom carrier simulator for integration testing
any SIP-based voice system. This document calls that system the **system under
test (SUT)**.

The system must emulate a real SIP trunk/carrier sufficiently to test:

* Outbound calls: SUT → Sutel
* Inbound calls: Sutel → SUT
* SIP signaling over UDP
* RTP audio
* PCMA / G.711 A-law
* PCMU / G.711 μ-law
* DTMF via RFC 4733/2833
* DTMF via SIP INFO
* Audio playback
* Audio receiving and recording
* Audio content verification
* Common carrier failure scenarios
* SIP edge cases and future carrier-specific quirks

The implementation must use:

* **SIPp** as the SIP signaling/scenario engine.
* **Go** as the test harness.
* **Go RTP verifier** for receiving, decoding, recording, and verifying RTP audio.

Do NOT implement a SIP stack from scratch.

### 1.1 Requirement language and V1 scope

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY** are
normative. Declarative requirements are mandatory unless the section is
explicitly marked as future work. Code and text explicitly labeled
“suggested”, “recommended”, or “example” are non-normative unless a later
requirement makes them mandatory.

V1 means the complete set of requirements in:

* Required Scenarios — V1
* Definition of Done
* the five implementation milestones

Milestones describe delivery order, not optional scope. In particular, 183
early media and sending RFC4733 DTMF are not required in Milestone 1, but they
are required before V1 is complete.

Features under sections explicitly named `NOT V1` or `Future-Friendly` are
design constraints only and do not need an implementation in V1.

The V1 baseline is:

* minimum and CI-pinned SIPp version: `3.7.7`
* RTP streaming support for media playback
* PCAP playback support for carrier-to-SUT RFC4733

The repository's dependency/CI configuration must repeat this exact version and
the build capabilities required by each scenario. Upgrading SIPp requires the
scenario and parallelism acceptance suites to pass against the new pin.

The full V1 acceptance suite requires a SIPp build with PCAP playback support
because carrier-to-SUT RFC4733 playback is mandatory. A smaller test run may
omit that scenario only through an explicit test-helper capability check; the
core library must return a capability error and must never silently skip or
downgrade it.

---

## 2. Core Architecture

```text
                         Go integration test
                                |
                                v
                    +-----------------------+
                    |   sutel Go package    |
                    +-----------+-----------+
                                |
                    +-----------+-----------+
                    |                       |
                    v                       v
             SIPp process            Go RTP Verifier
             SIP signaling           RTP receiving
             SIP assertions          PCMA/PCMU decode
             UAC / UAS               RFC4733 decode
             failures                WAV recording
             DTMF SIP INFO            audio matching
             RTP playback
                    |                       |
                    | SIP               RTP |
                    +-----------+-----------+
                                |
                                v
                         SUT
```

Responsibilities must be strictly separated.

### SIPp is responsible for

* SIP UDP transport
* INVITE / ACK / BYE / CANCEL
* 100 Trying
* 180 Ringing
* 183 Session Progress
* 200 OK
* error responses
* SDP generation
* expected SIP headers
* SIP retransmissions
* SIP INFO DTMF
* SIP timing/scenarios
* sending PCMA/PCMU RTP with `rtp_stream`
* RFC4733 RTP playback for scenarios that send it

### Go RTP verifier is responsible for

* receiving RTP sent by SUT
* parsing RTP
* PCMA decoding
* PCMU decoding
* RFC4733 DTMF decoding
* RTP diagnostics
* converting received audio to PCM
* saving received audio as WAV
* comparing received audio against expected MP3/WAV
* computing audio similarity and coverage

### Go test harness is responsible for

* allocating ports
* rendering SIPp XML scenarios
* launching SIPp
* terminating SIPp
* managing contexts/timeouts
* creating temporary directories
* starting/stopping RTP verifier
* preparing audio files for SIPp
* collecting SIPp logs
* exposing a high-level Go testing API
* ensuring isolation between parallel tests

---

## 3. Runtime Constraints

Everything must run locally.

Default network interface:

```text
127.0.0.1
```

No Internet connection is required during test execution.

SIP transport:

```text
UDP only
```

Not required:

```text
TCP
TLS
SRTP
SIP authentication
OPUS
NAT traversal
STUN
TURN
ICE
packet-loss simulation
jitter simulation
load testing
multiple simultaneous calls inside one Sutel instance
```

SIP trunk authentication model:

```text
trusted IP
```

There must be no REGISTER/authentication flow.

---

## 4. Parallel Test Isolation

This is a hard requirement.

Approximately 10 SUT instances may run simultaneously on different local ports.

Each test creates its own Sutel instance.

Example:

```text
Test A
    SUT :15001
    SIPp        :dynamic-A
    RTP verifier:dynamic-A

Test B
    SUT :15002
    SIPp        :dynamic-B
    RTP verifier:dynamic-B

...

Test J
    SUT :15010
    SIPp        :dynamic-J
    RTP verifier:dynamic-J
```

There must be:

* no global mutable call state
* no fixed SIP port
* no fixed RTP port
* no shared recording file
* no shared scenario output file
* no shared log file
* no shared temporary directory

Every Sutel instance owns its own:

```text
SIPp process
scenario XML
working directory
SIP port
RTP socket
audio buffers
logs
recordings
DTMF events
result state
```

The implementation must work correctly with:

```go
t.Parallel()
```

---

## 5. Project Structure

Recommended layout:

```text
sutel/
    carrier.go
    config.go
    result.go
    errors.go

    sipp/
        runner.go
        ports.go
        process.go
        logs.go
        render.go

    scenarios/
        outbound_answer.xml.tmpl
        outbound_busy.xml.tmpl
        outbound_reject.xml.tmpl
        outbound_no_answer.xml.tmpl
        outbound_183.xml.tmpl

        inbound_answer.xml.tmpl
        inbound_audio.xml.tmpl
        inbound_cancel.xml.tmpl

    rtp/
        receiver.go
        packet.go
        session.go
        recorder.go
        dtmf.go

    audio/
        decode.go
        mp3.go
        wav.go
        resample.go
        g711.go
        match.go
        normalize.go

    testkit/
        carrier.go
        assert.go

    testdata/
        ...
```

Do not put all logic into a single file.

---

## 6. SIPp Dependency

The harness must locate SIPp using:

1. explicit configuration
2. environment variable
3. PATH

Example priority:

```text
Config.SIPpBinary
SIPP_BIN
exec.LookPath("sipp")
```

Expose binary discovery separately from validation:

```go
type SIPpCapabilities struct {
    Version      string
    RTPStreaming bool
    PCAPPlayback bool
}

func ResolveSIPpBinary(config Config) (string, error)
func CheckSIPp(config Config) (SIPpCapabilities, error)
```

`CheckSIPp` should execute:

```text
sipp -v
```

and provide an actionable error when SIPp is unavailable, too old, or missing
baseline RTP streaming support. It returns all detected capabilities; each
scenario constructor then rejects any missing scenario-specific capability. If
`sipp -v` is insufficient to prove a capability, use a small bounded smoke
check.

Do not silently skip tests inside the core library.

An optional test helper may provide:

```go
testkit.RequireSIPp(t)
testkit.RequireSIPpCapabilities(t, sutel.RequirePCAPPlayback)
```

These helpers may call `t.Skip` with the detected binary/version/capability in
the reason. They are for deliberately capability-gated developer test runs;
the full V1 CI suite must use hard requirements and may not skip.

CI must use the exact pinned/tested SIPp version and build options declared by
the repository. Full V1 CI must build SIPp `3.7.7` with PCAP playback enabled
(for example, the upstream CMake option `-DUSE_PCAP=1`) and verify the detected
capability before running scenarios.

Do not launch SIPp using `-bg`.

The Go process must remain the parent and own the SIPp child process.

Use:

```go
exec.CommandContext(...)
```

or equivalent explicit lifecycle handling.

---

## 7. Sutel Core API

The core package should NOT depend directly on `testing.T`.

Suggested API:

```go
type Config struct {
    SIPpBinary string

    // Optional parent directories. Every Carrier still creates its own
    // unique child directory beneath these paths.
    WorkDir     string
    ArtifactDir string

    ArtifactPolicy ArtifactPolicy

    LocalIP string

    DefaultTimeout       time.Duration
    ReadinessTimeout     time.Duration
    RTPDrainTimeout      time.Duration
    RTPReorderWindow     int
    MaxSynthesizedRTPGap time.Duration
}

type ArtifactPolicy int

const (
    ArtifactsOnFailure ArtifactPolicy = iota
    ArtifactsAlways
    ArtifactsNever
)

func New(config Config) (*Carrier, error)
```

Default:

```go
LocalIP = "127.0.0.1"
ArtifactPolicy = ArtifactsOnFailure
DefaultTimeout = 20 * time.Second
ReadinessTimeout = 2 * time.Second
RTPDrainTimeout = 200 * time.Millisecond
RTPReorderWindow = 10
MaxSynthesizedRTPGap = 2 * time.Second
```

Zero durations mean “use the default”, not “wait forever”. Invalid addresses,
negative durations, an unusable directory, or contradictory configuration must
fail in `New` with a field-specific error.

`WorkDir` and `ArtifactDir` are parent directories, never shared instance
directories. `New` must create a unique child directory even when either parent
is explicitly configured.

Carrier:

```go
type Carrier struct {
    // private state
}

func (c *Carrier) Close() error
```

`Close()` must be idempotent.

### Error contract

Public errors must support `errors.Is`/`errors.As` for at least:

```text
ErrSIPpUnavailable
ErrSIPpCapability
ErrCarrierBusy
ErrNegotiation
ErrVerification
```

Context expiry must wrap `context.DeadlineExceeded` or `context.Canceled`.
Process, negotiation, and verification errors must carry the scenario name and
artifact location without requiring callers to parse an error string.

An expected negative carrier behavior is not a Go error. For example, when a
`Busy{}` scenario observes and sends the expected `486`, `Wait` returns a
successful `CallResult`; a missing/mismatched `486` returns a verification
error.

---

## 8. Outbound Call API

"Outbound" means:

```text
SUT ---> Sutel
```

SIPp acts as UAS.

Recommended API:

```go
type OutboundScenario struct {
    From NumberMatcher
    To   NumberMatcher

    Behavior OutboundBehavior

    Codecs []Codec

    DTMF *DTMFExpectation

    Audio *AudioExpectation

    Timeout time.Duration
}

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
```

Zero-valued audio-match options select the documented defaults. Explicit
thresholds must be finite and within their documented ranges: similarity and
coverage in `[0, 1]`, frame threshold in `(0, 1]`, silence threshold in
`[-96, 0)` dBFS, and durations greater than zero.

The effective session deadline is the earliest of the caller context deadline
and the non-zero scenario timeout. If neither supplies a deadline, use
`Config.DefaultTimeout`; no session may be unbounded.

Example:

```go
session, err := carrier.ExpectOutboundCall(
    ctx,
    sutel.OutboundScenario{
        From: sutel.ExactNumber("19001234"),
        To:   sutel.ExactNumber("0912345678"),

        Behavior: sutel.Answer{
            TryingAfter:  0,
            RingingAfter: 100 * time.Millisecond,
            AnswerAfter:  300 * time.Millisecond,
        },

        Codecs: []sutel.Codec{
            sutel.PCMA,
            sutel.PCMU,
        },

        Audio: &sutel.AudioExpectation{
            File:               "testdata/greeting.mp3",
            MinSimilarity:      0.95,
            MinCoverage:        0.80,
        },
    },
)
```

Then:

```go
carrierAddr := session.SIPAddr()
```

Example:

```text
127.0.0.1:43821
```

Configure SUT trunk to this address.

Then trigger the real SUT action.

Finally:

```go
result, err := session.Wait(ctx)
```

---

## 9. Outbound Test Example

The target workflow is: create a carrier, declare the expectation, wait for
readiness, configure SUT with `session.SIPAddr()`, trigger the real call, and
finish with `session.Wait(ctx)`. The complete authoritative example is the
Reference Test: Greeting Verification; do not maintain a second divergent copy
here.

The resulting WAV must be directly playable by a normal audio player.

---

## 10. Outbound SIP Happy Path

Expected flow:

```text
SUT                         SIPp
     |                               |
     |---------- INVITE ------------>|
     |<--------- 100 Trying ----------|
     |<--------- 180 Ringing ---------|
     |<--------- 200 OK + SDP --------|
     |---------- ACK ---------------->|
     |                               |
     |========== RTP =============================> Go RTP verifier
     |                               |
     |---------- BYE ---------------->|
     |<--------- 200 OK --------------|
```

Sutel's 200 SDP must advertise the RTP verifier, NOT an RTP socket owned by SIPp.

Example generated SDP:

```text
v=0
o=sutel 1 1 IN IP4 127.0.0.1
s=Sutel
c=IN IP4 127.0.0.1
t=0 0
m=audio 43172 RTP/AVP 8 0 101
a=rtpmap:8 PCMA/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=sendrecv
```

Here:

```text
43172
```

belongs to the Go RTP verifier.

SIPp must NOT attempt to bind the same RTP port.

The SDP above is illustrative only when the peer offered all three formats.
The concrete answer must contain only the negotiated intersection defined in
Codec Negotiation.

---

## 11. RTP Verifier

Start the verifier before SIPp sends any SDP that advertises its address. This
includes an outbound 183/200 response and an inbound INVITE offer.

Bind:

```go
net.ListenUDP(
    "udp",
    &net.UDPAddr{
        IP:   net.ParseIP("127.0.0.1"),
        Port: 0,
    },
)
```

The OS selects the RTP port.

Expose:

```go
func (r *Receiver) Addr() netip.AddrPort
```

Then inject that port into the generated SIPp scenario SDP.

### Media endpoint ownership

Signaling and media ownership are independent. For every call, the scenario
must follow this table:

| Call leg | SDP advertised by Sutel | SUT → Sutel RTP | Sutel → SUT RTP |
| --- | --- | --- | --- |
| Outbound, including 183 early media | Go RTP verifier address in SIPp's 183/200 answer | Go RTP verifier receives it | SIPp streams to the media address from SUT's INVITE offer |
| Inbound | Go RTP verifier address in SIPp's INVITE offer | Go RTP verifier receives it | SIPp streams to the media address from SUT's 200 answer |

The SIPp signaling socket, SIPp media sender, and Go RTP verifier may therefore
use different local UDP ports. SIPp must never bind the verifier's port.

The scenario must extract and validate the remote connection address, audio
port, payload mapping, and direction attributes from the peer SDP before media
playback. Missing or malformed peer SDP must fail with an actionable error.

V1 supports one audio media section. A rejected audio section (`m=audio 0`), an
unsupported direction such as `a=inactive`, or no common codec must fail media
setup rather than sending RTP to a guessed address.

---

## 12. RTP Parsing

Prefer using a mature RTP packet parser such as:

```text
github.com/pion/rtp
```

Do not implement the RTP header parser manually unless there is a strong reason.

Required fields to track:

```text
Version
Marker
PayloadType
SequenceNumber
Timestamp
SSRC
Payload
ArrivalTime
```

Ignore unrelated UDP traffic.

Only RTP version 2 packets whose payload type is present in the negotiated SDP
are eligible. RTCP, malformed datagrams, unknown payload types, and packets from
an unexpected source must be counted in diagnostics and ignored.

V1 validates the source IP against the peer signaling/SDP address but does not
require the UDP source port to equal the peer's advertised receive port. Lock
the source endpoint after the first valid audio packet for the selected SSRC.
This permits a legitimate asymmetric source port without allowing two senders
to be mixed. A future explicit symmetric-RTP profile may use different rules.

By default, identify the audio SSRC from the first valid negotiated audio
packet. Once selected, packets belonging to other SSRCs must not be mixed into
the recording. Record their existence in diagnostics. Automatic SSRC switching
is not part of V1.

Sequence numbers and timestamps must be compared with wraparound-safe RTP
arithmetic. Maintain a bounded reorder window so a late packet can be placed at
its RTP timestamp; do not permanently classify a packet as missing until it has
fallen outside that window. The default is `Config.RTPReorderWindow` (10
packets), and tests may configure it for deterministic cases.

---

## 13. RTP Payload Types

Minimum supported static audio types:

```text
0 = PCMU
8 = PCMA
```

Telephone event payload type must be configurable.

Default:

```text
101 = telephone-event/8000
```

Do not assume every future scenario will use 101.

Static payload types `0` and `8` retain their standard meanings. Dynamic
payload types, including telephone-event, must come from the negotiated SDP.
For an SDP answer, reuse the payload type offered by the peer; the configured
default is used only when Sutel originates the SDP offer.

---

## 14. G.711

Implement:

```go
func DecodeALaw(byte) int16
func EncodeALaw(int16) byte

func DecodeMuLaw(byte) int16
func EncodeMuLaw(int16) byte
```

Add exhaustive unit tests.

A full byte-domain codec test should cover all 256 possible encoded values.

Received G.711 audio must be converted to:

```text
PCM signed 16-bit
8000 Hz
mono
```

internally.

---

## 15. RTP Recording

The recording system should use RTP timestamps to reconstruct timing.

Do not simply append UDP payloads blindly if RTP timestamps indicate a gap.

Output format:

```text
WAV
PCM signed 16-bit
8000 Hz
mono
```

Public API:

```go
func (r *CallResult) SaveReceivedAudio(path string) error
```

Example:

```go
result.SaveReceivedAudio(
    "/tmp/TestGreeting/received.wav",
)
```

This recording represents:

```text
SUT --> Sutel
```

and must be suitable for human listening.

Do not include RFC4733 telephone-event payloads in the WAV.

Normative reconstruction rules:

1. Identify packets by SSRC and wraparound-extended sequence number. An exact
   repeat is a duplicate; conflicting contents for the same identity are a
   diagnostic and the first accepted packet wins.
2. Order audio by RTP timestamp after the bounded reorder window.
3. Decode each payload to the number of samples it actually contains.
4. Fill a positive timestamp gap with PCM silence.
5. Discard an exact overlap/duplicate; report a conflicting overlap as a
   diagnostic and keep the first accepted audio.
6. Cap any single synthesized gap at `Config.MaxSynthesizedRTPGap` (default 2
   seconds). A larger jump starts a discontinuity and must not allocate an
   unbounded buffer.

Sequence-number wrap and timestamp wrap are normal and must not create a false
gap. The WAV duration and `ReceivedDuration` are based on reconstructed RTP
sample time, not wall-clock packet arrival time.

---

## 16. RTP Diagnostics

Collect diagnostics even though packet-loss testing is not a goal.

Example:

```go
type RTPStats struct {
    SSRC uint32

    Codec Codec

    PacketsReceived int

    FirstSequence uint16
    LastSequence  uint16

    FirstTimestamp uint32
    LastTimestamp  uint32

    DuplicatePackets int
    MissingPackets   int
    OutOfOrderPackets int
    IgnoredPackets    int
    Discontinuities   int

    ReceivedDuration time.Duration
}
```

Missing packets should be diagnostic information.

Do NOT automatically fail because one packet appears missing unless a specific assertion requires it.

Define `MissingPackets` only after the reorder window has closed. Diagnostics
must distinguish duplicates, late/out-of-order packets, malformed packets,
unknown payload types, foreign SSRCs, and timestamp discontinuities.

---

## 17. Audio Input Support

The framework should accept at least:

```text
.mp3
.wav
```

Example:

```go
AudioExpectation{
    File: "greeting.mp3",
}
```

The audio normalization pipeline must produce:

```text
PCM16
8000 Hz
mono
```

Use pure-Go decoding where reasonably possible.

Avoid requiring an external `ffmpeg` process for normal test execution.

MP3 decoding may use a mature pure-Go library.

WAV support must accept standard uncompressed PCM WAV.

---

## 18. Audio Matching

This is an important feature.

Do NOT compare:

```text
RTP bytes
G.711 bytes
raw PCM sample-by-sample without alignment
```

The received audio may contain:

```text
initial silence
RTP startup delay
small timing differences
different gain
MP3 decoding differences
G.711 quantization
```

The matching pipeline should be:

```text
expected MP3/WAV
      |
      v
decode
      |
      v
mono PCM
      |
      v
resample to 8 kHz
      |
      v
normalize
      |
      +----------------------+
                             |
received RTP                 |
      |                      |
      v                      |
PCMA/PCMU decode             |
      |                      |
      v                      |
PCM 8 kHz                    |
      |                      |
      v                      |
normalize                    |
      |                      |
      +------ align ---------+
                 |
                 v
        windowed comparison
                 |
          +------+------+
          |             |
          v             v
     similarity      coverage
```

For V1 this pipeline is normative. Both inputs must use the same deterministic
resampler and must be converted to mono PCM16/8000 Hz before alignment.
Reconstructed silence caused by RTP timestamp gaps must be preserved.

In this document, `normalize` means convert PCM16 to deterministic floating
point samples in `[-1, 1]` and remove the stream's DC mean. It does not mean
peak normalization, compression, or clipping. Gain invariance comes from
normalized correlation and per-window RMS normalization, so the silence
threshold remains meaningful against the original expected signal level.

V1 does not perform time stretching, dynamic time warping, speech recognition,
or semantic matching. A duration mismatch is reflected in coverage. Extra
received audio outside the aligned expected interval is diagnostic information
and does not increase similarity or coverage.

---

## 19. Audio Alignment

Before comparing audio content, find the alignment offset.

Example:

```text
Expected:
|----- greeting ----------------------|

Received:
       |----- greeting ----------------------|
       ^
       + 140 ms startup delay
```

This must still pass.

Recommended implementation:

1. Convert both streams to PCM16/8kHz/mono.
2. Generate a short-time RMS envelope using 20 ms windows and a 10 ms hop.
3. Use normalized cross-correlation on the envelope.
4. Find the best offset.
5. Limit alignment search to a configurable window.

Default:

```go
MaxAlignmentOffset = 2 * time.Second
```

Return:

```go
AlignmentOffset time.Duration
```

for diagnostics.

The search range is `[-MaxAlignmentOffset, +MaxAlignmentOffset]`. A positive
offset means the matching content begins later in the received stream than in
the expected stream. If offsets tie, select the smallest absolute offset, then
the earlier offset. If either envelope has no active audio or correlation is
undefined, alignment fails with a typed “no active audio” result rather than
returning an arbitrary offset.

---

## 20. Similarity and Coverage

Do not collapse these into one metric.

Return both:

```go
type AudioMatchResult struct {
    Similarity float64
    Coverage   float64

    AlignmentOffset time.Duration

    ExpectedDuration time.Duration
    ReceivedDuration time.Duration

    ComparedDuration time.Duration
}
```

Definitions:

### Similarity

How similar the overlapping received audio is to the expected audio.

Range:

```text
0.0 ... 1.0
```

### Coverage

How much of the expected active audio content was successfully matched.

Range:

```text
0.0 ... 1.0
```

Example:

```text
Expected greeting: 5.0 sec
Correct received content: ~4.2 sec

Similarity: 0.98
Coverage:   0.84
```

This should pass:

```go
MinSimilarity = 0.95
MinCoverage   = 0.80
```

Normative edge cases:

* If expected audio has no active windows, the expectation is invalid.
* If received audio has no comparable active content, similarity and coverage
  are both `0`.
* An expected active window extending beyond the received recording is
  unmatched and lowers coverage.
* Missing windows are excluded from the similarity mean because similarity
  describes comparable overlap; they still lower coverage.
* All public metric values must be finite and clamped to `[0, 1]`.

---

## 21. V1 Windowed Comparison Algorithm

Use a deterministic, simple DSP algorithm.

Do NOT add speech-to-text, machine learning, cloud APIs, or perceptual AI models.

After alignment:

1. Split expected audio into 100 ms windows.
2. Remove each window's DC component and determine its RMS.
3. Mark an expected window active when its RMS is above the configured silence
   threshold.
4. Map each active expected window into received audio using the alignment
   offset. A window with insufficient received samples is not comparable.
5. Independently RMS-normalize the two comparable windows.
6. Compute normalized correlation and clamp negative or undefined results to
   `0`, producing a score in `[0, 1]`.
7. Treat a comparable window as matched if its score is greater than or equal
   to the configured frame threshold.
8. Compute overall metrics using the exact formulas below.

Required initial defaults:

```go
FrameDuration       = 100 * time.Millisecond
FrameMatchThreshold = 0.80
SilenceThresholdDBFS = -45.0
```

`SilenceThresholdDBFS` is measured against full-scale PCM16 RMS. A final
partial window must contain at least 50% of `FrameDuration`; shorter tails are
ignored consistently.

Then:

```text
Coverage =
matched active expected windows
--------------------------------
total active expected windows
```

and:

```text
Similarity =
mean correlation across compared active windows
```

In formula form:

```text
active      = expected windows above SilenceThresholdDBFS
comparable  = active windows fully mapped to received samples
matched     = comparable windows with score >= FrameMatchThreshold

Coverage    = len(matched) / len(active)
Similarity  = mean(score for comparable)
```

If `len(comparable) == 0`, similarity is `0`. These definitions intentionally
allow a correct truncated prefix to have high similarity and low coverage.

These constants must not be hard-coded deeply.

They should be configurable and unit-tested.

---

## 22. Audio Match Failure Diagnostics

When matching fails, produce an actionable message.

Example:

```text
audio expectation failed

expected:
  testdata/greeting.mp3

required:
  similarity >= 95.0%
  coverage   >= 80.0%

actual:
  similarity = 96.8%
  coverage   = 63.4%

codec:
  PCMA

expected duration:
  4.82s

received duration:
  3.14s

alignment:
  +124ms

RTP packets:
  157
```

When the artifact policy preserves the run, save:

```text
expected-normalized.wav
received.wav
audio-match.json
rtp-events.jsonl
```

---

## 23. Artifact Handling

Never write to global names such as:

```text
/tmp/call.wav
/tmp/sipp.log
```

Every carrier must have a unique working directory.

For example:

```text
/tmp/sutel-314729/
```

Files may include:

```text
scenario.xml
sipp.stdout.log
sipp.stderr.log
sipp_messages.log
sipp_errors.log
sipp_calldebug.log

received.wav
expected-normalized.wav

rtp-events.jsonl
result.json
```

Allow the caller to configure a persistent artifact parent directory:

```go
Config{
    ArtifactDir: "...",
}
```

If no persistent parent is given, use an instance-specific temporary directory.

Artifact retention is deterministic:

* `ArtifactsOnFailure` is the default. Preserve artifacts when SIPp fails, an
  expectation fails, setup fails after process launch, or the context expires.
* `ArtifactsAlways` preserves every run.
* `ArtifactsNever` removes the instance directory after all result data and
  relevant logs have been loaded into memory.

`Close` must not delete a preserved directory. A result must report whether its
artifact directory was preserved and return its final absolute path. Never put
temporary instance files directly into the configured parent.

---

## 24. SIPp Scenario Rendering

Store scenario templates in the repository and embed them into the Go package
using `go:embed` so installed library users do not depend on the repository's
current working directory.

Use Go `text/template`.

Example:

```text
outbound_answer.xml.tmpl
```

The harness renders a temporary concrete scenario:

```text
/tmp/.../scenario.xml
```

Dynamic template values include:

```text
expected From
expected To

RTP verifier IP
RTP verifier port

codec list
telephone-event payload type

ring delay
answer delay

expected SIP INFO DTMF
```

Avoid generating XML by concatenating arbitrary strings everywhere.

Keep protocol behavior visible in XML templates.

Validate all template inputs before rendering. XML text, SIP header values,
filenames, and regular expressions require context-appropriate escaping; do
not insert arbitrary caller strings into XML, SIP messages, or SIPp actions.
Parse the rendered file as XML before process launch and report the template
name and line/column on failure.

---

## 25. Outbound Answer Scenario

At minimum implement:

```text
INVITE
100
180
200
ACK
RTP
BYE
200
```

Example behavior configuration:

```go
type Answer struct {
    OmitTrying bool

    TryingAfter  time.Duration
    RingingAfter time.Duration
    AnswerAfter  time.Duration

    HangupAfter time.Duration
}
```

`HangupAfter == 0` means wait for the remote side to terminate unless scenario explicitly defines otherwise.

All `*After` values are deadlines relative to receipt of the first valid INVITE,
not cumulative sleeps. They must be non-negative and nondecreasing for messages
that are enabled. The zero value sends `100 Trying` immediately; set
`OmitTrying` only for a scenario that intentionally omits it.

---

## 26. Carrier Error Scenarios

Mandatory outbound behaviors:

```go
sutel.Answer{}
sutel.Busy{}
sutel.Reject{}
sutel.NotFound{}
sutel.ServiceUnavailable{}
sutel.NoAnswer{}
sutel.Timeout{}
```

Expected SIP behavior:

```text
Busy:
    486 Busy Here

Reject:
    603 Decline

NotFound:
    404 Not Found

ServiceUnavailable:
    503 Service Unavailable
```

### NoAnswer

Example:

```text
INVITE
100
180
...
CANCEL
200 OK to CANCEL
487 Request Terminated to INVITE
ACK
```

The test should verify that SUT correctly cancels the unanswered call.

The scenario must accept normal retransmissions of the same INVITE while
waiting. After CANCEL it must send `200 OK` to CANCEL, send `487` for the
original INVITE, receive ACK for that `487`, and only then complete.

### Timeout

Sutel intentionally does not send an appropriate SIP response within the test window.

After receiving the first INVITE, it remains silent and tolerates retransmitted
copies of that same transaction. `Timeout` is distinct from `NoAnswer`: it does
not send `100` or `180` unless a separate timeout variant explicitly requests
one.

The scenario must remain bounded by the integration test context.

No test is allowed to hang forever.

---

## 27. 183 / Early Media

Implement an explicit scenario supporting:

```text
INVITE
100
183 Session Progress + SDP
RTP early media
180 optional
200 OK
ACK
```

Expose this as an `OutboundBehavior` with explicit final-answer timing:

```go
sutel.EarlyMedia{
    File:          "...",
    ProgressAfter: 100 * time.Millisecond,
    SendRinging:   false,
    RingingAfter:  0,
    AnswerAfter:   2 * time.Second,
}
```

`ProgressAfter`, optional `RingingAfter`, and `AnswerAfter` are relative to the
INVITE and follow the same validation rules as `Answer`. The 183 and 200 SDP
must be compatible; if the media address or payload mapping changes, the Go RTP
verifier must already own the newly advertised port before the response is sent.

This belongs to Milestone 5 rather than Milestone 1, but it is mandatory for a
complete V1.

---

## 28. Codec Negotiation

Supported codecs:

```go
type Codec int

const (
    PCMA Codec = iota
    PCMU
)
```

No OPUS.

Scenario must allow:

```go
Codecs: []Codec{PCMA}
```

or:

```go
Codecs: []Codec{PCMU}
```

or:

```go
Codecs: []Codec{PCMA, PCMU}
```

The RTP verifier must detect the actual codec from RTP payload type and SDP configuration.

Negotiation rules are deterministic:

### Outbound / SIPp as answerer

1. Parse the audio formats and `rtpmap` attributes in SUT's offer.
2. Intersect them with `OutboundScenario.Codecs`.
3. Emit only the intersection in the answer, ordered by the scenario's codec
   preference.
4. Reuse static payload types `8` for PCMA and `0` for PCMU.
5. If there is no common audio codec, send `488 Not Acceptable Here`, do not
   start media, and return a typed negotiation result.

An empty outbound codec list defaults to `[]Codec{PCMA, PCMU}`. When multiple
formats are answered, the first valid audio RTP packet selects the actual codec
for that call. A mid-call switch to another audio payload type without a new
offer/answer is outside V1 and must fail explicitly instead of corrupting the
recording.

### Inbound / SIPp as offerer

The INVITE offers `InboundScenario.Codec` plus telephone-event when required.
The 200 answer must contain that codec. Otherwise SIPp must not send media and
the session returns a typed negotiation error.

For telephone-event, an answer reuses the dynamic payload type in the peer
offer. An offer originated by Sutel uses the configured payload type, default
`101`. The RTP verifier uses the negotiated mapping, never an unconditional
hard-coded `101`.

The selected audio codec and telephone-event payload type must be present in
`CallResult` and the diagnostic output.

Tests must include:

```text
PCMA-only carrier
PCMU-only carrier
both codecs
```

---

## 29. Inbound Calls

"Inbound" means:

```text
Sutel ---> SUT
```

SIPp acts as UAC.

Suggested API:

```go
type InboundScenario struct {
    TargetSIPAddr netip.AddrPort

    From string
    To   string

    Codec Codec

    Audio *AudioPlayback

    DTMF []DTMFAction

    RingTimeout time.Duration
    CallDuration time.Duration
}
```

Usage:

```go
session, err := carrier.Call(
    ctx,
    sutel.InboundScenario{
        TargetSIPAddr: system.SIPAddr(),

        From: "0912345678",
        To:   "19001234",

        Codec: sutel.PCMA,
    },
)
```

SIPp sends a real INVITE to SUT.

`RingTimeout == 0` uses a 5-second default capped by the effective session
deadline. `CallDuration == 0` waits for SUT to terminate; a positive value
causes Sutel to send BYE that long after ACK. Audio and DTMF actions begin
only after ACK unless a scenario explicitly models early media.

A non-2xx final response, malformed SDP, codec rejection, or ring timeout is a
completed typed call result, not an unexplained SIPp process error.

---

## 30. Inbound Happy Path

Expected flow:

```text
SIPp                              SUT
 |                                     |
 |------------ INVITE ---------------->|
 |<----------- 100 --------------------|
 |<----------- 180 --------------------|
 |<----------- 200 + SDP --------------|
 |------------ ACK ------------------->|
 |                                     |
 |============ RTP ===================>|
 |                                     |
 |------------ BYE ------------------->|
 |<----------- 200 --------------------|
```

This diagram shows the Sutel-hangup variant. The SUT-hangup variant
receives BYE and returns `200 OK` instead.

The inbound scenario must correctly use the SDP returned by SUT for RTP playback.

---

## 31. Playing Audio from Sutel

The public API should accept MP3 or WAV:

```go
AudioPlayback{
    File: "testdata/customer-speech.mp3",
}
```

Before launching SIPp:

```text
MP3/WAV
   |
   v
decode
   |
   v
PCM16
   |
   v
mono
   |
   v
8kHz
   |
   v
G.711 encode
   |
   +--> raw PCMA
   |
   +--> raw PCMU
```

Generate the appropriate temporary raw audio file.

For PCMA use SIPp RTP streaming with payload type 8.

For PCMU use payload type 0.

Do not require developers to manually prepare `.alaw` or `.ulaw` fixtures.

---

## 32. DTMF — Receiving RFC4733

When SUT sends DTMF using RTP telephone-event:

```text
SUT
     |
     | RTP PT 101
     v
Go RTP verifier
```

The verifier must parse RFC4733 telephone-event payloads.

Expose:

```go
type DTMFEvent struct {
    Digit string

    Duration time.Duration

    Volume uint8

    Timestamp uint32
}
```

Handle event IDs:

```text
0-9
*
#
A-D
```

Repeated RTP packets belonging to the same telephone event must NOT produce duplicate logical DTMF events.

The RFC4733 end flag must be handled correctly.

Use `(SSRC, RTP timestamp, event ID)` as the logical event identity. Merge
retransmitted start, continuation, and end packets; use the greatest valid
duration field; and emit exactly one `DTMFEvent` after the first end packet.
Repeated end packets update diagnostics but do not emit another event. Convert
duration using the negotiated telephone-event clock rate.

An event without an end packet at media shutdown is incomplete. Keep it in
diagnostics and fail a matching DTMF expectation, but do not report it as a
completed digit.

---

## 33. DTMF — Receiving SIP INFO

SIPp handles SIP INFO.

Support:

```text
Content-Type: application/dtmf-relay
```

Typical body:

```text
Signal=5
Duration=160
```

Scenario expectations should be generated from:

```go
DTMFExpectation{
    Method: SIPInfo,
    Digits: "123#",
}
```

The SIPp scenario should fail if the expected INFO sequence is incorrect.

Each valid INFO must receive `200 OK`; malformed or unexpected INFO must receive
an explicit error response and fail the scenario. Parse `Signal` and `Duration`
case-insensitively with optional surrounding whitespace, but reject duplicate
or invalid fields.

---

## 34. DTMF — Sending SIP INFO

For inbound calls, SIPp must be able to send:

```text
INFO
Content-Type: application/dtmf-relay
```

Example API:

```go
DTMFAction{
    Method:   sutel.SIPInfo,
    Digits:   "123#",
    Interval: 200 * time.Millisecond,
}
```

---

## 35. DTMF — Sending RFC4733

This must also be supported.

Preferred initial implementation:

* use SIPp RTP/PCAP facilities
* keep reusable RFC4733 fixtures under testdata
* one fixture per required event or a deterministic generator
* replay toward the negotiated RTP destination

This feature requires SIPp with PCAP support and is mandatory for the full V1
acceptance suite.

If RFC4733 transmission cannot be supported by the installed SIPp build, fail with an explicit capability error.

Do not silently downgrade RFC4733 to SIP INFO.

Fixtures or the deterministic generator must use the telephone-event payload
type negotiated for the call; they must not assume `101`. Generated events must
contain a start packet, monotonically increasing duration, the end bit, and the
usual repeated end packets. Playback must complete, or be explicitly cancelled,
before SIPp sends BYE and exits.

---

## 36. SIP Header Verification

Outbound scenarios should support verifying:

```text
Request-URI
From
To
Contact
Call-ID presence
CSeq
Content-Type
SDP presence
```

Use SIPp regexp/assertion mechanisms rather than parsing SIP independently in Go wherever possible.

Number matching API:

```go
type NumberMatcher interface {
    // internal
}

func ExactNumber(v string) NumberMatcher
func NumberRegexp(v string) NumberMatcher
func AnyNumber() NumberMatcher
```

Do not assume Vietnamese formatting rules inside Sutel.

For example:

```text
0912345678
84912345678
+84912345678
```

must remain test decisions rather than framework normalization.

---

## 37. SIPp Process Arguments

For one call per carrier, prefer UDP mono-socket mode.

Conceptually:

```text
sipp
    -sf scenario.xml
    -i 127.0.0.1
    -p <unique SIP port>
    -cp <unique control port>
    -t u1
    -m 1
    -nostdin
    -trace_err
    -trace_msg
    -trace_calldebug
```

Do not rely on SIPp's default remote-control port. Allocate a unique control
port per process, pass it explicitly, and include it in cleanup and bind-retry
handling. This prevents one parallel test from controlling another SIPp
instance.

Add a global timeout appropriate to the test.

Do not disable SIP retransmissions globally.

Let scenarios use normal UDP retransmission behavior where appropriate.

---

## 38. SIP Port Allocation

RTP is easy because Go owns the UDP socket and can bind to port zero.

SIPp cannot use the already-open Go socket directly.

Implement SIP port allocation carefully.

Recommended approach:

1. Reserve an available UDP port using Go.
2. Release it immediately before starting SIPp.
3. Start SIPp on that port.
4. Detect bind failure.
5. Retry with another port if necessary.

SIPp SIP-port or control-port bind failure must trigger an automatic retry with
a completely new candidate port set and a newly rendered command.

Use a bounded retry count, for example:

```text
5 attempts
```

Do not maintain a global incrementing port counter.

This design must tolerate parallel tests.

---

## 39. SIPp Readiness

For outbound/UAS tests, SUT must not place the call before SIPp has bound its SIP socket.

Implement a readiness check.

Do NOT depend on an arbitrary long sleep such as:

```go
time.Sleep(time.Second)
```

A short polling readiness mechanism is acceptable.

Readiness is the conjunction of:

* the exact child process is still running
* its startup output contains no bind or scenario-load failure
* an exclusive bind probe confirms that the selected SIP UDP port is owned
* the condition remains true until the address is returned

A UDP send succeeding is not a readiness signal because UDP has no handshake.
The bind probe is correlated with the just-started child and the previously
reserved port; if ownership is ambiguous, abort that attempt and retry. Poll at
a short interval until `Config.ReadinessTimeout`, while continuously checking
for child exit.

Once ready, return the carrier SIP address to the caller.

---

## 40. Process Lifecycle

Every SIPp process must be owned by one carrier/session.

On normal completion:

```text
scenario completes
SIPp exits
Go waits for process
exit status checked
logs collected
RTP reorder window flushed
RTP receiver drained
assertions finalized
```

On context cancellation or test cleanup:

1. request graceful termination if practical
2. wait briefly
3. terminate
4. force kill only if necessary
5. wait/reap process
6. close RTP sockets

Never leave zombie SIPp processes.

Never leave goroutines running after `Close()`.

The context passed when creating a session owns the full session lifetime.
Cancellation starts cleanup immediately. A context passed to `Wait` also
cancels that wait and the session; it is not merely a polling timeout.

`Session.Wait` must be concurrency-safe and idempotent: all callers receive the
same immutable final result/error. It returns only after SIPp has been reaped,
the RTP reorder buffer has been flushed, and the receiver has observed no new
valid RTP for `RTPDrainTimeout` (bounded by the session context).

`Carrier.Close` must be concurrency-safe and idempotent. It cancels any active
session, waits for cleanup, applies artifact retention, and then returns a
combined cleanup error. One Carrier supports one active call at a time in V1;
attempting to start a second active session returns a typed busy error.

---

## 41. SIPp Exit Handling

Treat SIPp process results as part of the test result.

Examples:

```go
type SIPpResult struct {
    ExitCode int

    Stdout string
    Stderr string

    MessagesLog string
    ErrorsLog   string
    CallDebugLog string
}
```

A SIPp scenario failure should become a Go error containing:

```text
scenario name
exit code
stderr
relevant SIP trace
artifact directory
```

Do not return only:

```text
exit status 1
```

---

## 42. Call Result

Suggested result object:

```go
type CallResult struct {
    Direction CallDirection

    SIP SIPpResult
    Outcome SIPOutcome

    RTP RTPStats
    Media NegotiatedMedia

    Audio *AudioMatchResult

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
func (r CallResult) ArtifactDir() (path string, preserved bool)
```

`CallDirection` has only `Inbound` and `Outbound`. It is distinct from event or
packet direction. `CallParty` has `NoParty`, `SUT`, and `Sutel`.
`InviteFinalStatus` is zero when no final INVITE response was observed, as in a
timeout scenario.

Do not expose internal mutable buffers directly. Slice accessors return copies,
and repeated `Wait` calls return independent deep copies of the same logical
result so one caller cannot mutate another caller's data.

---

## 43. High-Level Test Helper

Provide an optional package that integrates with `testing.TB`.

Example:

```go
carrier := testkit.NewCarrier(t)
```

It should:

```text
mark helper
create temp workdir
register cleanup
fail with readable diagnostics
```

Example:

```go
session := carrier.ExpectOutboundCall(
    sutel.OutboundScenario{
        To: sutel.ExactNumber("0912345678"),

        Behavior: sutel.Answer{},

        Audio: &sutel.AudioExpectation{
            File:          "greeting.mp3",
            MinSimilarity: 0.95,
            MinCoverage:   0.80,
        },
    },
)

system.SetTrunk(session.SIPAddr())

// trigger SUT call

result := session.RequireSuccess(t)

result.RequireAudio(t)
```

The core package must still remain usable without `testing.T`.

---

## 44. Required Scenarios — V1

V1 must include all of these scenarios:

### Outbound

```text
1. normal answer PCMA
2. normal answer PCMU
3. busy 486
4. reject 603
5. not found 404
6. service unavailable 503
7. no answer + CANCEL
8. remote carrier hangs up with BYE
9. SUT hangs up with BYE
10. 183 early media
11. no common codec returns 488
```

### Inbound

```text
1. normal call PCMA
2. normal call PCMU
3. Sutel sends audio
4. Sutel hangs up
5. SUT hangs up
6. SUT rejects the offered codec
```

### DTMF

```text
1. SUT -> carrier RFC4733
2. SUT -> carrier SIP INFO
3. carrier -> SUT RFC4733
4. carrier -> SUT SIP INFO
```

### Media

```text
1. RTP received
2. PCMA decode
3. PCMU decode
4. WAV recording
5. MP3 expected-audio comparison
6. WAV expected-audio comparison
```

---

## 45. Parallelism Acceptance Test

Create a test that launches at least 10 isolated Sutel instances concurrently.

Example:

```go
func TestTenCarriersInParallel(t *testing.T) {
    for i := 0; i < 10; i++ {
        i := i

        t.Run(
            fmt.Sprintf("carrier-%d", i),
            func(t *testing.T) {
                t.Parallel()

                // Create a separate Sutel instance.
                // Start separate SIPp.
                // Bind separate RTP verifier.
                // Complete a simple SIP call.
            },
        )
    }
}
```

Verify:

```text
no port collisions
no control-port collisions
no cross-call RTP
no cross-call SIP
no shared logs
no leaked processes
no data races
```

Run:

```text
go test -race ./...
```

---

## 46. Audio Unit Tests

Audio matching must have dedicated deterministic tests.

Required cases:

```text
exact same audio
    similarity near 1
    coverage near 1

100 ms leading silence
    must still match

500 ms leading silence
    must still match

gain -6 dB
    must still match

G.711 encode/decode roundtrip
    must still match

first 80% only
    coverage approximately 0.80

first 50% only
    coverage approximately 0.50

completely different speech/audio
    must fail similarity

silence instead of greeting
    must fail

random noise
    must fail

silence-only expected fixture
    must be rejected as invalid

extra received prefix and suffix
    must not increase coverage

no comparable received windows
    similarity and coverage must be finite zero values

silence threshold boundary
    active-window classification must be deterministic
```

Do not tune thresholds only against one greeting file.

Coverage assertions for truncated audio must allow at most one active frame of
rounding error. Add golden metric tests that exercise the exact formulas, not
only pass/fail thresholds.

---

## 47. RTP Unit Tests

Required:

```text
PCMA RTP packet decoding
PCMU RTP packet decoding

sequence increment
timestamp reconstruction

duplicate RTP packet
out-of-order packet
sequence number wrap
timestamp wrap
timestamp gap filled with silence
oversized timestamp discontinuity
reorder-window flush at shutdown

telephone-event decoding

telephone-event repeated packets
telephone-event end bit
telephone-event missing end bit
telephone-event dynamic payload type

different SSRC ignored
source endpoint locked after first valid packet
unknown payload type ignored
malformed RTP ignored
```

Packet-loss/jitter simulation is not a V1 product feature, but parser robustness is still expected.

---

## 48. SIPp Scenario Tests

Where practical, test SIPp scenarios against another SIPp instance or a minimal controlled endpoint.

At minimum ensure generated XML is valid before attempting a SUT integration test.

A malformed template must fail immediately with an actionable message.

Required scenario-level tests include codec intersection/order, `488` on no
common codec, dynamic telephone-event payload mapping, missing/malformed SDP,
CANCEL/487/ACK sequencing, both BYE directions, and PCAP capability failure.

---

## 49. Logging

Normal successful tests should not spam stdout.

On failure print:

```text
Sutel scenario
SIPp command
SIPp exit code

SIP trace summary

RTP stats

audio metrics

artifact directory
```

Example:

```text
Sutel verification failed

scenario:
    outbound-answer

SIP:
    INVITE received
    100 sent
    180 sent
    200 sent
    ACK received

RTP:
    codec: PCMA
    packets: 157
    duration: 3.14s

audio:
    similarity: 96.8%
    coverage:   63.4%
    required:   similarity >= 95%, coverage >= 80%

artifacts:
    /tmp/sutel-138271/
```

---

## 50. Event Timeline

Expose a unified event timeline for debugging.

Example:

```text
00.000 SIP <- INVITE
00.001 SIP -> 100 Trying
00.104 SIP -> 180 Ringing
00.308 SIP -> 200 OK
00.314 SIP <- ACK

00.327 RTP <- PT=8 seq=1193 ts=58240
00.347 RTP <- PT=8 seq=1194 ts=58400
00.367 RTP <- PT=8 seq=1195 ts=58560

03.442 SIP <- BYE
03.443 SIP -> 200 OK
```

This does not need a sophisticated distributed tracing system.

Simple structured events are sufficient.

Recommended representation:

```go
type Event struct {
    Time time.Time

    Layer EventLayer
    Direction EventDirection

    Type string
    Detail string
}
```

`EventDirection` describes traffic relative to Sutel (`Sent` or
`Received`) and must not reuse `CallDirection`.

---

## 51. Carrier Profiles — NOT V1

Design APIs so future carrier profiles are possible:

```go
carrier.Profile(sutel.Viettel)
carrier.Profile(sutel.VNPT)
carrier.Profile(sutel.FPT)
```

But DO NOT implement fake assumptions about Viettel/VNPT/FPT yet.

Profiles should only be added after real behavior has been observed.

Future examples:

```text
specific SDP ordering
specific header
183 behavior
session timers
telephone-event payload type
specific error code
Contact behavior
RTP behavior
```

Do not invent carrier quirks.

---

## 52. PCAP Regression — Future-Friendly

The architecture should allow a future scenario to replay captured RTP using SIPp PCAP playback.

Example future workflow:

```text
production interoperability issue
        |
        v
capture sanitized packet behavior
        |
        v
create regression fixture
        |
        v
SIPp replay
        |
        v
SUT regression test
```

Do not make PCAP capture/replay the primary media architecture.

---

## 53. Security / Privacy

All default sockets must bind to:

```text
127.0.0.1
```

Never bind to:

```text
0.0.0.0
```

unless explicitly configured.

Fixtures committed to the repository must not contain:

```text
real customer phone numbers
real production conversations
authentication credentials
production SIP headers containing secrets
```

---

## 54. Non-Goals

Do NOT implement the following in V1:

```text
full RFC3261 SIP stack in Go
SIP registrar
REGISTER
Digest authentication
TCP
TLS
SRTP
OPUS
WebRTC
ICE
STUN
TURN
NAT simulation
packet-loss simulation
network jitter
media transcoding server
PBX
call queues
conference calls
multiple simultaneous calls per carrier instance
real Viettel/VNPT/FPT connectivity
load testing
thousands of calls
GUI
HTTP API
database
Docker requirement
cloud service
speech recognition
AI audio comparison
```

If something is not necessary for integration testing SUT, do not build it.

---

## 55. Implementation Order

Implement in this order.

### Milestone 1 — SIPp harness

Deliver:

```text
SIPp discovery
process lifecycle
port allocation
working directories
scenario rendering
outbound UAS happy path
inbound UAC happy path
logs
parallel isolation
```

No audio matching yet.

Acceptance:

```text
SUT can make a real SIP call to SIPp.
SIPp can make a real SIP call to SUT.
10 independent SIPp instances can run concurrently.
```

### Milestone 2 — RTP receive + recording

Deliver:

```text
RTP UDP listener
Pion RTP parsing
PCMA decode
PCMU decode
WAV export
RTP diagnostics
```

Acceptance:

```text
SUT sends greeting audio.
Sutel creates a WAV.
A developer can listen to the WAV and hear the greeting.
```

### Milestone 3 — Audio verification

Deliver:

```text
MP3 decode
WAV decode
8k mono normalization
alignment
similarity
coverage
diagnostics
```

Acceptance:

```text
correct greeting passes
truncated greeting reports correct coverage
wrong audio fails
```

### Milestone 4 — DTMF

Deliver:

```text
RFC4733 receive
SIP INFO receive
SIP INFO send
RFC4733 send
```

### Milestone 5 — Failure scenarios

Deliver:

```text
486
603
404
503
no-answer/CANCEL
remote BYE
183
```

---

## 56. Definition of Done

V1 is considered complete when all of the following are true:

1. Tests run entirely on localhost.
2. SIP signaling is performed by real SIPp processes over UDP.
3. SUT connects through a real SIP trunk-like network boundary.
4. Both inbound and outbound calls work.
5. PCMA works.
6. PCMU works.
7. Sutel can receive SUT RTP.
8. Received audio can be exported as WAV.
9. A human can listen to the WAV.
10. `greeting.mp3` can be automatically verified.
11. Audio verification reports similarity separately from coverage.
12. RFC4733 DTMF can be verified.
13. SIP INFO DTMF can be verified.
14. Sutel can send both forms of DTMF.
15. Common SIP failure scenarios work.
16. Ten Sutel/SUT pairs can run in parallel without interference.
17. `go test -race ./...` passes.
18. Closing a carrier leaves no SIPp process or Go goroutine behind.
19. Failures produce useful SIP/RTP/audio diagnostics.
20. No custom SIP stack has been implemented.
21. The repository declares and CI uses its pinned SIPp version/build options.
22. Full V1 CI runs with PCAP support and does not skip carrier-to-SUT
    RFC4733 playback.

---

## 57. Reference Test: Greeting Verification

This is the canonical V1 integration test.

Goal:

> SUT calls a fake telecom carrier and sends `greeting.mp3`. The carrier must receive audio that matches the expected greeting, with at least 80% coverage.

Target test shape:

```go
func TestSystemSendsGreeting(t *testing.T) {
    t.Parallel()

    ctx, cancel := context.WithTimeout(
        context.Background(),
        20*time.Second,
    )
    defer cancel()

    carrier := testkit.NewCarrier(t)

    call := carrier.ExpectOutboundCall(
        sutel.OutboundScenario{
            To: sutel.ExactNumber(
                "0912345678",
            ),

            Behavior: sutel.Answer{
                RingingAfter: 100 * time.Millisecond,
                AnswerAfter:  300 * time.Millisecond,
            },

            Codecs: []sutel.Codec{
                sutel.PCMA,
                sutel.PCMU,
            },

            Audio: &sutel.AudioExpectation{
                File: "testdata/greeting.mp3",

                MinSimilarity: 0.95,
                MinCoverage:   0.80,
            },
        },
    )

    system := startVoiceSystem(t)

    system.SetTrunk(
        call.SIPAddr(),
    )

    callID, err := system.Call(
        ctx,
        "0912345678",
    )
    require.NoError(t, err)

    require.NoError(
        t,
        system.PlayAudio(
            ctx,
            callID,
            "testdata/greeting.mp3",
        ),
    )

    result := call.RequireSuccess(t)

    require.GreaterOrEqual(
        t,
        result.Audio.Similarity,
        0.95,
    )

    require.GreaterOrEqual(
        t,
        result.Audio.Coverage,
        0.80,
    )

    require.NoError(
        t,
        result.SaveReceivedAudio(
            filepath.Join(
                carrier.ArtifactDir(),
                "received.wav",
            ),
        ),
    )
}
```

Expected failure output if SUT sends only half of the greeting:

```text
audio expectation failed

expected:
    greeting.mp3

similarity:
    97.2%

coverage:
    51.3%

required:
    similarity >= 95.0%
    coverage >= 80.0%

codec:
    PCMA

alignment:
    +118ms

expected duration:
    4.82s

received duration:
    2.51s

artifact:
    .../received.wav
```

That behavior is the baseline standard for the entire framework.

---

## 58. Engineering Principle

This framework is not intended to become another PBX or SIP implementation.

The design principle is:

```text
SIPp owns SIP behavior.
Go owns testing ergonomics and media assertions.
SUT must see real SIP and RTP network traffic.
```

Whenever a new requirement appears, prefer:

```text
new SIPp scenario
```

over:

```text
new SIP implementation in Go
```

For media assertions, prefer:

```text
small deterministic Go DSP/tooling
```

over:

```text
complex external telecom infrastructure
```

The framework exists to make SUT integration tests deterministic, reproducible, local, fast, and easy to debug.
