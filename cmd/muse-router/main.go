// Command muse-router adalah AI router di atas Muse Code subscription:
// ia mengekspos endpoint OpenAI chat completions, OpenAI Responses, dan
// Anthropic Messages dengan satu kredensial Muse.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	deliveryhttp "muse-router/internal/delivery/http"
	"muse-router/internal/domain"
	"muse-router/internal/infrastructure/auth"
	"muse-router/internal/infrastructure/config"
	"muse-router/internal/infrastructure/muse"
	"muse-router/internal/usecase"
)

// version diisi saat build: -ldflags "-X main.version=x.y.z".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "muse-router:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "serve":
		return runServe()
	case "login":
		return runLogin()
	case "models":
		return runModels()
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q (see `muse-router help`)", command)
	}
}

func printHelp() {
	fmt.Println(`muse-router — AI router di atas Muse Code subscription.

Penggunaan:
  muse-router serve     Jalankan server HTTP (default bila tanpa argumen)
  muse-router login     Login device-code Meta, mint subscription key, simpan kredensial
  muse-router models    Tampilkan katalog model statis
  muse-router version   Tampilkan versi

Endpoint:
  GET  /healthz
  GET  /v1/models
  POST /v1/chat/completions
  POST /v1/responses
  GET  /v1/responses/{id}
  POST /v1/messages

Konfigurasi via environment (lihat .env.example):
  ROUTER_BIND, ROUTER_API_KEYS, MUSE_CREDENTIAL_JSON, MUSE_CREDENTIAL_FILE,
  MUSE_OAUTH_TOKEN, MUSE_API_KEY, MUSE_BASE_URL, MUSE_KEY_URL,
  MUSE_STORE_RESPONSES, MUSE_TIMEOUT_SECONDS`)
}

func runServe() error {
	cfg, err := config.Load(version)
	if err != nil {
		return err
	}
	creds := auth.NewCredentialStore(cfg)
	if !creds.HasSource() {
		return fmt.Errorf("no Muse credential: set MUSE_CREDENTIAL_JSON or run `muse-router login`")
	}
	client := muse.NewClient(cfg.MuseBaseURL, cfg.HeaderTimeout)
	catalog := muse.NewCatalog()
	services := deliveryhttp.Services{
		Chat:      usecase.NewChatService(client, creds, catalog, cfg.StoreResponses),
		Responses: usecase.NewResponsesService(client, creds, catalog, cfg.StoreResponses),
		Messages:  usecase.NewMessagesService(client, creds, catalog, cfg.StoreResponses),
		Models:    usecase.NewModelsService(client, creds, catalog),
	}
	routerAuth := auth.NewRouterAuth(cfg.BearerTokens)
	server := deliveryhttp.New(cfg, routerAuth, services)
	httpServer := &http.Server{
		Addr:              cfg.Bind,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("muse-router %s listening on http://%s (muse: %s)", version, cfg.Bind, cfg.MuseBaseURL)
	if !routerAuth.Required() {
		log.Printf("muse-router: WARNING: ROUTER_API_KEYS kosong; /v1/* tanpa autentikasi")
	}
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func runLogin() error {
	cfg, err := config.Load(version)
	if err != nil {
		return err
	}
	if cfg.CredentialFile == "" {
		return fmt.Errorf("cannot determine credential file: set MUSE_CREDENTIAL_FILE")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Println("Meminta otorisasi device...")
	device, err := muse.RequestDeviceAuth(ctx, cfg.DeviceAuthURL, cfg.DeviceClientID)
	if err != nil {
		return err
	}
	url := device.VerificationURIComplete
	if url == "" {
		url = device.VerificationURI
	}
	fmt.Printf("\nBuka: %s\nMasukkan kode: %s\n\nMenunggu otorisasi di browser...\n", url, device.UserCode)
	token, err := muse.PollDeviceToken(ctx, cfg.DeviceTokenURL, cfg.DeviceClientID, device.DeviceCode, device.Interval, device.ExpiresIn)
	if err != nil {
		return err
	}
	fmt.Println("Otorisasi diterima. Mint subscription API key...")
	payload, err := muse.RequestKey(ctx, cfg.MuseKeyURL, token, true)
	if err != nil {
		return err
	}
	apiKey, err := muse.ValidateKeyResponse(payload)
	if err != nil {
		return err
	}
	cred := domain.MuseCredential{OAuthAccessToken: token, APIKey: apiKey}
	if err := os.MkdirAll(filepath.Dir(cfg.CredentialFile), 0o700); err != nil {
		return fmt.Errorf("buat direktori kredensial: %w", err)
	}
	if err := os.WriteFile(cfg.CredentialFile, []byte(cred.Encode()+"\n"), 0o600); err != nil {
		return fmt.Errorf("simpan kredensial: %w", err)
	}
	fmt.Printf("\nLogin berhasil.\n")
	if payload.UserEmail != "" {
		fmt.Printf("  Akun : %s\n", payload.UserEmail)
	}
	if tier := payload.TierName(); tier != "" {
		fmt.Printf("  Tier  : %s\n", tier)
	}
	printUsage(payload)
	fmt.Printf("  File  : %s\n", cfg.CredentialFile)
	fmt.Println("\nJalankan `muse-router serve` untuk memulai router.")
	return nil
}

func printUsage(payload *domain.MuseKeyResponse) {
	if payload.SubsUsage == nil {
		return
	}
	if window := payload.SubsUsage.Window; window != nil && window.UsedPercent != nil {
		fmt.Printf("  Window: %.1f%% terpakai", *window.UsedPercent)
		if reset := window.ResetsAt.String(); reset != "" {
			fmt.Printf(" (reset %s)", reset)
		}
		fmt.Println()
	}
	if weekly := payload.SubsUsage.Weekly; weekly != nil && weekly.UsedPercent != nil {
		fmt.Printf("  Weekly: %.1f%% terpakai", *weekly.UsedPercent)
		if reset := weekly.ResetsAt.String(); reset != "" {
			fmt.Printf(" (reset %s)", reset)
		}
		fmt.Println()
	}
}

func runModels() error {
	catalog := muse.NewCatalog()
	for _, model := range catalog.List() {
		fmt.Printf("%s\t%s\tctx=%d max=%d\n", model.QualifiedID(), model.Name, model.ContextWindow, model.MaxTokens)
	}
	return nil
}
