// openplayx-asr: standalone speech recognition service for OpenPlayX.
//
// One resident process; a single worker calls the whisper.cpp library
// in-process (opx_asr C ABI, shared with the App). It accepts only
// normalised audio uploaded by the App: no video URLs, NAS credentials or
// client paths, and it never starts ffmpeg/whisper-cli/whisper-server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/openplayx-asr/config.json", "config file")
	hashToken := flag.Bool("hash-token", false, "read a token on stdin and print its sha256 for tokens_file")
	showVersion := flag.Bool("version", false, "print version and engine, then exit")
	flag.Parse()
	if *showVersion {
		info := newNativeEngine().Info()
		fmt.Printf("openplayx-asr %s (%s %s, available=%v)\n", version, info.Engine, info.Version, info.Available)
		return
	}
	if *hashToken {
		printTokenHash()
		return
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := run(context.Background(), cfg, newNativeEngine()); err != nil {
		log.Fatal(err)
	}
}

func setup(cfg *Config, engine Engine) (*API, *Store, func(), error) {
	engineInfo = engine.Info()
	if !engineInfo.Available {
		return nil, nil, nil, errors.New("recognition engine unavailable in this build (build with -tags asr_native)")
	}
	tokens, err := loadTokens(cfg.TokensFile)
	if err != nil {
		return nil, nil, nil, err
	}
	models := map[string]*loadedModel{}
	closeAll := func() {
		for _, m := range models {
			m.model.Close()
		}
	}
	for _, mc := range cfg.Models {
		if err := verifyModel(mc); err != nil {
			closeAll()
			return nil, nil, nil, err
		}
		// Preloaded once; reused serially by the worker.
		m, err := engine.OpenModel(mc.Path)
		if err != nil {
			closeAll()
			return nil, nil, nil, fmt.Errorf("model %s: %w", mc.ID, err)
		}
		models[mc.ID] = &loadedModel{cfg: mc, model: m, info: m.Info()}
		log.Printf("model %s loaded (%s, multilingual=%v)", mc.ID, m.Info().Type, m.Info().Multilingual)
	}
	store, err := newStore(cfg.DataDir, cfg.Limits, models)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	tmp, err := uploadDir(cfg.DataDir)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	return &API{store: store, tokens: tokens, limits: cfg.Limits, tmpDir: tmp}, store, closeAll, nil
}

func run(ctx context.Context, cfg *Config, engine Engine) error {
	api, store, closeAll, err := setup(cfg, engine)
	if err != nil {
		return err
	}
	defer closeAll()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	worker := make(chan struct{})
	go func() {
		store.Run(ctx)
		close(worker)
	}()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("openplayx-asr %s (%s %s) listening on %s", version, engineInfo.Engine, engineInfo.Version, cfg.Listen)
	if cfg.TLSCert != "" {
		err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	<-worker // native work stops before models are released
	return err
}

func printTokenHash() {
	var token string
	if _, err := fmt.Fscanln(os.Stdin, &token); err != nil || token == "" {
		log.Fatal("no token on stdin")
	}
	fmt.Println(sha256Hex([]byte(token)))
}
