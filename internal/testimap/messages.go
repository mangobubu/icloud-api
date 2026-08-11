package testimap

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	stdmail "net/mail"
	"net/textproto"
	"strings"
	"time"
)

const maxControlMessageBytes = 16 << 20

//go:embed static/chatgpt-verification.html
var verificationHTMLTemplate string

type AttachmentInput struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

type MessageInput struct {
	FromName    string            `json:"from_name"`
	FromEmail   string            `json:"from_email"`
	Alias       string            `json:"alias"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	ReceivedAt  string            `json:"received_at,omitempty"`
	Seen        bool              `json:"seen"`
	Route       string            `json:"route,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []AttachmentInput `json:"attachments,omitempty"`
}

func (input MessageInput) InternalDate(now time.Time) (time.Time, error) {
	if strings.TrimSpace(input.ReceivedAt) == "" {
		return now.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ReceivedAt))
	if err != nil {
		return time.Time{}, fmt.Errorf("received_at must be RFC3339: %w", err)
	}
	return parsed.UTC(), nil
}

func RenderMessage(input MessageInput, forwardAddress string, now time.Time) ([]byte, error) {
	input.FromName = strings.TrimSpace(input.FromName)
	input.Subject = strings.TrimSpace(input.Subject)
	if strings.ContainsAny(input.FromName+input.Subject, "\r\n") {
		return nil, errors.New("from_name and subject must be single-line values")
	}
	if input.FromEmail == "" {
		input.FromEmail = "sender@example.test"
	}
	var err error
	if input.FromEmail, err = normalizeMessageAddress(input.FromEmail); err != nil {
		return nil, errors.New("from_email must be a valid bare email address")
	}
	if input.Alias, err = normalizeMessageAddress(input.Alias); err != nil {
		return nil, errors.New("alias must be a valid email address")
	}
	if forwardAddress, err = normalizeMessageAddress(forwardAddress); err != nil {
		return nil, errors.New("forwarding address must be a valid email address")
	}
	if input.Subject == "" {
		input.Subject = "Test message"
	}
	if input.Text == "" && input.HTML == "" {
		input.Text = "Test message body."
	}
	internalDate, err := input.InternalDate(now)
	if err != nil {
		return nil, err
	}
	messageID, err := randomMessageID()
	if err != nil {
		return nil, err
	}

	var output bytes.Buffer
	writeHeader(&output, "Date", internalDate.Format(time.RFC1123Z))
	from := input.FromEmail
	if input.FromName != "" {
		from = mime.QEncoding.Encode("UTF-8", input.FromName) + " <" + input.FromEmail + ">"
	}
	writeHeader(&output, "From", from)
	writeHeader(&output, "To", input.Alias)
	writeHeader(&output, "Subject", mime.QEncoding.Encode("UTF-8", input.Subject))
	writeHeader(&output, "Message-ID", "<"+messageID+"@example.test>")
	writeHeader(&output, "MIME-Version", "1.0")

	switch strings.ToLower(strings.TrimSpace(input.Route)) {
	case "", "valid_hme":
		writeHeader(&output, "Original-Recipient", "rfc822; "+forwardAddress)
		writeHeader(&output, "X-ICLOUD-HME", fmt.Sprintf(
			"p=%s; f=%s; r=to; s=%s", input.Alias, forwardAddress, input.FromEmail,
		))
	case "invalid_hme":
		writeHeader(&output, "Original-Recipient", "rfc822; "+forwardAddress)
		writeHeader(&output, "X-ICLOUD-HME", fmt.Sprintf(
			"p=%s; f=wrong-forward@example.test; r=to; s=%s", input.Alias, input.FromEmail,
		))
	case "strong_header":
		writeHeader(&output, "Delivered-To", input.Alias)
	case "weak_header":
		// To is intentionally the only recipient header.
	default:
		return nil, errors.New("route must be valid_hme, invalid_hme, strong_header, or weak_header")
	}
	for name, value := range input.Headers {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !validHeaderName(name) || value == "" || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("custom headers must have single-line names and values")
		}
		name = textproto.CanonicalMIMEHeaderKey(name)
		if protectedMessageHeader(name) {
			return nil, fmt.Errorf("custom header %s is managed by the test server", name)
		}
		writeHeader(&output, name, value)
	}

	if len(input.Attachments) > 0 {
		mixed := multipart.NewWriter(&output)
		writeHeader(&output, "Content-Type", `multipart/mixed; boundary="`+mixed.Boundary()+`"`)
		output.WriteString("\r\n")
		if err := writeAlternativePart(mixed, input.Text, input.HTML); err != nil {
			return nil, err
		}
		for _, attachment := range input.Attachments {
			if err := writeAttachment(mixed, attachment); err != nil {
				return nil, err
			}
		}
		if err := mixed.Close(); err != nil {
			return nil, err
		}
	} else if input.Text != "" && input.HTML != "" {
		alternative := multipart.NewWriter(&output)
		writeHeader(&output, "Content-Type", `multipart/alternative; boundary="`+alternative.Boundary()+`"`)
		output.WriteString("\r\n")
		if err := writeTextPart(alternative, "text/plain; charset=UTF-8", input.Text); err != nil {
			return nil, err
		}
		if err := writeTextPart(alternative, "text/html; charset=UTF-8", input.HTML); err != nil {
			return nil, err
		}
		if err := alternative.Close(); err != nil {
			return nil, err
		}
	} else {
		contentType, content := "text/plain; charset=UTF-8", input.Text
		if input.HTML != "" {
			contentType, content = "text/html; charset=UTF-8", input.HTML
		}
		writeHeader(&output, "Content-Type", contentType)
		writeHeader(&output, "Content-Transfer-Encoding", "8bit")
		output.WriteString("\r\n")
		output.WriteString(normalizeCRLF(content))
		output.WriteString("\r\n")
	}
	if output.Len() > maxControlMessageBytes {
		return nil, fmt.Errorf("rendered message exceeds %d bytes", maxControlMessageBytes)
	}
	return output.Bytes(), nil
}

func protectedMessageHeader(name string) bool {
	switch strings.ToLower(name) {
	case "date", "from", "to", "subject", "message-id", "mime-version", "content-type",
		"content-transfer-encoding", "original-recipient", "x-icloud-hme", "delivered-to":
		return true
	default:
		return false
	}
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if char < 33 || char > 126 || char == ':' {
			return false
		}
	}
	return true
}

func normalizeMessageAddress(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 320 || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("invalid email address")
	}
	parsed, err := stdmail.ParseAddress(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return "", errors.New("invalid email address")
	}
	return value, nil
}

func writeHeader(output *bytes.Buffer, name, value string) {
	output.WriteString(name)
	output.WriteString(": ")
	output.WriteString(value)
	output.WriteString("\r\n")
}

func writeAlternativePart(writer *multipart.Writer, text, htmlBody string) error {
	if text != "" && htmlBody != "" {
		headers := make(textproto.MIMEHeader)
		boundary := randomBoundary()
		headers.Set("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		part, err := writer.CreatePart(headers)
		if err != nil {
			return err
		}
		alternative := multipart.NewWriter(part)
		if err := alternative.SetBoundary(boundary); err != nil {
			return err
		}
		if err := writeTextPart(alternative, "text/plain; charset=UTF-8", text); err != nil {
			return err
		}
		if err := writeTextPart(alternative, "text/html; charset=UTF-8", htmlBody); err != nil {
			return err
		}
		return alternative.Close()
	}
	contentType, content := "text/plain; charset=UTF-8", text
	if htmlBody != "" {
		contentType, content = "text/html; charset=UTF-8", htmlBody
	}
	return writeTextPart(writer, contentType, content)
}

func writeTextPart(writer *multipart.Writer, contentType, content string) error {
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Type", contentType)
	headers.Set("Content-Transfer-Encoding", "8bit")
	part, err := writer.CreatePart(headers)
	if err != nil {
		return err
	}
	_, err = ioWriteString(part, normalizeCRLF(content)+"\r\n")
	return err
}

func writeAttachment(writer *multipart.Writer, input AttachmentInput) error {
	filename := strings.TrimSpace(input.Filename)
	if filename == "" || strings.ContainsAny(filename, "\r\n\"") {
		return errors.New("attachment filename is required and must be a single-line token")
	}
	contentType := strings.TrimSpace(input.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return fmt.Errorf("attachment %s has an invalid content type", filename)
	}
	if parameters == nil {
		parameters = make(map[string]string)
	}
	content, err := base64.StdEncoding.DecodeString(input.ContentBase64)
	if err != nil {
		return fmt.Errorf("decode attachment %s: %w", filename, err)
	}
	parameters["name"] = filename
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Type", mime.FormatMediaType(mediaType, parameters))
	headers.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	headers.Set("Content-Transfer-Encoding", "base64")
	part, err := writer.CreatePart(headers)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	_, err = ioWriteString(part, encoded+"\r\n")
	return err
}

func normalizeCRLF(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func randomMessageID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomBoundary() string {
	value, err := randomMessageID()
	if err != nil {
		return "icloud-api-test-boundary"
	}
	return "icloud-api-" + value
}

func ioWriteString(writer interface{ Write([]byte) (int, error) }, value string) (int, error) {
	return writer.Write([]byte(value))
}

func PresetMessage(name, alias string, now time.Time) (MessageInput, int, error) {
	base := MessageInput{
		FromName:  "Example Service",
		FromEmail: "notice@example.test",
		Alias:     alias,
		Subject:   "Test notification",
		Text:      "This is a test notification.",
		Route:     "valid_hme",
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "verification-code":
		base.FromName = "ChatGPT"
		base.FromEmail = "noreply@tm.openai.com"
		base.Subject = "Your temporary ChatGPT verification code"
		base.Text = "Enter this temporary verification code to continue: 123456\n\nPlease ignore this email if this wasn’t you trying to create a ChatGPT account.\n\nBest,\nThe ChatGPT team"
		base.HTML = verificationHTML("123456")
		base.Headers = map[string]string{"Reply-To": "noreply@tm.openai.com"}
	case "plain":
		base.Subject = "Plain text test message"
		base.Text = "A plain text message for UI testing."
	case "html":
		base.Subject = "HTML test message"
		base.Text = "HTML test message fallback."
		base.HTML = `<html><body><h1>HTML test message</h1><p>Rendered for UI testing.</p></body></html>`
	case "attachment":
		base.Subject = "Message with an attachment"
		base.Text = "This message contains one small attachment."
		base.Attachments = []AttachmentInput{{
			Filename: "sample.txt", ContentType: "text/plain",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("test attachment\n")),
		}}
	case "old":
		base.Subject = "Old verification code"
		base.Text = "Your old verification code is 654321."
		base.ReceivedAt = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	case "seen":
		base.Subject = "Already read verification code"
		base.Text = "Your verification code is 112233."
		base.Seen = true
	case "invalid-hme":
		base.Subject = "Invalid HME route"
		base.Text = "Your verification code is 445566."
		base.Route = "invalid_hme"
	case "bulk":
		base.Subject = "Bulk test message"
		base.Text = "Bulk message generated for sync progress testing."
		return base, 300, nil
	default:
		return MessageInput{}, 0, fmt.Errorf("unknown preset %q", name)
	}
	return base, 1, nil
}

func verificationHTML(code string) string {
	return strings.NewReplacer(
		"{{VERIFICATION_CODE}}", code,
		"{{LOGO_URL}}", "https://example.test/assets/chatgpt-logo.png",
		"{{CHATGPT_URL}}", "https://example.test/chatgpt",
		"{{HELP_CENTER_URL}}", "https://example.test/help",
	).Replace(verificationHTMLTemplate)
}
