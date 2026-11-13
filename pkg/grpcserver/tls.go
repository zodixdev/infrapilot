package grpcserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zodixdev/infrapilot/pkg/internal/tlsreload"
)

// transportCredentials builds TLS (or mTLS with a client CA) from PEM files; plaintext when no certificate is
// configured (local development and tests; config validation requires TLS in staging and prod). The current
// certificate's expiry is exported as tls_certificate_expiry_timestamp_seconds{role="server"}.
func transportCredentials(cfg Config, logger *slog.Logger, mp metric.MeterProvider, name string) (credentials.TransportCredentials, error) {
	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		if cfg.TLSClientCAFile != "" {
			return nil, errors.New("grpcserver: TLSClientCAFile requires a server certificate")
		}
		return insecure.NewCredentials(), nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, errors.New("grpcserver: TLSCertFile and TLSKeyFile must be set together")
	}
	m, err := tlsreload.Load(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile, tlsreload.OnError(func(err error) {
		logger.Error("grpc server TLS material reload failed; keeping the previous certificate", slog.Any("error", err))
	}))
	if err != nil { // fail fast on unreadable material
		return nil, err
	}
	if err := registerExpiry(mp, m, "server", name); err != nil {
		return nil, err
	}
	base := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Resumed sessions reuse the chains verified at the original handshake; disabling tickets makes every
		// connection verify the client certificate against the CA bundle loaded now (CA rotation/removal).
		SessionTicketsDisabled: true,
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
		// Per handshake, return a config built from cached material that follows in-place rotation.
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := m.Current()
			c := base.Clone()
			c.Certificates = []tls.Certificate{*cert}
			if pool != nil {
				c.ClientCAs = pool
				c.ClientAuth = tls.RequireAndVerifyClientCert
			}
			return c, nil
		},
	}), nil
}

// registerExpiry exports the certificate expiry so an alert fires before a stuck rotation becomes an outage.
func registerExpiry(mp metric.MeterProvider, m *tlsreload.Material, role, name string) error {
	_, err := mp.Meter("github.com/zodixdev/infrapilot/pkg/grpcserver").Float64ObservableGauge(
		"tls_certificate_expiry_timestamp_seconds",
		metric.WithDescription("Expiry (Unix seconds) of the TLS certificate currently in use"), metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			if na := m.NotAfter(); !na.IsZero() {
				o.Observe(float64(na.Unix()), metric.WithAttributes(attribute.String("role", role), attribute.String("name", name)))
			}
			return nil
		}))
	if err != nil {
		return fmt.Errorf("grpcserver: create expiry gauge: %w", err)
	}
	return nil
}
