# Sutel

Sutel là thư viện Go dùng để integration test hệ thống thoại SIP trên máy local.
Thư viện đóng vai một nhà mạng: nhận cuộc gọi từ hoặc gọi vào hệ thống cần kiểm
thử, gửi/nhận RTP audio, kiểm tra DTMF và mô phỏng các lỗi SIP phổ biến.

Trong tài liệu này, hệ thống cần kiểm thử được gọi tắt là **SUT** (*system under
test*). SUT có thể là SIP server, voice gateway, PBX hoặc bất kỳ ứng dụng thoại
nào giao tiếp qua SIP/RTP.

## Cài đặt

```bash
go get github.com/subiz/sutel
```

Sutel chạy hoàn toàn bằng Go. Test chỉ cần quyền mở các cổng UDP local cho SIP và RTP.

## Chọn đúng hướng cuộc gọi

| Trường hợp | Ý nghĩa | API bắt đầu |
| --- | --- | --- |
| Outbound | SUT gọi ra Sutel | `ExpectOutboundCall` |
| Inbound | Sutel gọi vào SUT | `Call` |

Quy trình luôn gồm ba bước:

1. Khai báo cuộc gọi Sutel cần mô phỏng hoặc chờ đợi.
2. Trigger hành động thật ở SUT.
3. Gọi `Wait()` để hoàn tất và kiểm tra kết quả.

## Quick start: nhận một cuộc gọi outbound

```go
package voice_test

import (
	"context"
	"testing"
	"time"

	"github.com/subiz/sutel"
)

func TestOutboundCall(t *testing.T) {
    t.Parallel()

    call, err := sutel.ExpectOutboundCall(
        context.Background(),
        sutel.OutboundScenario{
            LocalIP: "127.0.0.1",
            Timeout: 30 * time.Second,

            From: "19001234",
            To:   "0912345678",

            Behavior: sutel.Answer{
                RingingAfter: 100 * time.Millisecond,
                AnswerAfter:  300 * time.Millisecond,
            },

            Codecs: []sutel.Codec{
                sutel.PCMA,
                sutel.PCMU,
            },
        },
    )
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(call.Close)

    // Hai hàm này thuộc test harness của ứng dụng, không thuộc Sutel.
    configureSystemTrunk(t, call.SIPAddr())
    triggerOutboundCall(t, "0912345678")

    result, err := call.Wait()
    if err != nil {
        t.Fatal(err)
    }

    if !result.Outcome.Established {
        t.Fatal("call was not established")
    }
}
```

`ExpectOutboundCall` chỉ trả về sau khi Sutel đã bind xong socket SIP/RTP.
`call.SIPAddr()` vì vậy có thể dùng ngay để cấu hình SUT, không cần thêm
`time.Sleep`.

### Quy tắc trả lỗi

- `ExpectOutboundCall` và `Call` trả lỗi nếu scenario không hợp lệ, không bind
  được SIP/RTP socket hoặc không load được media đầu vào.
- `Wait` trả kết quả cuối cùng cùng lỗi signaling, media hoặc verification.
- `Session.Close` chỉ cancel, đóng tài nguyên và chờ session dừng nên không trả
  lỗi; dùng `Wait` nếu cần kết quả cuộc gọi.

Context truyền vào lúc tạo call kiểm soát toàn bộ vòng đời session. `Wait`
không nhận thêm context để tránh hai nguồn cancellation khác nhau.

## Các trường hợp sử dụng phổ biến

### Kiểm tra audio SUT gửi ra

Sutel nhận RTP, decode PCMA/PCMU rồi so sánh với file WAV mong đợi:

```go
call, err := sutel.ExpectOutboundCall(
    context.Background(),
    sutel.OutboundScenario{
        To:       "0912345678",
        Behavior: sutel.Answer{},

        ExpectAudio: &sutel.AudioExpectation{
            File:          "testdata/sample.wav",
            MinSimilarity: 0.95,
            MinCoverage:   0.80,
        },
    },
)
if err != nil {
    t.Fatal(err)
}

configureSystemTrunk(t, call.SIPAddr())
triggerGreeting(t, "0912345678", "testdata/sample.wav")

result, err := call.Wait()
if err != nil {
    t.Fatal(err)
}

t.Logf(
    "similarity=%.2f coverage=%.2f",
    result.Audio.Similarity,
    result.Audio.Coverage,
)

wav := result.ReceivedWAV()
if len(wav) == 0 {
    t.Fatal("no received audio")
}
```

- `Similarity` cho biết phần audio nhận được giống file gốc đến mức nào.
- `Coverage` cho biết Sutel nhận được bao nhiêu phần nội dung audio mong đợi.

Ví dụ audio đúng nhưng chỉ gửi được một nửa có thể có similarity cao và
coverage khoảng `0.5`.

### Phát audio cho SUT, không kiểm tra microphone

Trường hợp SUT chỉ gọi tới số `200` và chờ Sutel phát một bản nhạc sau khi bắt
máy:

```go
call, err := sutel.ExpectOutboundCall(
    context.Background(),
    sutel.OutboundScenario{
        To: "200",
        Behavior: sutel.Answer{
            Playback: &sutel.AudioPlayback{
                File: "testdata/sample.wav",
            },
        },
    },
)
if err != nil {
    t.Fatal(err)
}
t.Cleanup(call.Close)

configureSystemTrunk(t, call.SIPAddr())
triggerOutboundCall(t, "200")

if _, err := call.Wait(); err != nil {
    t.Fatal(err)
}
```

Sutel phát file một lần sau khi call được answer, rồi chủ động gửi BYE khi frame
audio cuối đã phát xong. Scenario không khai báo
`OutboundScenario.ExpectAudio`, vì vậy RTP/microphone SUT gửi ngược lại không
được assert và không làm call fail.

Ở phía SUT, test có thể lấy audio mà SUT thực sự decode được dưới dạng WAV
trong memory rồi dùng chính bộ so khớp của Sutel để kiểm tra:

```go
receivedWAV := sut.RecordedAudio() // []byte containing an uncompressed PCM WAV

match, err := sutel.VerifyAudio(
    sutel.AudioExpectation{
        File:          "testdata/sample.wav",
        MinSimilarity: 0.95,
        MinCoverage:   0.80,
    },
    receivedWAV,
)
if err != nil {
    t.Fatal(err)
}
t.Logf("similarity=%.2f coverage=%.2f", match.Similarity, match.Coverage)
```

`VerifyAudio` không lấy audio từ call Sutel: SUT phải cung cấp WAV bytes mà
chính receive path của SUT đã thu lại. Không cần tạo file tạm. Hàm decode và
normalize audio về PCM16 mono 8 kHz, sau đó dùng đúng alignment, similarity,
coverage cùng threshold như `ExpectAudio`. Khi threshold không đạt, hàm vẫn
trả metrics và lỗi tương thích với
`errors.Is(err, sutel.ErrVerification)`.

Sutel không negotiate Opus trong call, nhưng `VerifyAudio` hoạt động tốt với
audio mà SUT đã decode từ stream Opus của carrier thật: bộ so khớp chịu được
quantization của Opus ở bitrate VoIP lẫn độ trễ lookahead (~6.5 ms) mà
decoder để lại trong bản thu. Với audio đi qua Opus, ngưỡng khuyến nghị là
`MinSimilarity: 0.90`, `MinCoverage: 0.95`.

### Echo audio của SUT sau một khoảng trễ

Trường hợp cần kiểm tra SUT gửi microphone và nhận lại chính audio đó sau 3
giây:

```go
call, err := sutel.ExpectOutboundCall(
    context.Background(),
    sutel.OutboundScenario{
        To: "200",
        Behavior: sutel.Answer{
            Echo: &sutel.AudioEcho{
                Delay: 3 * time.Second,
            },
        },
    },
)
if err != nil {
    t.Fatal(err)
}
t.Cleanup(call.Close)

configureSystemTrunk(t, call.SIPAddr())
triggerEchoCall(t, "200")

if _, err := call.Wait(); err != nil {
    t.Fatal(err)
}
```

Sau khi call được answer, Sutel decode RTP audio nhận từ SUT, giữ một hàng đợi
3 giây rồi encode và phát ngược lại bằng codec đã negotiate. Audio được phát
theo pacing bình thường, không burst để bù thời gian. Nếu SUT ngừng gửi audio,
Sutel cũng ngừng phát sau khi hàng đợi đã hết.

Echo không bao gồm DTMF và không tự tạo assertion. `Answer.Playback` và
`Answer.Echo` loại trừ nhau; khai báo cả hai làm `ExpectOutboundCall` trả lỗi.

### Thử trực tiếp bằng pjsua

Repository có chương trình mẫu bind Sutel tại `127.0.0.1:5060`, nhận cuộc gọi
tới số `200` và phát file WAV một lần sau khi nhận ACK. Chạy Sutel trước:

```bash
go run ./examples/pjsua_playback testdata/sample.wav
```

Sau khi thấy dòng `waiting for pjsua`, mở terminal khác và gọi:

```bash
pjsua --id=sip:test@127.0.0.1 --local-port=5061 --ip-addr=127.0.0.1 --no-tcp sip:200@127.0.0.1:5060
```

Khi frame audio cuối phát xong, Sutel tự gửi BYE. pjsua kết thúc call và chương
trình mẫu in kết quả RTP rồi thoát; không cần gõ `h`.

`LocalPort: 5060` chỉ phù hợp cho chạy tay hoặc môi trường có port được cấp cố
định. Trong test chạy song song, để `LocalPort` bằng `0` để hệ điều hành tự chọn
port rồi cấu hình SUT bằng `call.SIPAddr()`.

### Chỉ cho phép một codec

```go
scenario.Codecs = []sutel.Codec{sutel.PCMA}
```

Hoặc:

```go
scenario.Codecs = []sutel.Codec{sutel.PCMU}
```

Nếu để trống, Sutel mặc định hỗ trợ cả PCMA và PCMU.

### Mô phỏng lỗi nhà mạng

Chỉ cần đổi `Behavior`:

```go
call, err := sutel.ExpectOutboundCall(
    context.Background(),
    sutel.OutboundScenario{
        To:       "0912345678",
        Behavior: sutel.Busy{},
    },
)
if err != nil {
    t.Fatal(err)
}
```

Các behavior thường dùng:

| Behavior | Kết quả SIP |
| --- | --- |
| `sutel.Busy{}` | `486 Busy Here` |
| `sutel.Reject{}` | `603 Decline` |
| `sutel.NotFound{}` | `404 Not Found` |
| `sutel.ServiceUnavailable{}` | `503 Service Unavailable` |
| `sutel.NoAnswer{}` | Ringing rồi chờ SUT gửi CANCEL |
| `sutel.Timeout{}` | Không phản hồi INVITE |
| `sutel.NetworkLoss{After: d}` | Answer, rồi biến mất sau `d` mà không gửi BYE |

Một lỗi nhà mạng được mô phỏng đúng vẫn là test thành công. Ví dụ với
`sutel.Busy{}`, `Wait()` trả về `nil` error khi SUT và Sutel hoàn thành đúng
flow 486 đã khai báo.

### Sutel chủ động kết thúc cuộc gọi

```go
Behavior: sutel.Answer{
    AnswerAfter: 300 * time.Millisecond,
    HangupAfter: 2 * time.Second,
},
```

Nếu `HangupAfter` bằng `0`, Sutel chờ SUT gửi BYE, ngoại trừ
`Answer.Playback`: playback luôn tự gửi BYE sau khi phát xong file.

### Mô phỏng mất mạng giữa cuộc gọi

```go
call, err := sutel.ExpectOutboundCall(
    context.Background(),
    sutel.OutboundScenario{
        LocalIP:   "127.0.0.1",
        LocalPort: 5060,
        To:        "200",
        Behavior: sutel.NetworkLoss{
            After: 3 * time.Second,
        },
    },
)
```

Sutel hoàn thành `100 → 180 → 200 → ACK`, giữ dialog trong ba giây tính từ ACK,
rồi biến mất im lặng: không gửi BYE hay response kết thúc và tự đóng toàn bộ
socket/goroutine. `Wait()` trả thành công với `Outcome.Established == true` và
`Outcome.TerminatedBy == NoParty`; test tự kiểm tra timeout, reconnect hoặc
recovery behavior của client.

### Gọi inbound qua trunk yêu cầu Digest authentication

Mặc định Sutel theo mô hình trusted-IP. Nếu SUT challenge INVITE bằng `401`,
khai báo credentials để Sutel ACK challenge rồi retry INVITE đúng một lần với
header `Authorization`:

```go
call, err := sutel.Call(
    context.Background(),
    sutel.InboundScenario{
        TargetSIPAddr: sutAddr,
        From:          "0912345678",
        To:            "100",
        Codec:         sutel.PCMA,
        DigestCredentials: &sutel.DigestCredentials{
            Username: "trunk-user",
            Password: "trunk-secret",
        },
    },
)
```

Hỗ trợ Digest MD5 (RFC 2617) và SHA-256 (RFC 7616), `qop` vắng mặt hoặc
`auth`; server gửi nhiều challenge thì Sutel chọn challenge đầu tiên dùng
thuật toán hỗ trợ. `401` lần thứ hai không được retry tiếp mà so với
`ExpectStatus` như mọi final response khác. `407` (proxy authentication) nằm
ngoài phạm vi vì Sutel là trunk endpoint trực tiếp. Lưu ý mỗi transaction có
`RingTimeout` riêng nên cuộc gọi bị challenge có thể chờ tới hai lần
`RingTimeout` (vẫn bị chặn bởi `Timeout` tổng).

### Gọi inbound và phát audio vào SUT

```go
system := startYourVoiceSystem(t) // helper thuộc test harness của ứng dụng

call, err := sutel.Call(
    context.Background(),
    sutel.InboundScenario{
        TargetSIPAddr: system.SIPAddr(),
        From:          "0912345678",
        To:            "19001234",
        Codec:         sutel.PCMA,

        Playback: &sutel.AudioPlayback{
            File: "testdata/customer-speech.wav",
        },

        RingTimeout: 5 * time.Second,
        CallDuration: 4 * time.Second,
    },
)
if err != nil {
    t.Fatal(err)
}

result, err := call.Wait()
if err != nil {
    t.Fatal(err)
}
```

Sutel tự decode WAV, chuyển về mono 8 kHz và encode đúng codec. Người dùng
không cần tự chuẩn bị file `.alaw` hoặc `.ulaw`.

### Kiểm tra audio SUT gửi về trong inbound call

Trường hợp Sutel gọi vào SUT, SUT answer rồi phát file `abc.wav` về phía Sutel:

```go
call, err := sutel.Call(
    context.Background(),
    sutel.InboundScenario{
        TargetSIPAddr: system.SIPAddr(),
        From:          "0912345678",
        To:            "19001234",
        Codec:         sutel.PCMA,

        ExpectAudio: &sutel.AudioExpectation{
            File:          "testdata/abc.wav",
            MinSimilarity: 0.95,
            MinCoverage:   0.80,
        },

        RingTimeout: 5 * time.Second,
        CallDuration: 5 * time.Second,
    },
)
if err != nil {
    t.Fatal(err)
}
t.Cleanup(call.Close)

result, err := call.Wait()
if err != nil {
    t.Fatal(err)
}

t.Logf(
    "similarity=%.2f coverage=%.2f",
    result.Audio.Similarity,
    result.Audio.Coverage,
)
```

Sutel không phát audio nếu `Playback` để trống, nhưng vẫn nhận, decode và kiểm
tra RTP SUT gửi về theo `AudioExpectation`. Có thể khai báo đồng thời
`Playback` và `ExpectAudio` để kiểm tra cuộc gọi full-duplex.

Nếu WAV expectation chỉ chứa silence, `Wait()` trả lỗi nhận diện được bằng
`errors.Is(err, sutel.ErrInvalidExpectation)`. Trường hợp này khác với
`ErrVerification`: fixture không hợp lệ, không phải audio của SUT bị mismatch.

Ở cả hai hướng gọi, tên field có cùng ý nghĩa:

| Field | Hướng media | Ý nghĩa |
| --- | --- | --- |
| `Playback` | Sutel → SUT | Audio Sutel chủ động phát |
| `ExpectAudio` | SUT → Sutel | Audio Sutel kỳ vọng nhận và verify |

### Kỳ vọng SUT từ chối inbound call

Sutel có thể gọi vào SUT và coi một final status cụ thể là kết quả mong đợi:

```go
call, err := sutel.Call(
    context.Background(),
    sutel.InboundScenario{
        TargetSIPAddr: system.SIPAddr(),
        From:          "0912345678",
        To:            "19001234",
        Codec:         sutel.PCMA,
        ExpectStatus:  486,
        RingTimeout:   5 * time.Second,
    },
)
if err != nil {
    t.Fatal(err)
}
t.Cleanup(call.Close)

result, err := call.Wait()
if err != nil {
    t.Fatal(err)
}
if result.Outcome.InviteFinalStatus != 486 {
    t.Fatalf("status=%d", result.Outcome.InviteFinalStatus)
}
```

`ExpectStatus` mặc định là `200`. Khi SUT trả đúng non-2xx đã khai báo, Sutel
gửi ACK và `Wait()` thành công. Status khác expectation làm `Wait()` trả
`ErrVerification`.

### Kiểm tra DTMF SUT gửi sang Sutel

RFC4733:

```go
DTMF: &sutel.DTMFExpectation{
    Method: sutel.RFC4733,
    Digits: "123#",
},
```

SIP INFO:

```go
DTMF: &sutel.DTMFExpectation{
    Method: sutel.SIPInfo,
    Digits: "123#",
},
```

Các event RFC4733 đã được gộp retransmission, vì vậy mỗi phím chỉ xuất hiện một
lần trong:

```go
events := result.DTMFEvents()
```

### Lấy trace và media để debug

Sutel giữ dữ liệu cuộc gọi trong memory

```go
trace := result.SIPTrace()
events := result.Events()
pcm := result.ReceivedPCM()
wav := result.ReceivedWAV()
```

Các slice trả về là bản sao độc lập. Chuỗi hoặc slice rỗng nghĩa là cuộc gọi
không tạo ra loại dữ liệu đó. Nếu muốn lưu file, test có thể tự ghi `wav` bằng
`os.WriteFile`.

### Gửi DTMF từ Sutel sang SUT

Thêm action vào cuộc gọi inbound:

```go
DTMF: []sutel.DTMFAction{
    {
        Method:   sutel.SIPInfo, // hoặc sutel.RFC4733
        Digits:   "123#",
        Interval: 200 * time.Millisecond,
    },
},
```

Sutel tự tạo và gửi các RTP telephone-event; không cần chuẩn bị PCAP hay fixture
DTMF riêng.

### Mô phỏng 183 Early Media

```go
Behavior: sutel.EarlyMedia{
    File:          "testdata/ringback.wav",
    ProgressAfter: 100 * time.Millisecond,
    AnswerAfter:   2 * time.Second,
},
```

### Early media rồi kết thúc bằng lỗi

Một số telco gửi `183` và RTP ringback/thông báo trước khi trả lỗi cuối như
`486 Busy Here`. Dùng `EarlyFailure` cho flow này:

```go
Behavior: sutel.EarlyFailure{
    File:          "testdata/ringback.wav",
    ProgressAfter: 2 * time.Second,
    FailureAfter:  4 * time.Second,

    FinalStatus: 486, // mặc định là 486 nếu để 0
    ReasonHeader: `Q.850;cause=17;text="USER_BUSY"`,
},
```

Flow tương ứng là `100 → 183 + SDP/RTP → 486 → ACK`. Đây vẫn là một scenario
thành công khi SUT hoàn thành đúng flow đã khai báo; `Outcome.Established` sẽ
bằng `false`.

## Kiểm tra số điện thoại

`OutboundScenario.From` và `OutboundScenario.To` so khớp chính xác phần user
của SIP URI. Chuỗi rỗng nghĩa là bỏ qua kiểm tra. `To` được kiểm tra ở cả
header `To` và Request-URI.

Sutel không tự chuẩn hóa số Việt Nam: `091...`, `8491...` và `+8491...` là ba
giá trị khác nhau.

## Chạy test song song

Sutel hỗ trợ `t.Parallel()`. Mỗi call tự sở hữu SIP/RTP socket và dữ liệu riêng:

```go
func TestA(t *testing.T) {
    t.Parallel()
    call, err := sutel.ExpectOutboundCall(
        context.Background(),
        sutel.OutboundScenario{To: "0912345678"},
    )
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(call.Close)
    // ...
}
```

Không có global listener hoặc mutable state dùng chung giữa các test.

## Cấu hình nâng cao

Các tùy chọn runtime cần thiết nằm ngay trong scenario:

```go
call, err := sutel.ExpectOutboundCall(
    ctx,
    sutel.OutboundScenario{
        LocalIP:   "127.0.0.1",
        LocalPort: 0,
        Timeout:   30 * time.Second,
        To:        "0912345678",
    },
)
if err != nil {
    t.Fatal(err)
}
t.Cleanup(call.Close)
```

Nếu bỏ trống, `LocalIP` mặc định là `127.0.0.1`, `LocalPort` mặc định là `0`
(hệ điều hành tự chọn port) và `Timeout` mặc định là 20 giây. `LocalPort` chỉ
điều khiển SIP; RTP luôn dùng port động được advertise qua SDP. Các chi tiết
transaction như SIP T1/T2, reorder window và message-size cap là implementation
defaults, không nằm trong public API.

Khi lỗi, Sutel đưa SIP trace, RTP stats và audio metrics vào diagnostics trong
memory. Thư viện không tự ghi file và test thành công không spam stdout.

## Kiến trúc rất ngắn gọn

Sutel chứa một SIP user agent tối giản viết bằng Go cho đúng phạm vi integration
test: UDP, INVITE/ACK/CANCEL/BYE/INFO và SDP cho PCMA/PCMU. Media engine Go gửi
và nhận RTP, phát WAV, xử lý RFC4733, record WAV và thực hiện assertion. Đây
không phải PBX hay SIP stack tổng quát. Phần cú pháp SDP dùng `pion/sdp`; RTP
dùng `pion/rtp`. Tất cả mặc định chạy trên `127.0.0.1`.

Chi tiết kỹ thuật và yêu cầu triển khai nằm trong [spec.md](spec.md).
