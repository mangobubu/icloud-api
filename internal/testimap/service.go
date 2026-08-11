package testimap

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	imapserver "github.com/emersion/go-imap/server"
)

type ServiceConfig struct {
	IMAPAddr     string
	ControlAddr  string
	ServerName   string
	CAFile       string
	ControlToken string
	Logger       *slog.Logger
}

type Service struct {
	config  ServiceConfig
	backend *Backend
	ready   chan ServiceEndpoints
}

type ServiceEndpoints struct {
	IMAPAddress    string
	ControlAddress string
	TLSServerName  string
	CAFile         string
}

func NewService(config ServiceConfig) (*Service, error) {
	config.IMAPAddr = strings.TrimSpace(config.IMAPAddr)
	config.ControlAddr = strings.TrimSpace(config.ControlAddr)
	config.ServerName = strings.TrimSpace(config.ServerName)
	config.CAFile = strings.TrimSpace(config.CAFile)
	config.ControlToken = strings.TrimSpace(config.ControlToken)
	if err := validateListenAddress("IMAP", config.IMAPAddr); err != nil {
		return nil, err
	}
	if err := validateListenAddress("control", config.ControlAddr); err != nil {
		return nil, err
	}
	if config.ServerName == "" || strings.ContainsAny(config.ServerName, " \t\r\n") {
		return nil, errors.New("test IMAP TLS server name must be one non-empty token")
	}
	if config.CAFile == "" {
		return nil, errors.New("test IMAP CA output file is required")
	}
	if len(config.ControlToken) < 16 || strings.ContainsAny(config.ControlToken, " \t\r\n") {
		return nil, errors.New("test IMAP control token must contain at least 16 non-whitespace characters")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	uidValidity, err := randomUIDValidity()
	if err != nil {
		return nil, fmt.Errorf("generate test IMAP UIDVALIDITY: %w", err)
	}
	return &Service{
		config:  config,
		backend: newBackendWithUIDValidity(uidValidity),
		ready:   make(chan ServiceEndpoints, 1),
	}, nil
}

func randomUIDValidity() (uint32, error) {
	var contents [4]byte
	if _, err := rand.Read(contents[:]); err != nil {
		return 0, err
	}
	value := binary.BigEndian.Uint32(contents[:])
	if value == 0 {
		value = 1
	}
	return value, nil
}

func validateListenAddress(name, address string) error {
	if address == "" {
		return fmt.Errorf("test %s listen address is required", name)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("test %s listen address %q: %w", name, address, err)
	}
	return nil
}

func (s *Service) Backend() *Backend {
	return s.backend
}

func (s *Service) Ready() <-chan ServiceEndpoints {
	return s.ready
}

type serveResult struct {
	name string
	err  error
}

func (s *Service) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tlsConfig, caPEM, err := GenerateTLSConfig(s.config.ServerName, time.Now())
	if err != nil {
		return fmt.Errorf("generate test IMAP TLS certificate: %w", err)
	}

	listenConfig := net.ListenConfig{}
	imapListener, err := listenConfig.Listen(ctx, "tcp", s.config.IMAPAddr)
	if err != nil {
		return fmt.Errorf("listen for test IMAP: %w", err)
	}
	controlListener, err := listenConfig.Listen(ctx, "tcp", s.config.ControlAddr)
	if err != nil {
		_ = imapListener.Close()
		return fmt.Errorf("listen for test IMAP control API: %w", err)
	}
	if err := PublishCA(s.config.CAFile, caPEM); err != nil {
		_ = controlListener.Close()
		_ = imapListener.Close()
		return fmt.Errorf("publish test IMAP CA: %w", err)
	}

	imapService := imapserver.New(s.backend)
	imapService.AllowInsecureAuth = false
	imapService.TLSConfig = tlsConfig
	imapService.MaxLiteralSize = uint32(maxControlMessageBytes)
	secureIMAPListener := tls.NewListener(imapListener, tlsConfig)
	controlService := &http.Server{
		Handler:           newControlHandler(s.backend, s.config.ControlToken),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	results := make(chan serveResult, 2)
	go func() {
		results <- serveResult{name: "IMAP", err: imapService.Serve(secureIMAPListener)}
	}()
	go func() {
		results <- serveResult{name: "control API", err: controlService.Serve(controlListener)}
	}()
	s.config.Logger.Info(
		"测试 IMAP 服务已启动",
		"imap_address", imapListener.Addr().String(),
		"control_address", controlListener.Addr().String(),
		"tls_server_name", s.config.ServerName,
		"ca_file", s.config.CAFile,
	)

	endpoints := ServiceEndpoints{
		IMAPAddress:    imapListener.Addr().String(),
		ControlAddress: controlListener.Addr().String(),
		TLSServerName:  s.config.ServerName,
		CAFile:         s.config.CAFile,
	}
	s.ready <- endpoints
	close(s.ready)

	var serveErr error
	completed := 0
	select {
	case <-ctx.Done():
	case result := <-results:
		completed++
		if !expectedServeClose(result.err) {
			serveErr = fmt.Errorf("test %s stopped: %w", result.name, result.err)
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := controlService.Shutdown(shutdownContext)
	closeErr := imapService.Close()
	_ = controlListener.Close()
	_ = secureIMAPListener.Close()

	for completed < 2 {
		select {
		case result := <-results:
			completed++
			if serveErr == nil && !expectedServeClose(result.err) {
				serveErr = fmt.Errorf("test %s stopped: %w", result.name, result.err)
			}
		case <-shutdownContext.Done():
			completed = 2
		}
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
		serveErr = errors.Join(serveErr, fmt.Errorf("shut down test control API: %w", shutdownErr))
	}
	if closeErr != nil && !expectedServeClose(closeErr) {
		serveErr = errors.Join(serveErr, fmt.Errorf("close test IMAP service: %w", closeErr))
	}
	if serveErr != nil {
		return serveErr
	}
	s.config.Logger.Info("测试 IMAP 服务已关闭")
	return nil
}

func expectedServeClose(err error) bool {
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed)
}
