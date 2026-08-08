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

Sutel cần **SIPp 3.7.7** trên máy chạy test:

```bash
sipp -v
```

Sutel tìm binary theo thứ tự:

1. `Config.SIPpBinary`
2. biến môi trường `SIPP_BIN`
3. `sipp` trong `PATH`

Nếu cần gửi RFC4733 DTMF từ Sutel sang SUT, SIPp phải được build với PCAP
support. Có thể kiểm tra dependency ngay đầu test:

```go
testkit.RequireSIPp(t)
testkit.RequireSIPpCapabilities(t, sutel.RequirePCAPPlayback)
```

## Chọn đúng hướng cuộc gọi

| Trường hợp | Ý nghĩa | API bắt đầu |
| --- | --- | --- |
| Outbound | SUT gọi ra Sutel | `ExpectOutboundCall` |
| Inbound | Sutel gọi vào SUT | `Call` |

Quy trình luôn gồm ba bước:

1. Khai báo cuộc gọi Sutel cần mô phỏng hoặc chờ đợi.
2. Trigger hành động thật ở SUT.
3. Gọi `RequireSuccess(t)` hoặc `Wait(ctx)` để hoàn tất và kiểm tra kết quả.

## Quick start: nhận một cuộc gọi outbound

Ví dụ dưới đây dùng `testkit`, cách ngắn nhất khi viết Go test:

```go
package voice_test

import (
    "testing"
    "time"

    "github.com/subiz/sutel"
    "github.com/subiz/sutel/testkit"
)

func TestOutboundCall(t *testing.T) {
    t.Parallel()

    carrier := testkit.NewCarrier(t)

    call := carrier.ExpectOutboundCall(
        sutel.OutboundScenario{
            From: sutel.ExactNumber("19001234"),
            To:   sutel.ExactNumber("0912345678"),

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

    // Hai hàm này thuộc test harness của ứng dụng, không thuộc Sutel.
    configureSystemTrunk(t, call.SIPAddr())
    triggerOutboundCall(t, "0912345678")

    result := call.RequireSuccess(t)

    if !result.Outcome.Established {
        t.Fatal("call was not established")
    }
}
```

`call.SIPAddr()` chỉ được trả về sau khi SIPp đã sẵn sàng nhận cuộc gọi. Không
cần thêm `time.Sleep` trước khi trigger SUT.

## Các trường hợp sử dụng phổ biến

### Kiểm tra audio SUT gửi ra

Sutel nhận RTP, decode PCMA/PCMU rồi so sánh với file MP3 hoặc WAV mong đợi:

```go
call := carrier.ExpectOutboundCall(
    sutel.OutboundScenario{
        To:       sutel.ExactNumber("0912345678"),
        Behavior: sutel.Answer{},

        Audio: &sutel.AudioExpectation{
            File:          "testdata/greeting.mp3",
            MinSimilarity: 0.95,
            MinCoverage:   0.80,
        },
    },
)

configureSystemTrunk(t, call.SIPAddr())
triggerGreeting(t, "0912345678", "testdata/greeting.mp3")

result := call.RequireSuccess(t)

t.Logf(
    "similarity=%.2f coverage=%.2f",
    result.Audio.Similarity,
    result.Audio.Coverage,
)

receivedPath := filepath.Join(carrier.ArtifactDir(), "received.wav") // import "path/filepath"
if err := result.SaveReceivedAudio(receivedPath); err != nil {
    t.Fatal(err)
}
```

- `Similarity` cho biết phần audio nhận được giống file gốc đến mức nào.
- `Coverage` cho biết Sutel nhận được bao nhiêu phần nội dung audio mong đợi.

Ví dụ audio đúng nhưng chỉ gửi được một nửa có thể có similarity cao và
coverage khoảng `0.5`.

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
call := carrier.ExpectOutboundCall(
    sutel.OutboundScenario{
        To:       sutel.ExactNumber("0912345678"),
        Behavior: sutel.Busy{},
    },
)
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

Một lỗi nhà mạng được mô phỏng đúng vẫn là test thành công. Ví dụ với
`sutel.Busy{}`, `RequireSuccess(t)` pass khi SUT và Sutel hoàn thành đúng flow
486 đã khai báo.

### Sutel chủ động kết thúc cuộc gọi

```go
Behavior: sutel.Answer{
    AnswerAfter: 300 * time.Millisecond,
    HangupAfter: 2 * time.Second,
},
```

Nếu `HangupAfter` bằng `0`, Sutel chờ SUT gửi BYE.

### Gọi inbound và phát audio vào SUT

```go
system := startYourVoiceSystem(t) // helper thuộc test harness của ứng dụng

call := carrier.Call(
    sutel.InboundScenario{
        TargetSIPAddr: system.SIPAddr(),
        From:          "0912345678",
        To:            "19001234",
        Codec:         sutel.PCMA,

        Audio: &sutel.AudioPlayback{
            File: "testdata/customer-speech.wav",
        },

        RingTimeout: 5 * time.Second,
        CallDuration: 4 * time.Second,
    },
)

result := call.RequireSuccess(t)
```

Sutel tự decode MP3/WAV, chuyển về mono 8 kHz và encode đúng codec. Người dùng
không cần tự chuẩn bị file `.alaw` hoặc `.ulaw`.

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

Gửi bằng `sutel.RFC4733` yêu cầu SIPp có PCAP support. Sutel không tự động hạ
cấp sang SIP INFO nếu capability này bị thiếu.

### Mô phỏng 183 Early Media

```go
Behavior: sutel.EarlyMedia{
    File:          "testdata/ringback.wav",
    ProgressAfter: 100 * time.Millisecond,
    AnswerAfter:   2 * time.Second,
},
```

## Matcher số điện thoại

```go
sutel.ExactNumber("0912345678")
sutel.NumberRegexp(`^(\+84|84|0)912345678$`)
sutel.AnyNumber()
```

Sutel không tự chuẩn hóa số Việt Nam. Test quyết định `091...`, `8491...` và
`+8491...` có được xem là giống nhau hay không.

## Chạy test song song

Sutel hỗ trợ `t.Parallel()`. Mỗi test phải tạo một carrier riêng:

```go
func TestA(t *testing.T) {
    t.Parallel()
    carrier := testkit.NewCarrier(t)
    // ...
}
```

Không chia sẻ một `Carrier` giữa các test đang chạy đồng thời. Mỗi instance tự
sở hữu SIPp process, SIP/RTP port, thư mục làm việc, log và recording riêng.

## Cấu hình nâng cao

Trong phần lớn test, chỉ cần `testkit.NewCarrier(t)`. Dùng core API khi cần kiểm
soát binary, timeout hoặc artifacts:

```go
carrier, err := sutel.New(
    sutel.Config{
        SIPpBinary:     "/opt/sipp/bin/sipp",
        ArtifactDir:    "artifacts/sutel",
        ArtifactPolicy: sutel.ArtifactsOnFailure,
        DefaultTimeout:  30 * time.Second,
    },
)
if err != nil {
    t.Fatal(err)
}

t.Cleanup(func() {
    if err := carrier.Close(); err != nil {
        t.Errorf("close Sutel: %v", err)
    }
})
```

Artifact policy:

- `ArtifactsOnFailure`: chỉ giữ log và media khi test lỗi; đây là mặc định.
- `ArtifactsAlways`: luôn giữ artifacts.
- `ArtifactsNever`: xóa working directory sau khi đã thu thập kết quả.

Khi lỗi, Sutel cung cấp SIP trace, RTP stats, audio metrics và đường dẫn artifact
trong error. Test thành công không spam stdout.

## Kiến trúc rất ngắn gọn

Sutel dùng SIPp cho SIP signaling và phát RTP. Go test harness quản lý process,
port, timeout và scenario. Go RTP verifier nhận RTP từ SUT, decode audio,
record WAV và thực hiện assertion. Tất cả mặc định chạy trên `127.0.0.1`.

Chi tiết kỹ thuật và yêu cầu triển khai nằm trong [spec.md](spec.md).
