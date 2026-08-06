package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"icloud-api/internal/domain"
)

const (
	maxMIMEParts                  = 100
	maxMIMEDepth                  = 20
	defaultMaxSubjectBytes        = 8 << 10
	defaultMaxMessageIDBytes      = 2 << 10
	defaultMaxAddresses           = 64
	defaultMaxAttachments         = 32
	defaultMaxFilenameBytes       = 1 << 10
	defaultMetadataResultBytes    = 128 << 10
	maxEncodedSubjectBytes        = 16 << 10
	maxTopLevelHeaderBytes        = 256 << 10
	maxAddressHeaderSourceBytes   = 64 << 10
	maxFromAddresses              = 8
	maxToAddresses                = 32
	maxCCAddresses                = 32
	maxAddressNameBytes           = 1 << 10
	maxAddressEmailBytes          = 320
	maxAddressHeaderValueBytes    = 64 << 10
	maxMIMEParameterHeaderBytes   = 16 << 10
	maxAttachmentContentTypeBytes = 256
	parsedMessageBaseBytes        = 256
	mailAddressResultBytes        = 64
	attachmentResultBytes         = 64
)

var errMIMEBudgetExhausted = errors.New("MIME complexity budget exhausted")

type parsedMessage struct {
	messageID     string
	headerDate    *time.Time
	from          []domain.MailAddress
	to            []domain.MailAddress
	cc            []domain.MailAddress
	subject       string
	textBody      string
	htmlBody      string
	attachments   []domain.Attachment
	bodyTruncated bool
}

type mimeLimits struct {
	maxBodyBytes      int64
	maxSubjectBytes   int
	maxMessageIDBytes int
	maxAddresses      int
	maxAttachments    int
	maxFilenameBytes  int
	maxResultBytes    int64
}

type bodyBudget struct {
	remaining int64
	truncated bool
}

type mimeBudget struct {
	body                 bodyBudget
	partsRemaining       int
	attachmentsRemaining int
	textBody             strings.Builder
	htmlBody             strings.Builder
	limits               mimeLimits
}

var headerWordDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
var headerAddressParser = &stdmail.AddressParser{WordDecoder: headerWordDecoder}

func parseMIMEMessage(raw []byte, maxBodyBytes int64) (parsedMessage, error) {
	return parseMIMEMessageWithLimits(raw, defaultMIMELimits(maxBodyBytes, maxBodyBytes+defaultMetadataResultBytes))
}

func defaultMIMELimits(maxBodyBytes, maxResultBytes int64) mimeLimits {
	if maxBodyBytes < 0 {
		maxBodyBytes = 0
	}
	if maxResultBytes < parsedMessageBaseBytes {
		maxResultBytes = parsedMessageBaseBytes
	}
	return mimeLimits{
		maxBodyBytes:      maxBodyBytes,
		maxSubjectBytes:   defaultMaxSubjectBytes,
		maxMessageIDBytes: defaultMaxMessageIDBytes,
		maxAddresses:      defaultMaxAddresses,
		maxAttachments:    defaultMaxAttachments,
		maxFilenameBytes:  defaultMaxFilenameBytes,
		maxResultBytes:    maxResultBytes,
	}
}

func limitTopLevelHeader(raw []byte, limit int) (io.Reader, bool) {
	if limit < 0 {
		limit = 0
	}
	headerEnd, separatorLength := topLevelHeaderEnd(raw)
	if headerEnd >= 0 && headerEnd <= limit {
		return bytes.NewReader(raw), false
	}
	if headerEnd < 0 && len(raw) <= limit {
		return bytes.NewReader(raw), false
	}

	prefixLimit := min(limit, len(raw))
	prefixEnd := bytes.LastIndexByte(raw[:prefixLimit], '\n') + 1
	readers := []io.Reader{
		bytes.NewReader(raw[:prefixEnd]),
		strings.NewReader("\r\n"),
	}
	if headerEnd >= 0 {
		bodyStart := headerEnd + separatorLength
		readers = append(readers, bytes.NewReader(raw[bodyStart:]))
	}
	return io.MultiReader(
		readers...,
	), true
}

func topLevelHeaderEnd(raw []byte) (int, int) {
	crlfIndex := bytes.Index(raw, []byte("\r\n\r\n"))
	lfIndex := bytes.Index(raw, []byte("\n\n"))
	if crlfIndex >= 0 && (lfIndex < 0 || crlfIndex < lfIndex) {
		return crlfIndex, 4
	}
	if lfIndex >= 0 {
		return lfIndex, 2
	}
	return -1, 0
}

func parseMIMEMessageWithLimits(raw []byte, limits mimeLimits) (parsedMessage, error) {
	return parseMIMEMessageWithOptions(raw, limits, false)
}

func parseMIMEMessageWithOptions(raw []byte, limits mimeLimits, allowPartial bool) (parsedMessage, error) {
	messageReader, topLevelHeaderTruncated := limitTopLevelHeader(raw, maxTopLevelHeaderBytes)
	message, err := stdmail.ReadMessage(messageReader)
	if err != nil {
		return parsedMessage{}, fmt.Errorf("read message: %w", err)
	}

	rawMessageID := message.Header.Get("Message-ID")
	messageID := ""
	messageIDTruncated := len(rawMessageID) > limits.maxMessageIDBytes
	if !messageIDTruncated {
		messageID, messageIDTruncated = truncateUTF8(strings.TrimSpace(rawMessageID), limits.maxMessageIDBytes)
	}
	encodedSubjectLimit := min(maxEncodedSubjectBytes, max(limits.maxSubjectBytes*2, limits.maxSubjectBytes))
	encodedSubject, encodedSubjectTruncated := truncateUTF8(message.Header.Get("Subject"), encodedSubjectLimit)
	subject, subjectTruncated := truncateUTF8(decodeHeaderWord(encodedSubject), limits.maxSubjectBytes)
	addressesRemaining := limits.maxAddresses
	addressSourceRemaining := maxAddressHeaderSourceBytes
	from, fromTruncated := parseMailAddressesLimited(message.Header, "From", &addressesRemaining, &addressSourceRemaining, maxFromAddresses)
	to, toTruncated := parseMailAddressesLimited(message.Header, "To", &addressesRemaining, &addressSourceRemaining, maxToAddresses)
	cc, ccTruncated := parseMailAddressesLimited(message.Header, "Cc", &addressesRemaining, &addressSourceRemaining, maxCCAddresses)
	parsed := parsedMessage{
		messageID:     messageID,
		from:          from,
		to:            to,
		cc:            cc,
		subject:       subject,
		bodyTruncated: topLevelHeaderTruncated || messageIDTruncated || encodedSubjectTruncated || subjectTruncated || fromTruncated || toTruncated || ccTruncated,
	}
	if value := strings.TrimSpace(message.Header.Get("Date")); value != "" {
		if date, dateErr := stdmail.ParseDate(value); dateErr == nil {
			parsed.headerDate = &date
		}
	}

	budget := &mimeBudget{
		body:                 bodyBudget{remaining: limits.maxBodyBytes},
		partsRemaining:       maxMIMEParts,
		attachmentsRemaining: max(limits.maxAttachments, 0),
		limits:               limits,
	}
	if budget.body.remaining < 0 {
		budget.body.remaining = 0
	}
	if err := walkMIME(textproto.MIMEHeader(message.Header), message.Body, &parsed, budget, int64(len(raw)), 0); err != nil {
		if !errors.Is(err, errMIMEBudgetExhausted) && !(allowPartial && isPartialMIMEError(err)) {
			return parsedMessage{}, fmt.Errorf("parse MIME body: %w", err)
		}
		parsed.bodyTruncated = true
	}
	parsed.textBody = strings.Clone(budget.textBody.String())
	parsed.htmlBody = strings.Clone(budget.htmlBody.String())
	parsed.bodyTruncated = parsed.bodyTruncated || budget.body.truncated
	trimParsedMessageToBytes(&parsed, limits.maxResultBytes)
	return parsed, nil
}

func walkMIME(header textproto.MIMEHeader, body io.Reader, parsed *parsedMessage, budget *mimeBudget, maxPartBytes int64, depth int) error {
	if depth > maxMIMEDepth || budget.partsRemaining == 0 {
		parsed.bodyTruncated = true
		return errMIMEBudgetExhausted
	}
	budget.partsRemaining--

	contentTypeHeader, contentTypeHeaderTruncated := truncateUTF8(header.Get("Content-Type"), maxMIMEParameterHeaderBytes)
	dispositionHeader, dispositionHeaderTruncated := truncateUTF8(header.Get("Content-Disposition"), maxMIMEParameterHeaderBytes)
	if contentTypeHeaderTruncated || dispositionHeaderTruncated {
		parsed.bodyTruncated = true
	}
	mediaType, params, invalidContentType := parseContentType(contentTypeHeader)
	if invalidContentType {
		parsed.bodyTruncated = true
	}
	if contentTypeHeaderTruncated {
		mediaType = "application/octet-stream"
		params = nil
	}
	decoded := decodeTransferEncoding(body, header.Get("Content-Transfer-Encoding"))
	disposition, dispositionParams, invalidDisposition := parseDisposition(dispositionHeader)
	if invalidDisposition {
		parsed.bodyTruncated = true
	}
	filename := dispositionParams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	filename, filenameTruncated := truncateUTF8(decodeHeaderWord(filename), budget.limits.maxFilenameBytes)
	if filenameTruncated {
		parsed.bodyTruncated = true
	}
	isAttachment := dispositionHeaderTruncated || invalidDisposition || strings.EqualFold(disposition, "attachment") || filename != ""
	if isAttachment {
		if budget.attachmentsRemaining == 0 {
			parsed.bodyTruncated = true
			return nil
		}
		size, err := countLimited(decoded, maxPartBytes)
		if err != nil {
			return err
		}
		contentType, contentTypeTruncated := truncateUTF8(mediaType, maxAttachmentContentTypeBytes)
		if contentTypeTruncated {
			contentType = "application/octet-stream"
			parsed.bodyTruncated = true
		}
		parsed.attachments = append(parsed.attachments, domain.Attachment{
			Filename:    filename,
			ContentType: contentType,
			Size:        size,
		})
		budget.attachmentsRemaining--
		return nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart content type has no boundary")
		}
		reader := multipart.NewReader(decoded, boundary)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			err = walkMIME(textproto.MIMEHeader(part.Header), part, parsed, budget, maxPartBytes, depth+1)
			_ = part.Close()
			if errors.Is(err, errMIMEBudgetExhausted) {
				return err
			}
			if err != nil && !isPartialMIMEError(err) {
				return err
			}
			if err != nil {
				return err
			}
		}
	}

	if mediaType != "text/plain" && mediaType != "text/html" {
		_, err := countLimited(decoded, maxPartBytes)
		return err
	}

	if label := strings.TrimSpace(params["charset"]); label != "" {
		converted, err := charset.NewReaderLabel(label, decoded)
		if err == nil {
			decoded = converted
		}
	}
	target := &budget.textBody
	if mediaType == "text/html" {
		target = &budget.htmlBody
	}
	separatorBytes := int64(0)
	if target.Len() > 0 {
		separatorBytes = 1
	}
	available := budget.body.remaining - separatorBytes
	if available < 0 {
		available = 0
	}
	content, truncated, err := readBodyPart(decoded, available)
	if err != nil {
		return err
	}
	budget.body.truncated = budget.body.truncated || truncated
	if len(content) == 0 {
		return nil
	}
	if separatorBytes > 0 {
		_ = target.WriteByte('\n')
	}
	_, _ = target.WriteString(content)
	budget.body.remaining -= separatorBytes + int64(len(content))
	return nil
}

func parseContentType(value string) (string, map[string]string, bool) {
	if strings.TrimSpace(value) == "" {
		return "text/plain", map[string]string{"charset": "us-ascii"}, false
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "application/octet-stream", nil, true
	}
	return strings.ToLower(mediaType), params, false
}

func parseDisposition(value string) (string, map[string]string, bool) {
	if strings.TrimSpace(value) == "" {
		return "", nil, false
	}
	disposition, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", nil, true
	}
	return strings.ToLower(disposition), params, false
}

func decodeTransferEncoding(reader io.Reader, value string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, reader)
	case "quoted-printable":
		return quotedprintable.NewReader(reader)
	default:
		return reader
	}
}

func readBodyPart(reader io.Reader, limit int64) (string, bool, error) {
	if limit < 0 {
		limit = 0
	}
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return "", false, err
	}
	truncated := false
	if int64(len(content)) > limit {
		content = content[:limit]
		truncated = true
	}
	result, adjusted := truncateUTF8(string(content), int(limit))
	return result, truncated || adjusted, nil
}

func countLimited(reader io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		limit = 0
	}
	count, err := io.Copy(io.Discard, io.LimitReader(reader, limit+1))
	if err != nil {
		return count, err
	}
	if count > limit {
		return limit, nil
	}
	return count, nil
}

func parseMailAddressesLimited(
	header stdmail.Header,
	field string,
	remaining *int,
	sourceRemaining *int,
	fieldLimit int,
) ([]domain.MailAddress, bool) {
	values := textproto.MIMEHeader(header).Values(field)
	result := make([]domain.MailAddress, 0)
	truncated := false
	fieldRemaining := max(fieldLimit, 0)
	for _, value := range values {
		if *remaining <= 0 || *sourceRemaining <= 0 || fieldRemaining <= 0 {
			truncated = true
			break
		}
		valueLimit := min(maxAddressHeaderValueBytes, *sourceRemaining)
		consumed := min(len(value), valueLimit)
		*sourceRemaining -= consumed
		if len(value) > valueLimit {
			truncated = true
			prefix := value[:valueLimit]
			separator := strings.LastIndexByte(prefix, ',')
			if separator < 0 {
				continue
			}
			value = prefix[:separator]
		}
		addresses, err := headerAddressParser.ParseList(value)
		if err != nil {
			truncated = true
			continue
		}
		for _, address := range addresses {
			if *remaining <= 0 || fieldRemaining <= 0 {
				truncated = true
				break
			}
			email := domain.NormalizeEmail(address.Address)
			if email == "" || len(email) > maxAddressEmailBytes {
				truncated = true
				continue
			}
			name, nameTruncated := truncateUTF8(decodeHeaderWord(address.Name), maxAddressNameBytes)
			truncated = truncated || nameTruncated
			result = append(result, domain.MailAddress{
				Name:  name,
				Email: strings.Clone(email),
			})
			(*remaining)--
			fieldRemaining--
		}
	}
	return result, truncated
}

func decodeHeaderWord(value string) string {
	decoded, err := headerWordDecoder.DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func truncateUTF8(value string, limit int) (string, bool) {
	if limit < 0 {
		limit = 0
	}
	changed := false
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "\uFFFD")
		changed = true
	}
	if len(value) > limit {
		end := limit
		for end > 0 && !utf8.ValidString(value[:end]) {
			end--
		}
		value = value[:end]
		changed = true
	}
	return strings.Clone(value), changed
}

func parsedMessageResultBytes(message parsedMessage) int64 {
	return parsedMessageBaseBytes + parsedMessageDynamicBytes(message)
}

func parsedMessageDynamicBytes(message parsedMessage) int64 {
	size := int64(len(message.messageID) + len(message.subject) + len(message.textBody) + len(message.htmlBody))
	for _, list := range [][]domain.MailAddress{message.from, message.to, message.cc} {
		for _, address := range list {
			size += mailAddressResultBytes + int64(len(address.Name)+len(address.Email))
		}
	}
	for _, attachment := range message.attachments {
		size += attachmentResultBytes + int64(len(attachment.Filename)+len(attachment.ContentType))
	}
	return size
}

func trimParsedMessageToBytes(message *parsedMessage, limit int64) {
	if limit < parsedMessageBaseBytes {
		limit = parsedMessageBaseBytes
	}
	if parsedMessageResultBytes(*message) <= limit {
		return
	}

	remaining := limit - parsedMessageBaseBytes
	original := *message
	message.messageID, remaining = takeResultString(original.messageID, remaining)
	message.subject, remaining = takeResultString(original.subject, remaining)
	message.from = takeResultAddresses(original.from, &remaining)
	message.to = takeResultAddresses(original.to, &remaining)
	message.cc = takeResultAddresses(original.cc, &remaining)
	message.attachments = takeResultAttachments(original.attachments, &remaining)
	message.textBody, message.htmlBody = takeResultBodies(original.textBody, original.htmlBody, remaining)
	message.bodyTruncated = true
}

func takeResultString(value string, remaining int64) (string, int64) {
	if remaining <= 0 || value == "" {
		return "", max(remaining, 0)
	}
	limit := min(int64(len(value)), remaining)
	result, _ := truncateUTF8(value, int(limit))
	return result, remaining - int64(len(result))
}

func takeResultAddresses(values []domain.MailAddress, remaining *int64) []domain.MailAddress {
	result := make([]domain.MailAddress, 0, min(len(values), 8))
	for _, value := range values {
		baseCost := int64(mailAddressResultBytes + len(value.Email))
		if *remaining < baseCost {
			break
		}
		*remaining -= baseCost
		name, left := takeResultString(value.Name, *remaining)
		*remaining = left
		result = append(result, domain.MailAddress{Name: name, Email: value.Email})
	}
	return result
}

func takeResultAttachments(values []domain.Attachment, remaining *int64) []domain.Attachment {
	result := make([]domain.Attachment, 0, min(len(values), 8))
	for _, value := range values {
		baseCost := int64(attachmentResultBytes + len(value.ContentType))
		if *remaining < baseCost {
			break
		}
		*remaining -= baseCost
		filename, left := takeResultString(value.Filename, *remaining)
		*remaining = left
		result = append(result, domain.Attachment{
			Filename:    filename,
			ContentType: value.ContentType,
			Size:        value.Size,
		})
	}
	return result
}

func takeResultBodies(textBody, htmlBody string, remaining int64) (string, string) {
	if remaining <= 0 {
		return "", ""
	}
	if int64(len(textBody)+len(htmlBody)) <= remaining {
		return textBody, htmlBody
	}
	if textBody == "" {
		html, _ := truncateUTF8(htmlBody, int(remaining))
		return "", html
	}
	if htmlBody == "" {
		text, _ := truncateUTF8(textBody, int(remaining))
		return text, ""
	}

	textBudget := remaining / 2
	htmlBudget := remaining - textBudget
	if int64(len(textBody)) < textBudget {
		htmlBudget += textBudget - int64(len(textBody))
		textBudget = int64(len(textBody))
	}
	if int64(len(htmlBody)) < htmlBudget {
		textBudget += htmlBudget - int64(len(htmlBody))
		htmlBudget = int64(len(htmlBody))
	}
	text, _ := truncateUTF8(textBody, int(textBudget))
	html, _ := truncateUTF8(htmlBody, int(htmlBudget))
	return text, html
}

func isPartialMIMEError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "nextpart: eof")
}
