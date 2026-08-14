package publicimap

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"icloud-api/internal/secure"
)

// Service owns the implicit-TLS listener and all public IMAP sessions.
// Listen is separate from Serve so startup can fail synchronously before the
// HTTP health endpoint begins accepting traffic.
type Service struct {
	address string
	server  *imapserver.Server
	tls     *tls.Config

	mu       sync.Mutex
	listener net.Listener
	started  bool
	closeErr error
	close    sync.Once
	ready    atomic.Bool
}

func NewService(repo Repository, cipher *secure.Cipher, tlsConfig *tls.Config, logger imapserver.Logger) (*Service, error) {
	if repo == nil {
		return nil, errors.New("IMAPS repository is required")
	}
	if cipher == nil {
		return nil, errors.New("IMAPS credential cipher is required")
	}
	if tlsConfig == nil || len(tlsConfig.Certificates) == 0 {
		return nil, errors.New("IMAPS TLS configuration is required")
	}
	service := &Service{tls: tlsConfig.Clone()}
	service.server = imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return NewSession(repo, cipher), nil, nil
		},
		Caps:   imap.CapSet{imap.CapIMAP4rev1: {}},
		Logger: logger,
	})
	return service, nil
}

func (s *Service) Listen(address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil || s.started {
		return errors.New("IMAPS service has already been started")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for IMAPS: %w", err)
	}
	s.listener = tls.NewListener(listener, s.tls)
	s.address = listener.Addr().String()
	s.ready.Store(true)
	return nil
}

func (s *Service) Serve() error {
	s.mu.Lock()
	if s.listener == nil {
		s.mu.Unlock()
		return errors.New("IMAPS listener is not initialized")
	}
	if s.started {
		s.mu.Unlock()
		return errors.New("IMAPS service has already been served")
	}
	s.started = true
	listener := s.listener
	s.mu.Unlock()

	err := s.server.Serve(listener)
	s.ready.Store(false)
	return err
}

func (s *Service) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.address
}

func (s *Service) Ready() bool { return s != nil && s.ready.Load() }

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.close.Do(func() {
		s.ready.Store(false)
		s.mu.Lock()
		listener := s.listener
		s.mu.Unlock()
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				s.closeErr = errors.Join(s.closeErr, err)
			}
		}
		if err := s.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.closeErr = errors.Join(s.closeErr, err)
		}
	})
	return s.closeErr
}
