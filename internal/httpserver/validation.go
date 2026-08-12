package httpserver

import (
	"errors"
	mailaddr "net/mail"
	"strings"

	"icloud-api/internal/domain"
)

func validateEmail(value string) error {
	parsed, err := mailaddr.ParseAddress(value)
	if err != nil || domain.NormalizeEmail(parsed.Address) != domain.NormalizeEmail(value) || !strings.Contains(value, "@") {
		return errors.New("邮箱地址格式不正确")
	}
	return nil
}
