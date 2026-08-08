package mail

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/emersion/go-imap"

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
	session, err := settings.dial(ctx, address, host, settings.timeout)
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
			_ = session.Terminate()
		case <-stopCancellation:
		}
	}()
	defer func() {
		close(stopCancellation)
		<-cancellationStopped
		_ = session.Terminate()
	}()

	if err := session.Login(username, password); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("login IMAP account to mark messages seen: %w", err)
	}
	mailbox, err := session.Select("INBOX", false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("select INBOX read-write to mark messages seen: %w", err)
	}
	if mailbox == nil {
		return errors.New("select INBOX read-write to mark messages seen: empty mailbox status")
	}
	if mailbox.UidValidity == 0 {
		return errors.New("select INBOX read-write to mark messages seen: UIDVALIDITY is zero")
	}
	if mailbox.UidValidity != expectedUIDValidity {
		return &UIDValidityMismatchError{
			Expected: expectedUIDValidity,
			Actual:   mailbox.UidValidity,
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	set := new(imap.SeqSet)
	set.AddNum(uniqueUIDs...)
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	if err := session.UidStore(set, item, []interface{}{imap.SeenFlag}, nil); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("mark IMAP messages seen with UID STORE: %w", err)
	}
	return nil
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
