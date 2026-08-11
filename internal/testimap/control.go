package testimap

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed static/console.html
var consoleHTML []byte

type controlAPI struct {
	backend *Backend
	token   string
	now     func() time.Time
}

type createAccountRequest struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	ForwardAddress string `json:"forward_address"`
}

type setSeenRequest struct {
	Seen *bool `json:"seen"`
}

type resetUIDValidityRequest struct {
	ClearMessages *bool `json:"clear_messages"`
}

type presetRequest struct {
	AccountID int64  `json:"account_id"`
	Alias     string `json:"alias"`
	Count     int    `json:"count,omitempty"`
}

func newControlHandler(backend *Backend, token string) http.Handler {
	api := &controlAPI{backend: backend, token: token, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", api.console)
	mux.HandleFunc("GET /healthz", api.health)
	mux.Handle("GET /control/v1/accounts", api.authenticate(http.HandlerFunc(api.listAccounts)))
	mux.Handle("POST /control/v1/accounts", api.authenticate(http.HandlerFunc(api.createAccount)))
	mux.Handle("DELETE /control/v1/accounts/{accountID}", api.authenticate(http.HandlerFunc(api.deleteAccount)))
	mux.Handle("GET /control/v1/accounts/{accountID}/messages", api.authenticate(http.HandlerFunc(api.listMessages)))
	mux.Handle("POST /control/v1/accounts/{accountID}/messages", api.authenticate(http.HandlerFunc(api.createMessage)))
	mux.Handle("POST /control/v1/accounts/{accountID}/messages/raw", api.authenticate(http.HandlerFunc(api.createRawMessage)))
	mux.Handle("PATCH /control/v1/accounts/{accountID}/messages/{uid}", api.authenticate(http.HandlerFunc(api.setMessageSeen)))
	mux.Handle("DELETE /control/v1/accounts/{accountID}/messages/{uid}", api.authenticate(http.HandlerFunc(api.deleteMessage)))
	mux.Handle("POST /control/v1/accounts/{accountID}/uidvalidity/reset", api.authenticate(http.HandlerFunc(api.resetUIDValidity)))
	mux.Handle("POST /control/v1/presets/{name}", api.authenticate(http.HandlerFunc(api.createPreset)))
	mux.Handle("DELETE /control/v1/state", api.authenticate(http.HandlerFunc(api.resetState)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (api *controlAPI) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
		valid := found && strings.EqualFold(scheme, "Bearer") && token != "" &&
			subtle.ConstantTimeCompare([]byte(token), []byte(api.token)) == 1
		if !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeControlError(w, http.StatusUnauthorized, "UNAUTHORIZED", "控制令牌无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (api *controlAPI) console(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(consoleHTML)
}

func (api *controlAPI) health(w http.ResponseWriter, _ *http.Request) {
	writeControlData(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (api *controlAPI) listAccounts(w http.ResponseWriter, _ *http.Request) {
	writeControlData(w, http.StatusOK, map[string]any{"accounts": api.backend.ListAccounts()})
}

func (api *controlAPI) createAccount(w http.ResponseWriter, r *http.Request) {
	var input createAccountRequest
	if !decodeControlJSON(w, r, &input) {
		return
	}
	account, err := api.backend.CreateAccount(input.Username, input.Password, input.ForwardAddress)
	if err != nil {
		status, code := http.StatusBadRequest, "INVALID_ACCOUNT"
		if errors.Is(err, ErrAccountExists) {
			status, code = http.StatusConflict, "ACCOUNT_EXISTS"
		}
		writeControlError(w, status, code, err.Error())
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/control/v1/accounts/%d", account.ID))
	writeControlData(w, http.StatusCreated, map[string]any{"account": account})
}

func (api *controlAPI) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return
	}
	if err := api.backend.DeleteAccount(id); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *controlAPI) listMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return
	}
	messages, err := api.backend.ListStoredMessages(id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeControlData(w, http.StatusOK, map[string]any{"messages": messages})
}

func (api *controlAPI) createMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return
	}
	account, err := api.backend.GetAccount(id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	var input MessageInput
	if !decodeControlJSON(w, r, &input) {
		return
	}
	raw, err := RenderMessage(input, account.ForwardAddress, api.now())
	if err != nil {
		writeControlError(w, http.StatusBadRequest, "INVALID_MESSAGE", err.Error())
		return
	}
	internalDate, _ := input.InternalDate(api.now())
	created, err := api.backend.AddMessage(id, raw, internalDate, input.Seen)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeControlData(w, http.StatusCreated, map[string]any{"message": created})
}

func (api *controlAPI) createRawMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxControlMessageBytes+1)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeControlError(w, http.StatusRequestEntityTooLarge, "MESSAGE_TOO_LARGE", "原始邮件必须在 1 字节到 16 MiB 之间")
			return
		}
		writeControlError(w, http.StatusBadRequest, "INVALID_MESSAGE", "读取原始邮件失败")
		return
	}
	if len(raw) == 0 || len(raw) > maxControlMessageBytes {
		writeControlError(w, http.StatusRequestEntityTooLarge, "MESSAGE_TOO_LARGE", "原始邮件必须在 1 字节到 16 MiB 之间")
		return
	}
	seen, err := strconv.ParseBool(defaultString(r.URL.Query().Get("seen"), "false"))
	if err != nil {
		writeControlError(w, http.StatusBadRequest, "INVALID_MESSAGE", "seen 必须是布尔值")
		return
	}
	receivedAt := api.now().UTC()
	if value := strings.TrimSpace(r.URL.Query().Get("received_at")); value != "" {
		receivedAt, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeControlError(w, http.StatusBadRequest, "INVALID_MESSAGE", "received_at 必须是 RFC3339 时间")
			return
		}
	}
	created, err := api.backend.AddMessage(id, raw, receivedAt, seen)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeControlData(w, http.StatusCreated, map[string]any{"message": created})
}

func (api *controlAPI) setMessageSeen(w http.ResponseWriter, r *http.Request) {
	accountID, uid, ok := controlMessageIDs(w, r)
	if !ok {
		return
	}
	var input setSeenRequest
	if !decodeControlJSON(w, r, &input) {
		return
	}
	if input.Seen == nil {
		writeControlError(w, http.StatusBadRequest, "INVALID_MESSAGE", "seen 字段不能为空")
		return
	}
	if err := api.backend.SetMessageSeen(accountID, uint32(uid), *input.Seen); err != nil {
		writeBackendError(w, err)
		return
	}
	messages, _ := api.backend.ListStoredMessages(accountID)
	for _, message := range messages {
		if message.UID == uint32(uid) {
			writeControlData(w, http.StatusOK, map[string]any{"message": message})
			return
		}
	}
	writeControlError(w, http.StatusNotFound, "MESSAGE_NOT_FOUND", "邮件不存在")
}

func (api *controlAPI) deleteMessage(w http.ResponseWriter, r *http.Request) {
	accountID, uid, ok := controlMessageIDs(w, r)
	if !ok {
		return
	}
	if err := api.backend.DeleteMessage(accountID, uint32(uid)); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *controlAPI) resetUIDValidity(w http.ResponseWriter, r *http.Request) {
	id, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return
	}
	input := resetUIDValidityRequest{}
	if r.ContentLength != 0 && !decodeControlJSON(w, r, &input) {
		return
	}
	clearMessages := true
	if input.ClearMessages != nil {
		clearMessages = *input.ClearMessages
	}
	account, err := api.backend.ResetUIDValidity(id, clearMessages)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeControlData(w, http.StatusOK, map[string]any{"account": account})
}

func (api *controlAPI) createPreset(w http.ResponseWriter, r *http.Request) {
	var input presetRequest
	if !decodeControlJSON(w, r, &input) {
		return
	}
	account, err := api.backend.GetAccount(input.AccountID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	preset, defaultCount, err := PresetMessage(r.PathValue("name"), input.Alias, api.now())
	if err != nil {
		writeControlError(w, http.StatusNotFound, "PRESET_NOT_FOUND", err.Error())
		return
	}
	count := defaultCount
	if input.Count > 0 {
		count = input.Count
	}
	if count < 1 || count > 2000 {
		writeControlError(w, http.StatusBadRequest, "INVALID_PRESET", "count 必须在 1 到 2000 之间")
		return
	}
	created := make([]StoredMessage, 0, count)
	for index := 0; index < count; index++ {
		current := preset
		if count > 1 {
			current.Subject = fmt.Sprintf("%s %04d", preset.Subject, index+1)
			current.ReceivedAt = api.now().Add(time.Duration(index-count+1) * time.Second).UTC().Format(time.RFC3339)
		}
		raw, renderErr := RenderMessage(current, account.ForwardAddress, api.now())
		if renderErr != nil {
			writeControlError(w, http.StatusBadRequest, "INVALID_PRESET", renderErr.Error())
			return
		}
		internalDate, _ := current.InternalDate(api.now())
		message, addErr := api.backend.AddMessage(account.ID, raw, internalDate, current.Seen)
		if addErr != nil {
			writeBackendError(w, addErr)
			return
		}
		created = append(created, message)
	}
	writeControlData(w, http.StatusCreated, map[string]any{"messages": created, "count": len(created)})
}

func (api *controlAPI) resetState(w http.ResponseWriter, _ *http.Request) {
	api.backend.Reset()
	w.WriteHeader(http.StatusNoContent)
}

func controlMessageIDs(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	accountID, ok := controlID(w, r.PathValue("accountID"), "account")
	if !ok {
		return 0, 0, false
	}
	uid, ok := controlID(w, r.PathValue("uid"), "message UID")
	if !ok || uid > int64(^uint32(0)) {
		if ok {
			writeControlError(w, http.StatusBadRequest, "INVALID_ID", "message UID 超出范围")
		}
		return 0, 0, false
	}
	return accountID, uid, true
}

func controlID(w http.ResponseWriter, value, name string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		writeControlError(w, http.StatusBadRequest, "INVALID_ID", name+" ID 必须是正整数")
		return 0, false
	}
	return id, true
}

func decodeControlJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlMessageBytes+1)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeControlError(w, http.StatusBadRequest, "INVALID_JSON", "JSON 请求格式错误")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeControlError(w, http.StatusBadRequest, "INVALID_JSON", "JSON 请求只能包含一个对象")
		return false
	}
	return true
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAccountNotFound):
		writeControlError(w, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "测试账号不存在")
	case errors.Is(err, ErrMessageNotFound):
		writeControlError(w, http.StatusNotFound, "MESSAGE_NOT_FOUND", "邮件不存在")
	default:
		writeControlError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "测试服务处理失败")
	}
}

func writeControlData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeControlError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": code, "message": message,
	}})
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
