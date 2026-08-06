package mail

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseMIMEMessage(t *testing.T) {
	raw := strings.Join([]string{
		`Message-ID: <message-1@example.com>`,
		`Date: Thu, 6 Aug 2026 12:34:56 +0800`,
		`From: =?UTF-8?B?Sm9zw6k=?= <Sender@Example.com>`,
		`To: Alias <ALIAS@example.com>`,
		`Cc: Copy <copy@example.com>`,
		`Subject: =?ISO-8859-1?Q?Ol=E1?=`,
		`MIME-Version: 1.0`,
		`Content-Type: multipart/mixed; boundary=outer`,
		``,
		`--outer`,
		`Content-Type: multipart/alternative; boundary=inner`,
		``,
		`--inner`,
		`Content-Type: text/plain; charset=iso-8859-1`,
		`Content-Transfer-Encoding: quoted-printable`,
		``,
		`Ol=E1 texto`,
		`--inner`,
		`Content-Type: text/html; charset=utf-8`,
		`Content-Transfer-Encoding: base64`,
		``,
		`PHA+aGVsbG88L3A+`,
		`--inner--`,
		`--outer`,
		`Content-Type: application/octet-stream`,
		`Content-Disposition: attachment; filename*=UTF-8''report%20one.txt`,
		`Content-Transfer-Encoding: base64`,
		``,
		`aGVsbG8=`,
		`--outer--`,
		``,
	}, "\r\n")

	got, err := parseMIMEMessage([]byte(raw), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.messageID != "<message-1@example.com>" || got.subject != "Olá" {
		t.Fatalf("headers = message-id %q subject %q", got.messageID, got.subject)
	}
	if got.headerDate == nil || got.headerDate.Year() != 2026 {
		t.Fatalf("header date = %v", got.headerDate)
	}
	if len(got.from) != 1 || got.from[0].Name != "José" || got.from[0].Email != "sender@example.com" {
		t.Fatalf("from = %#v", got.from)
	}
	if got.textBody != "Olá texto" || got.htmlBody != "<p>hello</p>" {
		t.Fatalf("bodies = text %q html %q", got.textBody, got.htmlBody)
	}
	if len(got.attachments) != 1 {
		t.Fatalf("attachments = %#v", got.attachments)
	}
	attachment := got.attachments[0]
	if attachment.Filename != "report one.txt" || attachment.ContentType != "application/octet-stream" || attachment.Size != 5 {
		t.Fatalf("attachment = %#v", attachment)
	}
	if got.bodyTruncated {
		t.Fatal("body unexpectedly truncated")
	}
}

func TestParseMIMEMessageBodyBudget(t *testing.T) {
	raw := []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n123456789")
	got, err := parseMIMEMessage(raw, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.textBody != "12345" || !got.bodyTruncated {
		t.Fatalf("text = %q, truncated = %v", got.textBody, got.bodyTruncated)
	}
}

func TestParseMIMEMessagePartBudget(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("Content-Type: multipart/mixed; boundary=x\r\n\r\n")
	for i := 0; i < maxMIMEParts+10; i++ {
		raw.WriteString("--x\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	raw.WriteString("--x--\r\n")

	got, err := parseMIMEMessage([]byte(raw.String()), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !got.bodyTruncated {
		t.Fatal("part budget exhaustion was not marked truncated")
	}
}

func TestParseMIMEMessageDepthBudget(t *testing.T) {
	var raw strings.Builder
	for depth := 0; depth < maxMIMEDepth+3; depth++ {
		boundary := fmt.Sprintf("b%d", depth)
		if depth == 0 {
			raw.WriteString("Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n")
		} else {
			raw.WriteString("Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n")
		}
		raw.WriteString("--" + boundary + "\r\n")
	}
	raw.WriteString("Content-Type: text/plain\r\n\r\ndeep body\r\n")
	for depth := maxMIMEDepth + 2; depth >= 0; depth-- {
		raw.WriteString("--" + fmt.Sprintf("b%d", depth) + "--\r\n")
	}

	got, err := parseMIMEMessage([]byte(raw.String()), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !got.bodyTruncated {
		t.Fatal("depth budget exhaustion was not marked truncated")
	}
}

func TestParseMIMEMessageMetadataBudgets(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("Message-ID: <")
	raw.WriteString(strings.Repeat("m", defaultMaxMessageIDBytes))
	raw.WriteString(">\r\nSubject: ")
	raw.WriteString(strings.Repeat("界", defaultMaxSubjectBytes/3+16))
	raw.WriteString("\r\n")

	writeAddresses := func(field, prefix string, count int) {
		raw.WriteString(field + ": ")
		for i := 0; i < count; i++ {
			if i > 0 {
				raw.WriteString(", ")
			}
			name := fmt.Sprintf("Person%d", i)
			if field == "From" && i == 0 {
				name = strings.Repeat("N", maxAddressNameBytes+32)
			}
			raw.WriteString(fmt.Sprintf("%s <%s%d@example.com>", name, prefix, i))
		}
		raw.WriteString("\r\n")
	}
	writeAddresses("From", "from", maxFromAddresses+2)
	writeAddresses("To", "to", maxToAddresses+8)
	writeAddresses("Cc", "cc", maxCCAddresses+8)
	raw.WriteString("\r\nbody")

	got, err := parseMIMEMessage([]byte(raw.String()), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.messageID != "" {
		t.Fatalf("over-limit Message-ID = %q, want empty", got.messageID)
	}
	if len(got.subject) > defaultMaxSubjectBytes || !utf8.ValidString(got.subject) {
		t.Fatalf("subject bytes = %d, valid UTF-8 = %v", len(got.subject), utf8.ValidString(got.subject))
	}
	if len(got.from) != maxFromAddresses || len(got.to) != maxToAddresses || len(got.cc) != defaultMaxAddresses-maxFromAddresses-maxToAddresses {
		t.Fatalf("address counts = From %d To %d Cc %d", len(got.from), len(got.to), len(got.cc))
	}
	if len(got.from[0].Name) != maxAddressNameBytes {
		t.Fatalf("first display name bytes = %d, want %d", len(got.from[0].Name), maxAddressNameBytes)
	}
	if !got.bodyTruncated {
		t.Fatal("metadata budget truncation was not reported")
	}
}

func TestParseMIMEMessageAttachmentBudgets(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("Content-Type: multipart/mixed; boundary=attachments\r\n\r\n")
	for i := 0; i < defaultMaxAttachments+2; i++ {
		raw.WriteString("--attachments\r\nContent-Type: ")
		if i == 0 {
			raw.WriteString("application/" + strings.Repeat("x", maxAttachmentContentTypeBytes+32))
		} else {
			raw.WriteString("application/octet-stream")
		}
		raw.WriteString("\r\nContent-Disposition: attachment; filename=\"")
		raw.WriteString(strings.Repeat("f", defaultMaxFilenameBytes+32))
		raw.WriteString(fmt.Sprintf("-%d.txt\"\r\n\r\nx\r\n", i))
	}
	raw.WriteString("--attachments--\r\n")

	got, err := parseMIMEMessage([]byte(raw.String()), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.attachments) != defaultMaxAttachments {
		t.Fatalf("attachments = %d, want %d", len(got.attachments), defaultMaxAttachments)
	}
	if got.attachments[0].ContentType != "application/octet-stream" {
		t.Fatalf("over-limit content type = %q", got.attachments[0].ContentType)
	}
	for index, attachment := range got.attachments {
		if len(attachment.Filename) > defaultMaxFilenameBytes || !utf8.ValidString(attachment.Filename) {
			t.Fatalf("attachment %d filename bytes = %d, valid UTF-8 = %v", index, len(attachment.Filename), utf8.ValidString(attachment.Filename))
		}
	}
	if !got.bodyTruncated {
		t.Fatal("attachment metadata truncation was not reported")
	}
}

func TestParseMIMEMessageResultBudget(t *testing.T) {
	const dynamicBudget = int64(64)
	limits := defaultMIMELimits(1024, parsedMessageBaseBytes+dynamicBudget)
	raw := []byte("Message-ID: <message@example.com>\r\nSubject: a deliberately long subject\r\nFrom: Sender <sender@example.com>\r\n\r\n" + strings.Repeat("body", 256))

	got, err := parseMIMEMessageWithLimits(raw, limits)
	if err != nil {
		t.Fatal(err)
	}
	if size := parsedMessageResultBytes(got); size > parsedMessageBaseBytes+dynamicBudget {
		t.Fatalf("parsed result bytes = %d, budget = %d", size, parsedMessageBaseBytes+dynamicBudget)
	}
	if !got.bodyTruncated {
		t.Fatal("result-budget truncation was not reported")
	}
}

func TestParseMIMEMessageUTF8BodyTruncation(t *testing.T) {
	got, err := parseMIMEMessage([]byte("Content-Type: text/plain; charset=utf-8\r\n\r\n你好"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.textBody != "你" || !utf8.ValidString(got.textBody) || !got.bodyTruncated {
		t.Fatalf("text = %q, valid UTF-8 = %v, truncated = %v", got.textBody, utf8.ValidString(got.textBody), got.bodyTruncated)
	}
}

func TestParseMIMEMessageLimitsUnterminatedTopLevelHeader(t *testing.T) {
	raw := []byte("X-Long: " + strings.Repeat("x", maxTopLevelHeaderBytes+1024))
	got, err := parseMIMEMessage(raw, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !got.bodyTruncated {
		t.Fatal("unterminated over-limit top-level header was not reported")
	}
}

func TestTopLevelHeaderEndUsesEarliestSeparator(t *testing.T) {
	raw := []byte("X-Test: one\n\nbody\r\n\r\ntrailer")
	want := strings.Index(string(raw), "\n\n")
	index, separatorLength := topLevelHeaderEnd(raw)
	if index != want || separatorLength != 2 {
		t.Fatalf("header end = %d/%d, want %d/2", index, separatorLength, want)
	}
}

func TestParseMIMEMessageOnlyAllowsPartialMIMEForTruncatedSource(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\npartial")
	limits := defaultMIMELimits(1024, 2048)
	if _, err := parseMIMEMessageWithLimits(raw, limits); err == nil {
		t.Fatal("malformed complete multipart unexpectedly succeeded")
	}
	got, err := parseMIMEMessageWithOptions(raw, limits, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.bodyTruncated {
		t.Fatal("accepted partial multipart was not reported as truncated")
	}
}

func FuzzParseMIMEMessage(f *testing.F) {
	f.Add([]byte("Subject: hello\r\n\r\nbody"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x--\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		parsed, _ := parseMIMEMessage(raw, 64)
		if len(parsed.textBody)+len(parsed.htmlBody) > 64 {
			t.Fatalf("body budget exceeded: text=%d html=%d", len(parsed.textBody), len(parsed.htmlBody))
		}
	})
}
