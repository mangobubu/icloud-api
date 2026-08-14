package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"

	imapv2 "github.com/emersion/go-imap/v2"
	imapclientv2 "github.com/emersion/go-imap/v2/imapclient"

	"icloud-api/internal/domain"
)

// UIDValidityMismatchError means the queued UIDs belong to an older mailbox
// generation and must not be applied to the currently selected mailbox.
type UIDValidityMismatchError struct {
	Expected uint32
	Actual   uint32
}

func (e *UIDValidityMismatchError) Error() string {
	return fmt.Sprintf("IMAP UIDVALIDITY mismatch: expected %d, actual %d", e.Expected, e.Actual)
}

// MarkSeen marks the requested INBOX UIDs as read with one writable IMAP
// session and one silent UID STORE command.
func (f *Fetcher) MarkSeen(
	ctx context.Context,
	account domain.Account,
	password string,
	expectedUIDValidity uint32,
	uids []uint32,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uniqueUIDs, err := normalizeUIDs(uids)
	if err != nil {
		return err
	}
	if len(uniqueUIDs) == 0 {
		return nil
	}
	if expectedUIDValidity == 0 {
		return fmt.Errorf("%w: expected UIDVALIDITY is zero", ErrInvalidIMAPConfig)
	}
	if err := validateIMAPAccount(account, password); err != nil {
		return err
	}

	settings := f.settings()
	host, address, username, err := accountEndpoint(account)
	if err != nil {
		return err
	}
	client, err := dialSeenIMAP(ctx, address, host, settings)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("connect IMAP %s to mark messages seen: %w", address, err)
	}

	stopCancellation := make(chan struct{})
	cancellationStopped := make(chan struct{})
	go func() {
		defer close(cancellationStopped)
		select {
		case <-ctx.Done():
			_ = client.Close()
		case <-stopCancellation:
		}
	}()
	defer func() {
		close(stopCancellation)
		<-cancellationStopped
		_ = client.Close()
	}()

	if err := client.Login(username, password).Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("login IMAP account to mark messages seen: %w", err)
	}
	mailbox, err := client.Select("INBOX", &imapv2.SelectOptions{ReadOnly: false}).Wait()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("select INBOX read-write to mark messages seen: %w", err)
	}
	if mailbox == nil {
		return errors.New("select INBOX read-write to mark messages seen: empty mailbox status")
	}
	if mailbox.UIDValidity == 0 {
		return errors.New("select INBOX read-write to mark messages seen: UIDVALIDITY is zero")
	}
	if mailbox.UIDValidity != expectedUIDValidity {
		return &UIDValidityMismatchError{
			Expected: expectedUIDValidity,
			Actual:   mailbox.UIDValidity,
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	set := imapv2.UIDSet{}
	for _, uid := range uniqueUIDs {
		set.AddNum(imapv2.UID(uid))
	}
	flags := &imapv2.StoreFlags{
		Op:     imapv2.StoreFlagsAdd,
		Silent: true,
		Flags:  []imapv2.Flag{imapv2.FlagSeen},
	}
	if err := client.Store(set, flags, nil).Close(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("mark IMAP messages seen with UID STORE: %w", err)
	}
	return nil
}

// dialSeenIMAP keeps the write-side compatibility path independent from the
// archive fetcher's connection implementation.
func dialSeenIMAP(ctx context.Context, address, serverName string, settings fetchSettings) (*imapclientv2.Client, error) {
	dialer := &net.Dialer{Timeout: settings.timeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	tlsConnection := tls.Client(connection, tlsConfig)
	handshakeContext, cancel := context.WithTimeout(ctx, settings.timeout)
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeContext); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return imapclientv2.New(tlsConnection, &imapclientv2.Options{WordDecoder: headerWordDecoder}), nil
}

func normalizeUIDs(uids []uint32) ([]uint32, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	unique := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return nil, errors.New("mark IMAP messages seen: UID is zero")
		}
		unique[uid] = struct{}{}
	}
	result := make([]uint32, 0, len(unique))
	for uid := range unique {
		result = append(result, uid)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}
