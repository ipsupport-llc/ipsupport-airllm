// Command ipsupport-airllm is the gateway service entrypoint: it loads
// config, opens the stores, applies migrations, and serves HTTP.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ipsupport-llc/ipsupport-airllm/internal/auth"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/blob"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/capture"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/config"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/dlp"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/httpapi"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/limits"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/pricing"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/providers"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secondpass"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/secrets"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/seed"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/store"
	"github.com/ipsupport-llc/ipsupport-airllm/internal/webhook"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}

	if err := cfg.ResolveDevMasterKey(ctx, st.PutSettingIfAbsent); err != nil {
		return fmt.Errorf("resolve dev master key: %w", err)
	}

	if err := seed.EnsureBuiltinRoles(ctx, st); err != nil {
		return fmt.Errorf("ensure builtin roles: %w", err)
	}

	if err := seed.EnsureDLPDefaults(ctx, st.PG, os.Getenv("DLP_MODEL_URL_DEFAULT")); err != nil {
		return fmt.Errorf("ensure dlp defaults: %w", err)
	}

	// Wire the auth mode. AUTH_MODE=mock is a deprecated alias for local.
	var authImpl auth.Authenticator
	var loginImpl auth.LoginProvider
	var oidcImpl interface {
		LoginStart(http.ResponseWriter, *http.Request)
		Callback(http.ResponseWriter, *http.Request)
	}
	session := auth.NewSession(cfg.SessionKey)
	switch cfg.AuthMode {
	case "local":
		users := store.NewPGUsers(st)
		la := auth.NewLocalAuth(users, session)
		authImpl = la
		loginImpl = la
		if os.Getenv("AUTH_MODE") == "mock" {
			slog.Warn("AUTH_MODE=mock is a deprecated alias for local")
		}
		created, gen, err := auth.EnsureBootstrapAdmin(ctx, users,
			envOr("AIRLLM_ADMIN_USERNAME", "admin"), os.Getenv("AIRLLM_ADMIN_PASSWORD"))
		if err != nil {
			return fmt.Errorf("bootstrap admin: %w", err)
		}
		if err := reportBootstrapAdmin(bootstrapPasswordFile, envOr("AIRLLM_ADMIN_USERNAME", "admin"), created, gen); err != nil {
			return err
		}
	case "oidc":
		oa, err := auth.NewOIDCAuth(ctx, auth.OIDCConfig{
			Issuer:       cfg.OIDC.Issuer,
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			RedirectURL:  cfg.OIDC.RedirectURL,
			Scopes:       cfg.OIDC.Scopes,
			RolesClaim:   cfg.OIDC.RolesClaim,
			RoleMap:      cfg.OIDC.RoleMap,
		}, store.NewPGUsers(st), session)
		if err != nil {
			return fmt.Errorf("oidc init: %w", err)
		}
		authImpl = oa
		oidcImpl = oa
	}

	// Local-mode convenience: seed demo data + a fixed dev API key.
	if cfg.Env == "dev" && cfg.AuthMode == "local" {
		token, err := seed.Dev(ctx, st)
		if err != nil {
			return err
		}
		slog.Warn("dev mock seeded; using a fixed, non-secret API key", "token", token)
	}

	priceTable, err := pricing.Load(ctx, st)
	if err != nil {
		return err
	}

	if cfg.MasterKeyDev {
		slog.Warn("AIRLLM_MASTER_KEY not set; using a random per-install dev key persisted in the database (mock only, never for prod)")
	}
	sealer, err := secrets.New(cfg.MasterKey)
	if err != nil {
		return err
	}

	// Build the provider registry from the DB (decrypting stored credentials,
	// instantiating a client per kind). Reloaded when providers change.
	reg, err := providers.LoadFromStore(ctx, st, sealer)
	if err != nil {
		return err
	}

	// Build the capture pipeline. CAPTURE_BLOB_DIR env controls where blobs
	// land (default: ./capture-blobs for dev). Capture is off by default; the
	// pipeline is always wired so the config can be enabled at runtime.
	captureBlobDir := os.Getenv("CAPTURE_BLOB_DIR")
	if captureBlobDir == "" {
		captureBlobDir = "capture-blobs"
	}
	blobStore, err := blob.NewFS(captureBlobDir)
	if err != nil {
		return fmt.Errorf("capture blob store: %w", err)
	}
	pgIdx := &capture.PGInserter{PG: st.PG}

	// The pipeline's cfg function is a closure over apiSrvPtr, which is
	// stored after NewServer returns. The sweeper goroutine calls cfg() on a
	// ticker — use an atomic pointer so there is no data race between the
	// goroutine reading it and the main goroutine writing it.
	var apiSrvPtr atomic.Pointer[httpapi.Server]
	capturePipeline := capture.NewPipeline(blobStore, pgIdx, sealer, func() capture.Config {
		srv := apiSrvPtr.Load()
		if srv == nil {
			return capture.Config{Enabled: false}
		}
		return srv.CaptureCfg()
	})
	capturePipeline.Start(4)
	defer capturePipeline.Stop()

	deps := httpapi.Deps{
		Providers: reg,
		Limiter:   limits.New(st.RDB),
		Pricing:   priceTable,
		Sealer:    sealer,
		Auth:      authImpl,
		Login:     loginImpl,
		OIDC:      oidcImpl,
		Capture:   capturePipeline,
		Blob:      blobStore,
	}

	apiSrv := httpapi.NewServer(cfg, st, deps)
	apiSrvPtr.Store(apiSrv)
	apiSrv.Ledger().Start()
	defer apiSrv.Ledger().Stop()
	apiSrv.Metrics().RegisterCaptureDropped(func() float64 { return float64(capturePipeline.Dropped()) })
	apiSrv.Metrics().RegisterLedgerDropped(func() float64 { return float64(apiSrv.Ledger().Dropped()) })
	apiSrv.Metrics().RegisterWebhookDropped(func() float64 { return float64(webhook.Dropped()) })
	apiSrv.StartModelPool(ctx)
	apiSrv.StartConfigRefresh(ctx, cfg.ConfigRefreshInterval)

	// Build and start the second-pass background job. It uses an atomic
	// pointer for the config (same pattern as capturePipeline) so model /
	// enabled changes via PUT /api/admin/secondpass take effect on the next
	// RunOnce without a restart.
	spStoreAdapter := &secondpassStoreAdapter{idx: pgIdx}
	spBodyReader := func(blobCtx context.Context, blobKey string) ([]byte, error) {
		sealed, err := blobStore.Get(blobCtx, blobKey)
		if err != nil {
			return nil, err
		}
		return sealer.Open(sealed)
	}
	spChatFn := func(chatCtx context.Context, prompt string) (string, error) {
		srv := apiSrvPtr.Load()
		if srv == nil {
			return "", errors.New("secondpass: server not ready")
		}
		return srv.SecondpassChat(chatCtx, prompt)
	}
	spWebhookSender := secondpass.WebhookSender(func(hookCtx context.Context, event string, payload []byte) {
		eps, err := st.WebhooksForEvent(hookCtx, event)
		if err != nil || len(eps) == 0 {
			return
		}
		endpoints := make([]webhook.Endpoint, 0, len(eps))
		for _, e := range eps {
			secret := ""
			if len(e.SecretEnc) > 0 {
				if pt, err := sealer.Open(e.SecretEnc); err != nil {
					slog.Error("webhook secret decrypt failed; delivering unsigned", "err", err)
				} else {
					secret = string(pt)
				}
			}
			endpoints = append(endpoints, webhook.Endpoint{URL: e.URL, Secret: secret})
		}
		webhook.Send(endpoints, payload)
	})
	spMinScore := func() float64 {
		srv := apiSrvPtr.Load()
		if srv == nil {
			return 0.7
		}
		return srv.SecondpassCfg().MinScore
	}
	spAllowRaw := func() bool {
		srv := apiSrvPtr.Load()
		if srv == nil {
			return false
		}
		return srv.SecondpassCfg().AllowRaw
	}
	spEngine := &secondpass.LLMEngine{
		Chat:     spChatFn,
		MinScore: spMinScore,
	}
	spJob := secondpass.NewJob(spStoreAdapter, spBodyReader, spEngine, spWebhookSender, 50, spAllowRaw)

	spCfg := apiSrv.SecondpassCfg()
	spInterval := time.Duration(spCfg.IntervalSec) * time.Second
	spJob.Start(ctx, spInterval)
	defer spJob.Stop()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiSrv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", cfg.HTTPAddr)
	return serve(ctx, srv, ln, cfg.ShutdownDrain, cfg.ShutdownTimeout, apiSrv.SetDraining)
}

// serve runs srv on ln until ctx is cancelled, then shuts down in two steps.
// For drain it keeps accepting requests — a load balancer takes a moment to
// stop routing to a pod that is going away, and a connection refused in that
// window fails a request a healthy replica would have served. draining is
// called as the drain starts (readiness reports it). Then it stops accepting
// and gives in-flight requests, streams included, up to timeout to finish.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, drain, timeout time.Duration, draining func()) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down", "drain", drain, "timeout", timeout)
	draining()
	select {
	case err := <-errCh:
		return err
	case <-time.After(drain):
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// bootstrapPasswordFile is where a generated bootstrap-admin password is
// written when AIRLLM_ADMIN_PASSWORD isn't set, instead of being logged — a
// log line the app itself ships to centralized log storage (see the slog
// setup in run()) is a much larger, longer-lived blast radius than a local
// file the operator reads once via a shell into the container.
const bootstrapPasswordFile = "/tmp/airllm-bootstrap-admin-password"

// reportBootstrapAdmin hands a freshly generated bootstrap-admin password to
// the operator without ever passing it to slog. A no-op unless a new admin
// was just created with no AIRLLM_ADMIN_PASSWORD supplied.
func reportBootstrapAdmin(passwordFile, username string, created bool, generated string) error {
	if !created || generated == "" {
		return nil
	}
	if err := os.WriteFile(passwordFile, []byte(generated+"\n"), 0o600); err != nil {
		return fmt.Errorf("write bootstrap admin password: %w", err)
	}
	slog.Warn("bootstrap admin created; password written to a local file, never logged — read it once (e.g. kubectl exec/docker compose exec cat), then delete the file",
		"username", username, "path", passwordFile)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// secondpassStoreAdapter adapts capture.PGInserter to secondpass.Store.
type secondpassStoreAdapter struct {
	idx *capture.PGInserter
}

func (a *secondpassStoreAdapter) PendingForSecondPass(ctx context.Context, limit int) ([]secondpass.PendingRow, error) {
	rows, err := a.idx.PendingForSecondPass(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]secondpass.PendingRow, len(rows))
	for i, r := range rows {
		out[i] = secondpass.PendingRow{
			ID:           r.ID,
			BlobKey:      r.BlobKey,
			Detected:     r.Detected,
			Redacted:     r.Redacted,
			RawBlobKey:   r.RawBlobKey,
			RawExpiresAt: r.RawExpiresAt,
		}
	}
	return out, nil
}

func (a *secondpassStoreAdapter) UpdateSecondPass(ctx context.Context, id, status string, labels []dlp.Finding) error {
	return a.idx.UpdateSecondPass(ctx, id, status, labels)
}
