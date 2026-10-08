// Package app assembles the application graph: storage, push sender, hub,
// services. Both the server and the admin CLI construct through here so they
// share the same business layer (docs/CLI.md).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gochathub/gochathub-server/internal/config"
	"github.com/gochathub/gochathub-server/internal/push"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/storage"
	"github.com/gochathub/gochathub-server/internal/store"
	"github.com/gochathub/gochathub-server/internal/ws"
)

// Assemble builds the app graph. Returns the service app and its hub.
func Assemble(st *store.Store, cfg *config.Config, log *slog.Logger) (*service.App, *ws.Hub, error) {
	ctx := context.Background()

	stor, err := storageFor(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	sender := &push.Sender{
		Store:         st,
		Log:           log,
		HTTP:          &http.Client{Timeout: 20 * time.Second},
		AllowHosts:    cfg.PushAllowHosts,
		Subscriber:    cfg.VAPIDSubscriber,
		NtfyQueryFlag: cfg.NtfyQueryFlag,
	}
	if _, err := sender.EnsureVAPID(ctx, cfg.VAPIDPrivateKey, cfg.VAPIDPublicKey); err != nil {
		return nil, nil, fmt.Errorf("vapid: %w", err)
	}

	svc := &service.App{
		Store:          st,
		Storage:        stor,
		AllowUploads:   cfg.AllowUploads && cfg.S3Endpoint != "",
		MaxUploadBytes: cfg.MaxUpload,
		BaseOrigin:     cfg.BaseOrigin,
		Sender:         sender,
	}
	WireServices(svc, log, cfg.SessionTTL)
	if cfg.TurnstileSecret != "" {
		svc.Auth.VerifyCaptcha = (&service.Turnstile{Secret: cfg.TurnstileSecret, Hostname: cfg.TurnstileHostname}).Verify
	}

	hub := ws.NewHub(log)
	hub.App = svc
	svc.Notify = hub
	sender.ActiveIn = hub.SubscribedToRoom
	return svc, hub, nil
}

// WireServices attaches the shared service graph. Both the server and the
// CLI degrade-path (cli.minimalApp) call it — one wiring, no drift
// (docs/CLI.md).
func WireServices(app *service.App, log *slog.Logger, sessionTTL time.Duration) {
	app.Log = log
	app.Users = service.UserService{App: app}
	app.Auth = &service.AuthService{App: app, SessionTTL: sessionTTL}
	app.Rooms = &service.RoomService{App: app}
	app.Messages = &service.MessageService{App: app}
	app.Contacts = &service.ContactService{App: app}
	app.Invites = &service.InviteService{App: app}
	app.Attachments = &service.AttachmentService{App: app}
	app.Devices = &service.DeviceService{App: app}
	app.Webhooks = &service.WebhookService{App: app}
}

func storageFor(ctx context.Context, cfg *config.Config) (service.Storage, error) {
	if cfg.S3Endpoint == "" {
		return storage.Disabled{}, nil
	}
	s3, err := storage.NewS3(ctx, cfg.S3Endpoint, cfg.S3PublicEndpoint, cfg.S3Region, cfg.S3Bucket, cfg.S3AccessKey, cfg.S3SecretKey, cfg.S3UseTLS)
	if err != nil {
		return nil, fmt.Errorf("attach storage: %w", err)
	}
	return s3, nil
}
