package httpserver

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

const (
	randomAliasMinLength = 8
	randomAliasMaxLength = 12
	// Keep one request bounded so credential generation and the no-store
	// response stay manageable. This is a batch-size limit, not an account
	// capacity limit: custom mailboxes may call the endpoint repeatedly.
	randomAliasMaxCount = 1000
	randomAliasAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomAliasAttempts = 8
)

// adminAPIRandomAliasRequest accepts a few count spellings used by management
// clients. The suffix always comes from account_mailbox_settings so every
// generated address remains inside the account's configured domain.
type adminAPIRandomAliasRequest struct {
	Count    int    `json:"count"`
	Quantity int    `json:"quantity"`
	Number   int    `json:"number"`
	Label    string `json:"label"`
}

type adminAPIRandomAliasResultDTO struct {
	Created []adminAPIAppleCreatedAliasDTO `json:"created"`
	// Aliases is a convenient flat projection for clients that do not need the
	// one-time envelope metadata. Created remains the canonical response.
	Aliases []adminAPIAliasDTO `json:"aliases"`
	Count   int                `json:"count"`
}

// randomAliasAddress creates one local-part using a cryptographically secure
// source. Length is uniformly selected from 8..12 and each character is
// uniformly selected from ASCII letters and digits.
func randomAliasAddress(suffix string) (string, error) {
	suffix, err := domain.NormalizeEmailSuffix(suffix)
	if err != nil {
		return "", err
	}
	length, err := secureRandomInt(randomAliasMinLength, randomAliasMaxLength)
	if err != nil {
		return "", fmt.Errorf("generate random alias length: %w", err)
	}
	local := make([]byte, length)
	for index := range local {
		position, err := secureRandomInt(0, len(randomAliasAlphabet)-1)
		if err != nil {
			return "", fmt.Errorf("generate random alias character: %w", err)
		}
		local[index] = randomAliasAlphabet[position]
	}
	return string(local) + "@" + suffix, nil
}

func secureRandomInt(minimum, maximum int) (int, error) {
	if minimum < 0 || maximum < minimum {
		return 0, errors.New("invalid random range")
	}
	rangeSize := big.NewInt(int64(maximum - minimum + 1))
	value, err := cryptorand.Int(cryptorand.Reader, rangeSize)
	if err != nil {
		return 0, err
	}
	return minimum + int(value.Int64()), nil
}

func (s *Server) adminAPICreateRandomAliases(basePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		accountID, ok := adminAPIParseID(c)
		if !ok {
			return
		}
		var input adminAPIRandomAliasRequest
		if !decodeAdminAPIJSON(c, &input) {
			return
		}
		count := input.Count
		if count == 0 {
			count = input.Quantity
		}
		if count == 0 {
			count = input.Number
		}
		if count < 1 || count > randomAliasMaxCount {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED",
				fmt.Sprintf("生成数量必须在 1 到 %d 之间", randomAliasMaxCount))
			return
		}
		label := strings.TrimSpace(input.Label)
		if utf8.RuneCountInString(label) > 100 {
			writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "用途备注不能超过 100 个字符")
			return
		}

		var created []adminAPIAppleCreatedAliasDTO
		var flatAliases []adminAPIAliasDTO
		var account domain.Account
		err := s.withAccountLock(c.Request.Context(), accountID, func() error {
			var err error
			account, err = s.store.GetAccount(c.Request.Context(), accountID)
			if err != nil {
				return err
			}
			if !account.Enabled {
				return store.ErrAccountDisabled
			}
			if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeCustom {
				return errRandomAliasRequiresCustomMailbox
			}
			suffix := account.EmailSuffix
			suffix, err = domain.NormalizeEmailSuffix(suffix)
			if err != nil {
				return errInvalidRandomAliasSuffix
			}

			// ImportAliasesWithCredentialsStrict performs one account-locked,
			// transactional insert and issues a complete v2 credential bundle
			// for every row. A collision is retried with a fresh batch.
			for attempt := 0; attempt < randomAliasAttempts; attempt++ {
				candidates := make([]domain.AliasImportCandidate, 0, count)
				seen := make(map[string]struct{}, count)
				for len(candidates) < count {
					address, generateErr := randomAliasAddress(suffix)
					if generateErr != nil {
						return generateErr
					}
					addressKey := domain.NormalizeEmail(address)
					if _, duplicate := seen[addressKey]; duplicate {
						continue
					}
					seen[addressKey] = struct{}{}
					candidates = append(candidates, domain.AliasImportCandidate{
						Address: address,
						Label:   label,
						Active:  true,
					})
				}
				result, issued, importErr := s.store.ImportCustomAliasesWithCredentialsStrict(c.Request.Context(), accountID, candidates)
				if errors.Is(importErr, store.ErrAliasOwnershipConflict) ||
					errors.Is(importErr, store.ErrAliasIdentityConflict) {
					continue
				}
				if importErr != nil {
					return importErr
				}
				if len(result.Created) != count || len(issued) != count {
					// A same-account collision is reported as Existing; retry so
					// callers always receive exactly the requested count.
					continue
				}
				issuedByAddress := make(map[string]string, len(issued))
				for _, item := range issued {
					issuedByAddress[domain.NormalizeEmail(item.Alias.Address)] = item.APIKey
				}
				created = make([]adminAPIAppleCreatedAliasDTO, 0, len(result.Created))
				flatAliases = make([]adminAPIAliasDTO, 0, len(result.Created))
				for _, alias := range result.Created {
					dto, dtoErr := s.adminAPIAliasFromDomain(alias)
					if dtoErr != nil {
						return dtoErr
					}
					apiKey := issuedByAddress[domain.NormalizeEmail(alias.Address)]
					if apiKey == "" {
						apiKey = dto.APIKey
					}
					created = append(created, adminAPIAppleCreatedAliasDTO{
						Alias:             dto,
						APIKey:            apiKey,
						MailAPIDirectLink: dto.DirectLinkPath,
					})
					flatAliases = append(flatAliases, dto)
				}
				return nil
			}
			return errRandomAliasCollision
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "主号不存在")
			case errors.Is(err, store.ErrAccountDisabled):
				writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_DISABLED", "主号已停用，请先启用主号后再生成邮箱")
			case errors.Is(err, errInvalidRandomAliasSuffix):
				writeAdminAPIError(c, http.StatusBadRequest, "VALIDATION_FAILED", "邮箱后缀格式不正确")
			case errors.Is(err, errRandomAliasRequiresCustomMailbox):
				writeAdminAPIError(c, http.StatusConflict, "CUSTOM_MAILBOX_REQUIRED", "随机邮箱仅适用于自定义邮箱主号")
			case errors.Is(err, store.ErrCustomMailboxRequired):
				writeAdminAPIError(c, http.StatusConflict, "CUSTOM_MAILBOX_REQUIRED", "随机邮箱仅适用于自定义邮箱主号")
			case errors.Is(err, store.ErrAliasSuffixMismatch):
				writeAdminAPIError(c, http.StatusConflict, "ACCOUNT_CHANGED", "主号邮箱后缀已发生变化，请刷新后重试")
			case errors.Is(err, errRandomAliasCollision), adminAPIUniqueConstraint(err):
				writeAdminAPIError(c, http.StatusConflict, "ALIAS_EXISTS", "生成的邮箱地址发生冲突，请重试")
			default:
				s.writeAdminAPIInternalError(c, err)
			}
			return
		}

		session := mustSession(c)
		s.audit(c, &session.AdminID, session.Username, "create_random", "alias", strconv.Itoa(len(created)), "success", "")
		c.Header("Cache-Control", "no-store")
		c.Header("Location", fmt.Sprintf("%s/accounts/%d/aliases", basePath, accountID))
		writeAdminAPIData(c, http.StatusCreated, adminAPIRandomAliasResultDTO{
			Created: created,
			Aliases: flatAliases,
			Count:   len(created),
		})
	}
}

// adminAPIDeleteCustomAlias handles locally generated aliases without asking
// Apple to delete an address that was never reserved in Hide My Email. It
// returns true when the alias belongs to a custom mailbox (including when an
// account lookup failed and an error response was already written).
func (s *Server) adminAPIDeleteCustomAlias(c *gin.Context, alias domain.Alias) bool {
	account, err := s.store.GetAccount(c.Request.Context(), alias.AccountID)
	if err != nil {
		s.writeAdminAPIStoreReadError(c, err)
		return true
	}
	if domain.NormalizeMailboxType(account.MailboxType) != domain.MailboxTypeCustom {
		return false
	}
	adminSession := mustSession(c)
	err = s.withAccountLock(c.Request.Context(), alias.AccountID, func() error {
		return s.store.DeleteAlias(c.Request.Context(), alias.ID)
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAdminAPIError(c, http.StatusNotFound, "NOT_FOUND", "隐私邮箱不存在")
		} else if errors.Is(err, store.ErrAliasConfirmationPending) {
			writeAdminAPIAliasConfirmationPending(c)
		} else {
			s.writeAdminAPIInternalError(c, err)
		}
		return true
	}
	s.audit(c, &adminSession.AdminID, adminSession.Username, "delete", "alias", strconv.FormatInt(alias.ID, 10), "success", "local")
	c.Status(http.StatusNoContent)
	return true
}

var (
	errInvalidRandomAliasSuffix         = errors.New("invalid random alias suffix")
	errRandomAliasRequiresCustomMailbox = errors.New("random aliases require a custom mailbox")
	errRandomAliasCollision             = errors.New("random alias collision")
)
