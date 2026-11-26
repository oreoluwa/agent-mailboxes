package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/oreoluwa/agent-mailboxes/internal/api"
	"github.com/oreoluwa/agent-mailboxes/internal/attachanalyze"
	"github.com/oreoluwa/agent-mailboxes/internal/attachmentstore"
	"github.com/oreoluwa/agent-mailboxes/internal/auth"
	"github.com/oreoluwa/agent-mailboxes/internal/cli"
	"github.com/oreoluwa/agent-mailboxes/internal/config"
	"github.com/oreoluwa/agent-mailboxes/internal/embed"
	"github.com/oreoluwa/agent-mailboxes/internal/envfile"
	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
	"github.com/oreoluwa/agent-mailboxes/internal/searchparse"
	"github.com/oreoluwa/agent-mailboxes/internal/service"
	"github.com/oreoluwa/agent-mailboxes/internal/smtpclient"
	"github.com/oreoluwa/agent-mailboxes/internal/store"
	"github.com/oreoluwa/agent-mailboxes/internal/summarize"
	emaildv1connect "github.com/oreoluwa/agent-mailboxes/proto/agentemails/emaild/v1/emaildv1connect"
)

func main() {
	if err := loadEnvAtStartup(); err != nil {
		fatal(err)
	}
	if len(os.Args) < 2 {
		usage(1)
	}
	if maybeHandleHelp(os.Args[1:]) {
		return
	}
	cmd := os.Args[1]
	switch cmd {
	case "migrate":
		runMigrate()
	case "encrypt-mailbox-credentials":
		runEncryptMailboxCredentials()
	case "doctor":
		runDoctor()
	case "deps":
		runDeps()
	case "serve":
		runServe()
	case "worker":
		runWorker()
	case "install":
		runInstall()
	case "uninstall":
		runUninstall()
	case "search-poc-eval":
		runSearchPOCEval()
	case "alias", "token", "status", "sync":
		runAdminAPI()
	default:
		usage(1)
	}
}

func loadEnvAtStartup() error {
	if p := strings.TrimSpace(os.Getenv("EMAILD_ENV_FILE")); p != "" {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("read explicit env file: %w", err)
		}
		if fi.IsDir() {
			return fmt.Errorf("env path is a directory: %s", p)
		}
		return envfile.Load(p)
	}
	for _, p := range envFileCandidates() {
		fi, err := os.Stat(p)
		if err == nil {
			if fi.IsDir() {
				return fmt.Errorf("env path is a directory: %s", p)
			}
			return envfile.Load(p)
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func envFileCandidates() []string {
	candidates := make([]string, 0, 2)
	if runtime.GOOS == "darwin" {
		if cfgDir, err := os.UserConfigDir(); err == nil {
			candidates = append(candidates, filepath.Join(cfgDir, "emaild", ".env"))
		}
	}
	candidates = append(candidates, ".env")
	return candidates
}

func mustConfig() config.Config {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	return cfg
}

func dbConnect(dsn string) *sql.DB {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fatal(err)
	}
	if err := db.Ping(); err != nil {
		fatal(err)
	}
	return db
}

func runMigrate() {
	cfg := mustConfig()
	ensureServeAttachmentDependencies(cfg)
	db := dbConnect(cfg.PostgresDSN)
	defer db.Close()
	if err := store.ApplyMigrations(context.Background(), db, filepath.Join("migrations")); err != nil {
		fatal(err)
	}
	fmt.Println("migrations applied")
}

func parseCredentialMigrationArgs(args []string) ([]string, error) {
	fs := flag.NewFlagSet("encrypt-mailbox-credentials", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var plaintextIDs []string
	fs.Func("plaintext-mailbox-id", "Treat this verified legacy mailbox credential as plaintext (repeatable)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("plaintext mailbox ID cannot be empty")
		}
		plaintextIDs = append(plaintextIDs, value)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("unexpected migration arguments")
	}
	return plaintextIDs, nil
}

func runEncryptMailboxCredentials() {
	plaintextIDs, err := parseCredentialMigrationArgs(os.Args[2:])
	if err != nil {
		fatal(err)
	}
	cfg := mustConfig()
	db := dbConnect(cfg.PostgresDSN)
	defer db.Close()
	if err := store.ApplyMigrations(context.Background(), db, filepath.Join("migrations")); err != nil {
		fatal(err)
	}
	st := store.NewPostgres(db)
	if err := configureMailboxCredentialEncryption(st, cfg); err != nil {
		fatal(err)
	}
	count, err := st.EncryptLegacyMailboxCredentialsWithPlaintextOverrides(context.Background(), plaintextIDs)
	if err != nil {
		fatal(fmt.Errorf("encrypt legacy mailbox credentials: %w", err))
	}
	fmt.Printf("encrypted %d legacy mailbox credential record(s)\n", count)
}

func configureMailboxCredentialEncryption(st *store.Postgres, cfg config.Config) error {
	if err := st.ConfigureCredentialEncryption(cfg.MailboxCredentialKey, cfg.MailboxCredentialOlds); err != nil {
		return fmt.Errorf("configure mailbox credential encryption: %w", err)
	}
	return nil
}

type dependencyIssue struct {
	Name        string
	Required    bool
	InstallHint string
	Detail      string
}

func runDoctor() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	issues := attachmentDependencyIssues(cfg)
	if len(issues) == 0 {
		fmt.Println("doctor: ok")
		return
	}
	fmt.Println("doctor: dependency issues found")
	for _, is := range issues {
		level := "WARN"
		if is.Required {
			level = "ERROR"
		}
		fmt.Printf("- [%s] %s: %s\n", level, is.Name, is.Detail)
		if strings.TrimSpace(is.InstallHint) != "" {
			fmt.Printf("  install: %s\n", is.InstallHint)
		}
	}
	for _, is := range issues {
		if is.Required {
			os.Exit(1)
		}
	}
}

func runDeps() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  emaild deps install")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "install":
		cfg, err := config.Load()
		if err != nil {
			fatal(err)
		}
		if err := installMissingAttachmentDependencies(cfg, false, true); err != nil {
			fatal(err)
		}
	default:
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  emaild deps install")
		os.Exit(1)
	}
}

func ensureServeAttachmentDependencies(cfg config.Config) {
	if err := installMissingAttachmentDependencies(cfg, cfg.AttachSummary.PromptInstallDeps, false); err != nil {
		fatal(err)
	}
}

func attachmentDependencyIssues(cfg config.Config) []dependencyIssue {
	out := []dependencyIssue{}
	if !cfg.SummaryEnabled || !cfg.AttachSummary.Enabled {
		return out
	}
	pdfTool := strings.TrimSpace(cfg.AttachSummary.PDFRenderToolBin)
	if pdfTool == "" {
		pdfTool = "pdftoppm"
	}
	if _, err := exec.LookPath(pdfTool); err != nil {
		out = append(out, dependencyIssue{
			Name:        pdfTool,
			Required:    cfg.AttachSummary.RequirePDFRenderer,
			InstallHint: pdfInstallHint(),
			Detail:      "required for PDF page-render fallback when PDFs have no extractable text",
		})
	}
	officeTool := strings.TrimSpace(cfg.AttachSummary.OfficeConverterBin)
	if officeTool == "" {
		officeTool = "soffice"
	}
	if _, err := exec.LookPath(officeTool); err != nil {
		out = append(out, dependencyIssue{
			Name:        officeTool,
			Required:    false,
			InstallHint: officeInstallHint(),
			Detail:      "optional; used for legacy Office docs (.doc/.xls/.ppt) conversion fallback",
		})
	}
	return out
}

func agentPlannerForConfig(cfg config.Config) service.AgentPlanner {
	if !cfg.SummaryEnabled {
		return nil
	}
	return service.NewOpenAICompatPlanner(cfg.OpenAIBaseURL, cfg.AgentModelName, cfg.OpenAIAPIKey)
}

func installMissingAttachmentDependencies(cfg config.Config, allowPrompt bool, autoInstall bool) error {
	issues := attachmentDependencyIssues(cfg)
	if len(issues) == 0 {
		return nil
	}
	for _, is := range issues {
		install := autoInstall
		if !install && allowPrompt && isInteractiveStdin() {
			question := fmt.Sprintf("Dependency %q is missing (%s). Install now? [y/N]: ", is.Name, is.Detail)
			ok, err := promptYesNo(question)
			if err != nil {
				return err
			}
			install = ok
		}
		if install {
			if err := installDependency(is.Name); err != nil {
				if is.Required {
					return fmt.Errorf("install required dependency %s: %w", is.Name, err)
				}
				log.Printf("optional dependency install failed %s: %v", is.Name, err)
			}
		}
		if _, err := exec.LookPath(is.Name); err != nil {
			if is.Required {
				return fmt.Errorf("missing required dependency %q; %s", is.Name, is.InstallHint)
			}
			log.Printf("optional dependency missing %q (%s)", is.Name, is.Detail)
		}
	}
	return nil
}

func installDependency(depName string) error {
	switch depName {
	case "pdftoppm":
		return installPDFRenderer()
	case "soffice":
		return installOfficeConverter()
	default:
		return fmt.Errorf("no installer mapped for dependency %q", depName)
	}
}

func installPDFRenderer() error {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("brew"); err != nil {
			return fmt.Errorf("homebrew not found; install poppler manually")
		}
		return runInstallCommand("brew", "install", "poppler")
	case "linux":
		return runInstallCommand("sh", "-c", `if command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y poppler-utils; elif command -v dnf >/dev/null 2>&1; then sudo dnf install -y poppler-utils; elif command -v yum >/dev/null 2>&1; then sudo yum install -y poppler-utils; else echo "No supported package manager found"; exit 1; fi`)
	default:
		return fmt.Errorf("unsupported OS for auto-install; install poppler manually")
	}
}

func installOfficeConverter() error {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("brew"); err != nil {
			return fmt.Errorf("homebrew not found; install libreoffice manually")
		}
		return runInstallCommand("brew", "install", "--cask", "libreoffice")
	case "linux":
		return runInstallCommand("sh", "-c", `if command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y libreoffice; elif command -v dnf >/dev/null 2>&1; then sudo dnf install -y libreoffice; elif command -v yum >/dev/null 2>&1; then sudo yum install -y libreoffice; else echo "No supported package manager found"; exit 1; fi`)
	default:
		return fmt.Errorf("unsupported OS for auto-install; install libreoffice manually")
	}
}

func runInstallCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func promptYesNo(prompt string) (bool, error) {
	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(os.Stdin)
	in, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func pdfInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install poppler"
	case "linux":
		return "install poppler-utils (apt/dnf/yum)"
	default:
		return "install poppler and ensure pdftoppm is on PATH"
	}
}

func officeInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install --cask libreoffice"
	case "linux":
		return "install libreoffice (apt/dnf/yum)"
	default:
		return "install libreoffice and ensure soffice is on PATH"
	}
}

func runServe() {
	fs := flag.NewFlagSet("emaild serve", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { serveUsage(fs.Output()) }
	serveMailbox := fs.String("mailbox", "", "Run only this mailbox name")
	serveAllMailboxes := fs.Bool("all-mailboxes", false, "Run pollers for all configured mailboxes")
	httpOnly := fs.Bool("http-only", false, "Run API server only (disable mailbox pollers/workers)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err)
	}
	runRuntime(runtimeOptions{
		mailbox:      strings.TrimSpace(*serveMailbox),
		allMailboxes: *serveAllMailboxes,
		withHTTP:     true,
		withPollers:  !*httpOnly,
	})
}

func runWorker() {
	fs := flag.NewFlagSet("emaild worker", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { workerUsage(fs.Output()) }
	serveMailbox := fs.String("mailbox", "", "Run only this mailbox name")
	serveAllMailboxes := fs.Bool("all-mailboxes", false, "Run workers for all configured mailboxes")
	shardIndex := fs.Int("shard-index", -1, "Shard index (0-based); requires --shard-count and --all-mailboxes")
	shardCount := fs.Int("shard-count", 0, "Total shard count; requires --shard-index and --all-mailboxes")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err)
	}
	if (*shardIndex >= 0 && *shardCount == 0) || (*shardIndex < 0 && *shardCount > 0) {
		fatal(fmt.Errorf("--shard-index and --shard-count must be set together"))
	}
	if *shardCount < 0 {
		fatal(fmt.Errorf("--shard-count must be >= 0"))
	}
	if *shardIndex >= 0 {
		if !*serveAllMailboxes {
			fatal(fmt.Errorf("shard mode requires --all-mailboxes"))
		}
		if *shardCount <= 0 {
			fatal(fmt.Errorf("--shard-count must be > 0 when shard mode is enabled"))
		}
		if *shardIndex >= *shardCount {
			fatal(fmt.Errorf("--shard-index must be in range [0,%d)", *shardCount))
		}
	}
	runRuntime(runtimeOptions{
		mailbox:      strings.TrimSpace(*serveMailbox),
		allMailboxes: *serveAllMailboxes,
		withHTTP:     false,
		withPollers:  true,
		shardIndex:   *shardIndex,
		shardCount:   *shardCount,
	})
}

type runtimeOptions struct {
	mailbox      string
	allMailboxes bool
	withHTTP     bool
	withPollers  bool
	shardIndex   int
	shardCount   int
}

type runningMailbox struct {
	name string
	id   string
}

func runRuntime(opts runtimeOptions) {
	if opts.allMailboxes && opts.mailbox != "" {
		fatal(fmt.Errorf("cannot use --mailbox with --all-mailboxes"))
	}

	cfg := mustConfig()
	db := dbConnect(cfg.PostgresDSN)
	defer db.Close()
	if err := store.ApplyMigrations(context.Background(), db, filepath.Join("migrations")); err != nil {
		fatal(err)
	}

	st := store.NewPostgres(db)
	if err := configureMailboxCredentialEncryption(st, cfg); err != nil {
		fatal(err)
	}
	selectedMailboxes, setupOnly, err := resolveMailboxes(context.Background(), st, opts.mailbox, opts.allMailboxes)
	if err != nil {
		fatal(err)
	}
	if opts.shardCount > 0 && opts.shardIndex >= 0 {
		selectedMailboxes = filterMailboxesForShard(selectedMailboxes, opts.shardIndex, opts.shardCount)
	}
	if rejectsEmptyRuntime(setupOnly, opts) {
		fatal(fmt.Errorf("no mailbox configured; worker mode requires at least one enabled mailbox"))
	}

	var attachmentStore *attachmentstore.S3Store
	if strings.TrimSpace(cfg.AttachS3.Endpoint) != "" {
		attachmentStore, err = attachmentstore.NewS3(attachmentstore.S3Config{
			Endpoint:           cfg.AttachS3.Endpoint,
			Region:             cfg.AttachS3.Region,
			Bucket:             cfg.AttachS3.Bucket,
			AccessKey:          cfg.AttachS3.AccessKey,
			SecretKey:          cfg.AttachS3.SecretKey,
			Prefix:             cfg.AttachS3.Prefix,
			PathStyle:          cfg.AttachS3.PathStyle,
			InsecureSkipVerify: cfg.AttachS3.InsecureSkipVerify,
		})
		if err != nil {
			fatal(fmt.Errorf("init attachment s3 store: %w", err))
		}
		log.Printf("attachment storage enabled bucket=%s endpoint=%s", cfg.AttachS3.Bucket, cfg.AttachS3.Endpoint)
	}

	servicesByName := map[string]*service.Service{}
	servicesByMailboxID := map[string]*service.Service{}
	var defaultSvc *service.Service
	broker := api.NewEventBroker()
	var attachmentAnalyzer attachanalyze.Analyzer
	if cfg.SummaryEnabled && cfg.AttachSummary.Enabled {
		attachmentAnalyzer = attachanalyze.NewOpenAICompat(
			cfg.OpenAIBaseURL,
			cfg.VisionModelName,
			cfg.OpenAIAPIKey,
			attachanalyze.Config{
				Enabled:          cfg.AttachSummary.Enabled,
				MaxFiles:         cfg.AttachSummary.MaxFiles,
				MaxFileBytes:     cfg.AttachSummary.MaxFileBytes,
				MaxImageBytes:    cfg.AttachSummary.VisionMaxImageBytes,
				ProviderMode:     cfg.AttachSummary.VisionProviderMode,
				MaxRetries:       cfg.AttachSummary.VisionMaxRetries,
				VisionTimeout:    time.Duration(cfg.AttachSummary.VisionTimeoutSec) * time.Second,
				PDFRenderToolBin: cfg.AttachSummary.PDFRenderToolBin,
				OfficeToolBin:    cfg.AttachSummary.OfficeConverterBin,
			},
		)
		log.Printf("attachment summarization enabled max_files=%d max_file_bytes=%d vision_model=%q provider_mode=%s vision_retries=%d vision_timeout_sec=%d", cfg.AttachSummary.MaxFiles, cfg.AttachSummary.MaxFileBytes, cfg.VisionModelName, cfg.AttachSummary.VisionProviderMode, cfg.AttachSummary.VisionMaxRetries, cfg.AttachSummary.VisionTimeoutSec)
	}
	buildService := func(mailbox store.Mailbox) *service.Service {
		var emb embed.Embedder
		if cfg.SummaryEnabled && cfg.EmbeddingModel != "" {
			emb = embed.NewOpenAICompat(cfg.OpenAIBaseURL, cfg.EmbeddingModel, cfg.OpenAIAPIKey)
		}
		var parser searchparse.Parser
		if cfg.SummaryEnabled && cfg.SearchParserModel != "" {
			parser = searchparse.NewOpenAICompat(cfg.OpenAIBaseURL, cfg.SearchParserModel, cfg.OpenAIAPIKey, cfg.SearchParserTimeout)
		}
		var sum summarize.Summarizer
		if cfg.SummaryEnabled {
			configured := summarize.NewOpenAICompat(cfg.OpenAIBaseURL, cfg.ModelName, cfg.OpenAIAPIKey)
			if cfg.SummaryTimeout > 0 {
				configured.Client.Timeout = cfg.SummaryTimeout
			}
			sum = configured
		}
		return &service.Service{
			Store:                 st,
			Summarizer:            sum,
			Embedder:              emb,
			AgentPlanner:          agentPlannerForConfig(cfg),
			SearchParser:          parser,
			SearchDefaultTimezone: cfg.SearchDefaultTZ,
			AttachmentAnalyzer:    attachmentAnalyzer,
			Fetcher: imapingest.IMAPFetcher{
				Host:                   mailbox.Creds.IMAPHost,
				Port:                   mailbox.Creds.IMAPPort,
				TLS:                    mailbox.Creds.IMAPTLS,
				Username:               mailbox.Creds.Username,
				Password:               mailbox.Creds.Password,
				Folder:                 "INBOX",
				MaxFetch:               cfg.FetchMaxMessages,
				InitialSyncMaxMessages: &cfg.InitialSyncMaxMessages,
			},
			Sender: smtpclient.Client{
				Host:     mailbox.Creds.SMTPHost,
				Port:     mailbox.Creds.SMTPPort,
				TLS:      mailbox.Creds.SMTPTLS,
				Username: mailbox.Creds.Username,
				Password: mailbox.Creds.Password,
			},
			Attachments:            attachmentStore,
			MailboxID:              mailbox.ID,
			MailboxName:            mailbox.Name,
			SyncFolders:            cfg.SyncFolders,
			CatchallAdmin:          cfg.CatchallAdmin,
			ThreadContextMode:      cfg.ThreadContext.Mode,
			ThreadContextMaxChars:  cfg.ThreadContext.MaxChars,
			SummarizeWorkers:       cfg.SummarizeWorkers,
			EmbeddingBatchSize:     cfg.EmbeddingBatch,
			EmbeddingFlushInterval: cfg.EmbeddingFlush,
			Events:                 broker,
		}
	}
	if !setupOnly {
		for _, mailbox := range selectedMailboxes {
			svc := buildService(mailbox)
			if defaultSvc == nil {
				defaultSvc = svc
			}
			servicesByName[mailbox.Name] = svc
			servicesByMailboxID[mailbox.ID] = svc
		}
	}
	allowedMailboxNames := map[string]struct{}{}
	for _, mailbox := range selectedMailboxes {
		allowedMailboxNames[mailbox.Name] = struct{}{}
	}

	server := &api.Server{
		Svc:                       defaultSvc,
		ServicesByName:            servicesByName,
		ServicesByMailboxID:       servicesByMailboxID,
		Store:                     st,
		Broker:                    broker,
		MasterKey:                 cfg.MasterKey,
		MasterKeys:                cfg.MasterKeysForVerify(),
		ActorEnvelopeSecret:       cfg.ActorEnvelopeSecret,
		ActorEnvelopeReplayWindow: cfg.ActorReplayWindow,
		AllowInboxAll:             cfg.InboxAllEnabled,
	}
	if enablesLazyMailboxLoading(opts) {
		server.LoadMailboxService = func(loadCtx context.Context, mailboxID, mailboxName string) (*service.Service, error) {
			mailboxes, err := st.ListMailboxes(loadCtx)
			if err != nil {
				return nil, err
			}
			for _, mailbox := range mailboxes {
				if !mailbox.Enabled {
					continue
				}
				if mailboxID != "" && mailbox.ID != mailboxID {
					continue
				}
				if mailboxName != "" && mailbox.Name != mailboxName {
					continue
				}
				return buildService(mailbox), nil
			}
			return nil, store.ErrNotFound
		}
	}
	var httpSrv *http.Server
	if opts.withHTTP {
		httpSrv = &http.Server{Addr: cfg.ListenAddr, Handler: server.Handler()}
	}
	var connectSrv *http.Server
	if opts.withHTTP && cfg.ConnectEnabled {
		connectMux := http.NewServeMux()
		path, handler := emaildv1connect.NewEmailServiceHandler(server.ConnectHandler())
		connectMux.Handle(path, handler)
		connectSrv = &http.Server{Addr: cfg.ConnectListenAddr, Handler: connectMux}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	startConfiguredQueueWorkers(opts.withPollers, servicesByName, func(svc *service.Service) {
		svc.StartQueueWorkers(ctx)
	})
	pollerCancels := map[string]context.CancelFunc{}
	runningByMailboxID := map[string]runningMailbox{}
	runningByName := map[string]runningMailbox{}
	var pollerMu sync.Mutex
	startPoller := func(name string, svc *service.Service) bool {
		if svc == nil {
			return false
		}
		pollerMu.Lock()
		if _, exists := pollerCancels[svc.MailboxID]; exists {
			pollerMu.Unlock()
			return false
		}
		pollerCtx, pollerCancel := context.WithCancel(ctx)
		pollerCancels[svc.MailboxID] = pollerCancel
		pollerMu.Unlock()
		go func() {
			log.Printf("starting poller mailbox=%s", name)
			svc.RunPoller(pollerCtx, cfg.PollInterval)
		}()
		return true
	}
	stopPoller := func(mailboxID string) bool {
		pollerMu.Lock()
		cancelFn, exists := pollerCancels[mailboxID]
		if exists {
			delete(pollerCancels, mailboxID)
		}
		pollerMu.Unlock()
		if exists {
			cancelFn()
		}
		return exists
	}
	registerRunning := func(name, mailboxID string) {
		pollerMu.Lock()
		defer pollerMu.Unlock()
		rm := runningMailbox{name: name, id: mailboxID}
		runningByMailboxID[mailboxID] = rm
		runningByName[name] = rm
	}
	unregisterRunning := func(name, mailboxID string) {
		pollerMu.Lock()
		defer pollerMu.Unlock()
		delete(runningByMailboxID, mailboxID)
		delete(runningByName, name)
	}
	snapshotRunningByName := func() map[string]runningMailbox {
		pollerMu.Lock()
		defer pollerMu.Unlock()
		out := make(map[string]runningMailbox, len(runningByName))
		for k, v := range runningByName {
			out[k] = v
		}
		return out
	}
	server.OnMailboxCreated = func(reqCtx context.Context, mailbox store.Mailbox) (bool, error) {
		if !opts.allMailboxes {
			return false, nil
		}
		return activateMailboxService(mailbox, opts.withPollers, buildService, server.RegisterMailboxService,
			func(svc *service.Service) { svc.StartQueueWorkers(ctx) }, startPoller, registerRunning), nil
	}
	server.OnMailboxEnabled = func(reqCtx context.Context, name string, enabled bool) (bool, error) {
		mailbox, err := st.GetMailboxByName(reqCtx, name)
		if err != nil {
			return false, err
		}
		if enabled {
			if !opts.allMailboxes {
				if _, allowed := allowedMailboxNames[mailbox.Name]; !allowed {
					return false, nil
				}
			}
			return activateMailboxService(mailbox, opts.withPollers, buildService, server.RegisterMailboxService,
				func(svc *service.Service) { svc.StartQueueWorkers(ctx) }, startPoller, registerRunning), nil
		}
		stopped := stopPoller(mailbox.ID)
		removed := server.UnregisterMailboxService(mailbox.Name, mailbox.ID)
		unregisterRunning(mailbox.Name, mailbox.ID)
		return stopped || removed, nil
	}
	if opts.withPollers {
		if len(servicesByName) > 0 {
			for mailboxName, svc := range servicesByName {
				_ = startPoller(mailboxName, svc)
				registerRunning(mailboxName, svc.MailboxID)
			}
		} else if opts.allMailboxes && opts.withHTTP {
			log.Printf("no enabled mailbox configured. open http://%s/onboarding and add one; emaild will start polling it automatically", cfg.ListenAddr)
		}
	}

	if opts.withPollers && opts.allMailboxes && !opts.withHTTP {
		interval := workerReconcileInterval()
		log.Printf("worker reconcile enabled interval=%s", interval)
		if opts.shardCount > 0 && opts.shardIndex >= 0 {
			log.Printf("worker shard mode enabled index=%d/%d", opts.shardIndex, opts.shardCount)
		}
		go runWorkerMailboxReconciler(ctx, st, buildService, server, startPoller, stopPoller, registerRunning, unregisterRunning, snapshotRunningByName, interval, opts.shardIndex, opts.shardCount)
	}

	if opts.withHTTP {
		go func() {
			<-ctx.Done()
			_ = httpSrv.Shutdown(context.Background())
			if connectSrv != nil {
				_ = connectSrv.Shutdown(context.Background())
			}
		}()
		if connectSrv != nil {
			go func() {
				log.Printf("emaild connect listening on %s", cfg.ConnectListenAddr)
				if err := connectSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fatal(err)
				}
			}()
		}
		log.Printf("emaild listening on %s", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
		return
	}

	log.Printf("emaild worker running (mailboxes=%d, poll_interval=%s)", len(servicesByName), cfg.PollInterval)
	<-ctx.Done()
}

func rejectsEmptyRuntime(setupOnly bool, opts runtimeOptions) bool {
	return setupOnly && !opts.withHTTP && !opts.allMailboxes
}

func enablesLazyMailboxLoading(opts runtimeOptions) bool {
	return opts.withHTTP && !opts.withPollers && opts.allMailboxes
}

func activateMailboxService(
	mailbox store.Mailbox,
	withWorkers bool,
	buildService func(store.Mailbox) *service.Service,
	registerService func(string, *service.Service) bool,
	startQueueWorkers func(*service.Service),
	startPoller func(string, *service.Service) bool,
	registerRunning func(string, string),
) bool {
	svc := buildService(mailbox)
	if svc == nil || !registerService(mailbox.Name, svc) {
		return false
	}
	if !withWorkers {
		return false
	}
	startQueueWorkers(svc)
	started := startPoller(mailbox.Name, svc)
	registerRunning(mailbox.Name, mailbox.ID)
	return started
}

func startConfiguredQueueWorkers(enabled bool, services map[string]*service.Service, start func(*service.Service)) {
	if !enabled {
		return
	}
	for _, svc := range services {
		start(svc)
	}
}

func runWorkerMailboxReconciler(
	ctx context.Context,
	st *store.Postgres,
	buildService func(store.Mailbox) *service.Service,
	server *api.Server,
	startPoller func(string, *service.Service) bool,
	stopPoller func(string) bool,
	registerRunning func(string, string),
	unregisterRunning func(string, string),
	snapshotRunningByName func() map[string]runningMailbox,
	interval time.Duration,
	shardIndex int,
	shardCount int,
) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconcileWorkerMailboxes(ctx, st, buildService, server, startPoller, stopPoller, registerRunning, unregisterRunning, snapshotRunningByName, shardIndex, shardCount); err != nil {
				log.Printf("worker reconcile failed: %v", err)
			}
		}
	}
}

func reconcileWorkerMailboxes(
	ctx context.Context,
	st *store.Postgres,
	buildService func(store.Mailbox) *service.Service,
	server *api.Server,
	startPoller func(string, *service.Service) bool,
	stopPoller func(string) bool,
	registerRunning func(string, string),
	unregisterRunning func(string, string),
	snapshotRunningByName func() map[string]runningMailbox,
	shardIndex int,
	shardCount int,
) error {
	all, err := st.ListMailboxes(ctx)
	if err != nil {
		return err
	}
	enabled := map[string]store.Mailbox{}
	for _, mb := range all {
		if mb.Enabled && mailboxInShard(mb.ID, shardIndex, shardCount) {
			enabled[mb.Name] = mb
		}
	}
	running := snapshotRunningByName()
	for name, mb := range enabled {
		if _, exists := running[name]; exists {
			continue
		}
		svc := buildService(mb)
		svc.StartQueueWorkers(ctx)
		if !server.RegisterMailboxService(mb.Name, svc) {
			continue
		}
		_ = startPoller(name, svc)
		registerRunning(name, mb.ID)
		log.Printf("worker reconcile started mailbox=%s", name)
	}
	for name, rm := range running {
		if _, shouldRun := enabled[name]; shouldRun {
			continue
		}
		stopped := stopPoller(rm.id)
		removed := server.UnregisterMailboxService(name, rm.id)
		unregisterRunning(name, rm.id)
		if stopped || removed {
			log.Printf("worker reconcile stopped mailbox=%s", name)
		}
	}
	return nil
}

func workerReconcileInterval() time.Duration {
	const defaultSec = 30
	raw := strings.TrimSpace(os.Getenv("WORKER_RECONCILE_SEC"))
	if raw == "" {
		return time.Duration(defaultSec) * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return time.Duration(defaultSec) * time.Second
	}
	return time.Duration(n) * time.Second
}

func filterMailboxesForShard(mailboxes []store.Mailbox, shardIndex, shardCount int) []store.Mailbox {
	if shardCount <= 0 || shardIndex < 0 {
		return mailboxes
	}
	out := make([]store.Mailbox, 0, len(mailboxes))
	for _, mb := range mailboxes {
		if mailboxInShard(mb.ID, shardIndex, shardCount) {
			out = append(out, mb)
		}
	}
	return out
}

func mailboxInShard(mailboxID string, shardIndex, shardCount int) bool {
	if shardCount <= 0 || shardIndex < 0 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(mailboxID))))
	return int(h.Sum32()%uint32(shardCount)) == shardIndex
}

func resolveMailboxes(ctx context.Context, st *store.Postgres, selectedName string, allMailboxes bool) ([]store.Mailbox, bool, error) {
	all, err := st.ListMailboxes(ctx)
	if err != nil {
		return nil, false, err
	}
	if len(all) == 0 {
		return nil, true, nil
	}
	enabled := make([]store.Mailbox, 0, len(all))
	for _, m := range all {
		if m.Enabled {
			enabled = append(enabled, m)
		}
	}
	if len(enabled) == 0 {
		return nil, false, fmt.Errorf("no enabled mailboxes configured")
	}
	if allMailboxes {
		return enabled, false, nil
	}
	if selectedName != "" {
		for _, m := range enabled {
			if m.Name == selectedName {
				return []store.Mailbox{m}, false, nil
			}
		}
		if _, err := st.GetMailboxByName(ctx, selectedName); err == nil {
			return nil, false, fmt.Errorf("mailbox %q is disabled", selectedName)
		}
		return nil, false, store.ErrNotFound
	}
	if len(enabled) == 1 {
		return []store.Mailbox{enabled[0]}, false, nil
	}
	return nil, false, fmt.Errorf("multiple enabled mailboxes configured; pass --mailbox <name> for one mailbox or --all-mailboxes for all")
}

func runAdminAPI() {
	args, allowMint := stripBoolFlag(os.Args, "--mint")
	if len(args) < 2 {
		usage(1)
	}
	baseURL := getenv("EMAIL_API", "http://127.0.0.1:8787")
	token := strings.TrimSpace(os.Getenv("EMAIL_TOKEN"))
	adminMailbox := strings.TrimSpace(os.Getenv("EMAIL_MAILBOX"))
	if token == "" {
		if !allowMint {
			fatal(fmt.Errorf("EMAIL_TOKEN is required; pass --mint to mint one interactively"))
		}
		if !isInteractiveStdin() {
			fatal(fmt.Errorf("EMAIL_TOKEN is required for non-interactive admin api commands; --mint requires interactive stdin"))
		}
		p, err := promptHiddenPassword("Master password: ")
		if err != nil {
			fatal(fmt.Errorf("read master password: %w", err))
		}
		token, err = mintInteractiveAdminToken(p)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "Minted admin EMAIL_TOKEN for this run.")
		fmt.Fprintf(os.Stderr, "To reuse it, run: export EMAIL_TOKEN='%s'\n", token)
	}
	c := cli.New(baseURL, token)
	cmd := args[1]
	switch cmd {
	case "alias":
		if len(args) < 3 {
			aliasUsage(os.Stderr)
			os.Exit(1)
		}
		sub := args[2]
		switch sub {
		case "add":
			if len(args) > 3 && (isHelpFlag(args[3]) || args[3] == "help") {
				aliasAddUsage(os.Stdout)
				return
			}
			if len(args) < 4 {
				aliasAddUsage(os.Stderr)
				os.Exit(1)
			}
			if err := c.Post(withMailboxQuery("/v1/admin/aliases", adminMailbox), map[string]string{"email": args[3]}, nil); err != nil {
				fatal(err)
			}
			fmt.Println("ok")
		case "list":
			var out any
			if err := c.Get(withMailboxQuery("/v1/admin/aliases", adminMailbox), nil, &out); err != nil {
				fatal(err)
			}
			printJSON(out)
		default:
			aliasUsage(os.Stderr)
			os.Exit(1)
		}
	case "token":
		if len(args) < 3 {
			tokenUsage(os.Stderr)
			os.Exit(1)
		}
		sub := args[2]
		switch sub {
		case "mint":
			mintArgs := normalizeFlagsAnywhere(args[3:], map[string]bool{
				"--name":   true,
				"--scopes": true,
			})
			fs := flag.NewFlagSet("emaild token mint", flag.ContinueOnError)
			fs.SetOutput(os.Stdout)
			fs.Usage = func() { tokenMintUsage(fs.Output()) }
			name := fs.String("name", "agent", "Display name for the token")
			scopes := fs.String("scopes", "read", "Comma-separated scopes (read,send)")
			if err := fs.Parse(mintArgs); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return
				}
				fatal(err)
			}
			if fs.NArg() < 1 {
				tokenMintUsage(os.Stderr)
				os.Exit(1)
			}
			aliasEmail := fs.Arg(0)
			var out map[string]string
			if err := c.Post(withMailboxQuery("/v1/admin/tokens", adminMailbox), map[string]any{"alias_email": aliasEmail, "name": *name, "scopes": splitCSV(*scopes)}, &out); err != nil {
				fatal(err)
			}
			fmt.Printf("token_id=%s\n", out["token_id"])
			fmt.Printf("token=%s\n", out["token"])
		case "revoke":
			if len(args) > 3 && (isHelpFlag(args[3]) || args[3] == "help") {
				tokenRevokeUsage(os.Stdout)
				return
			}
			if len(args) < 4 {
				tokenRevokeUsage(os.Stderr)
				os.Exit(1)
			}
			if err := c.Post(withMailboxQuery("/v1/admin/tokens/revoke", adminMailbox), map[string]string{"token_id": args[3]}, nil); err != nil {
				fatal(err)
			}
			fmt.Println("ok")
		default:
			tokenUsage(os.Stderr)
			os.Exit(1)
		}
	case "status":
		var out any
		if err := c.Get(withMailboxQuery("/v1/status", adminMailbox), nil, &out); err != nil {
			fatal(err)
		}
		printJSON(out)
	case "sync":
		if len(args) > 2 && (isHelpFlag(args[2]) || args[2] == "help") {
			syncUsage(os.Stdout)
			return
		}
		if len(args) < 3 || args[2] != "now" {
			syncUsage(os.Stderr)
			os.Exit(1)
		}
		// Large backfills can exceed the default API client timeout.
		c.HTTP.Timeout = 20 * time.Minute
		if err := c.Post(withMailboxQuery("/v1/admin/sync", adminMailbox), map[string]string{"now": "1"}, nil); err != nil {
			fatal(err)
		}
		fmt.Println("ok")
	default:
		usage(1)
	}
}

func withMailboxQuery(path, mailbox string) string {
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "mailbox=" + url.QueryEscape(mailbox)
}

func stripBoolFlag(args []string, flagName string) ([]string, bool) {
	if len(args) == 0 {
		return args, false
	}
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == flagName {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeFlagsAnywhere allows known flags to be passed before or after positionals.
// Unknown flags/args are left untouched so the standard flag parser can report errors.
func normalizeFlagsAnywhere(args []string, knownLong map[string]bool) []string {
	if len(args) == 0 {
		return args
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if a == "-h" || a == "--help" {
			flags = append(flags, a)
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := a
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				name = a[:eq]
			}
			wantsValue, ok := knownLong[name]
			if !ok {
				positionals = append(positionals, a)
				continue
			}
			flags = append(flags, a)
			if wantsValue && !strings.Contains(a, "=") && i+1 < len(args) {
				next := args[i+1]
				if next != "--" && !strings.HasPrefix(next, "-") {
					i++
					flags = append(flags, next)
				}
			}
			continue
		}
		positionals = append(positionals, a)
	}
	out := make([]string, 0, len(flags)+len(positionals))
	out = append(out, flags...)
	out = append(out, positionals...)
	return out
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func maybeHandleHelp(args []string) bool {
	args, _ = stripBoolFlag(args, "--mint")
	if len(args) == 0 {
		return false
	}
	if args[0] == "help" || isHelpFlag(args[0]) {
		if len(args) > 1 {
			printHelpForPath(args[1:])
			return true
		}
		usage(0)
		return true
	}
	if containsHelp(args[1:]) {
		printHelpForPath(args)
		return true
	}
	return false
}

func printHelpForPath(path []string) {
	if len(path) == 0 {
		usage(0)
		return
	}
	switch path[0] {
	case "migrate":
		migrateUsage(os.Stdout)
	case "encrypt-mailbox-credentials":
		encryptMailboxCredentialsUsage(os.Stdout)
	case "serve":
		serveUsage(os.Stdout)
	case "worker":
		workerUsage(os.Stdout)
	case "doctor":
		doctorUsage(os.Stdout)
	case "deps":
		depsUsage(os.Stdout)
	case "install":
		installUsage(os.Stdout)
	case "uninstall":
		uninstallUsage(os.Stdout)
	case "search-poc-eval":
		searchPOCEvalUsage(os.Stdout)
	case "alias":
		if len(path) < 2 {
			aliasUsage(os.Stdout)
			return
		}
		switch path[1] {
		case "add":
			aliasAddUsage(os.Stdout)
		case "list":
			aliasListUsage(os.Stdout)
		default:
			aliasUsage(os.Stdout)
		}
	case "token":
		if len(path) < 2 {
			tokenUsage(os.Stdout)
			return
		}
		switch path[1] {
		case "mint":
			tokenMintUsage(os.Stdout)
		case "revoke":
			tokenRevokeUsage(os.Stdout)
		default:
			tokenUsage(os.Stdout)
		}
	case "status":
		statusUsage(os.Stdout)
	case "sync":
		syncUsage(os.Stdout)
	default:
		usage(0)
	}
}

func containsHelp(args []string) bool {
	for _, a := range args {
		if a == "help" || isHelpFlag(a) {
			return true
		}
	}
	return false
}

func isHelpFlag(a string) bool {
	return a == "-h" || a == "--help"
}

func usage(exitCode int) {
	w := io.Writer(os.Stderr)
	if exitCode == 0 {
		w = os.Stdout
	}
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild [--mint] <command> [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  migrate   Apply database migrations")
	fmt.Fprintln(w, "  encrypt-mailbox-credentials Encrypt legacy plaintext mailbox credentials")
	fmt.Fprintln(w, "  serve     Run API server (and pollers unless --http-only)")
	fmt.Fprintln(w, "  worker    Run mailbox pollers/workers without HTTP server")
	fmt.Fprintln(w, "  doctor    Check local dependency readiness for attachment analysis")
	fmt.Fprintln(w, "  deps      Install local dependencies used by attachment analysis")
	fmt.Fprintln(w, "  install   Install user autostart service (macOS LaunchAgent)")
	fmt.Fprintln(w, "  uninstall Remove user autostart service (macOS LaunchAgent)")
	fmt.Fprintln(w, "  search-poc-eval Run search intelligence POC evaluation (no prod behavior changes)")
	fmt.Fprintln(w, "  alias     Manage alias addresses (admin API)")
	fmt.Fprintln(w, "  token     Manage API tokens (admin API)")
	fmt.Fprintln(w, "  status    Show server status (admin API)")
	fmt.Fprintln(w, "  sync      Trigger immediate sync (admin API)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global help:")
	fmt.Fprintln(w, "  emaild --help")
	fmt.Fprintln(w, "  emaild <command> --help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  EMAIL_TOKEN optional bearer token for admin API commands (recommended for automation)")
	fmt.Fprintln(w, "             if unset, pass --mint to prompt for master password and mint a token")
	fmt.Fprintln(w, "  EMAIL_API   API base URL for admin API commands (default http://127.0.0.1:8787)")
	fmt.Fprintln(w, "  EMAIL_MAILBOX optional mailbox name selector for admin API commands")
	fmt.Fprintln(w, "  EMAILD_ENV_FILE optional path to .env file for all emaild commands")
	fmt.Fprintln(w, "  MAILBOX_CREDENTIAL_KEY_FILE path to a mode-0600 mailbox encryption key file")
	fmt.Fprintln(w, "  SYNC_FOLDERS comma-separated IMAP folders to synchronize (default: provider folders)")
	fmt.Fprintln(w, "  FETCH_MAX_MESSAGES_PER_SYNC max IMAP messages fetched per mailbox sync cycle (default 200)")
	fmt.Fprintln(w, "  WORKER_RECONCILE_SEC worker --all-mailboxes reconcile interval seconds (default 30)")
	fmt.Fprintln(w, "  INBOX_FOLDER_ALL_ENABLED enable folder=all on /v1/inbox/list (default false)")
	os.Exit(exitCode)
}

func migrateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild migrate")
}

func encryptMailboxCredentialsUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild encrypt-mailbox-credentials [--plaintext-mailbox-id UUID ...]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Encrypts legacy plaintext mailbox passwords and rotates old-key envelopes to the configured primary key in one transaction.")
	fmt.Fprintln(w, "Use --plaintext-mailbox-id only for a verified legacy plaintext value whose enc: prefix collides with the envelope format.")
	fmt.Fprintln(w, "Stop all API and worker processes and back up the database and keys before running this command.")
}

func serveUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild serve [--mailbox <name> | --all-mailboxes] [--http-only]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Modes:")
	fmt.Fprintln(w, "  single-auto (default): runs only mailbox when exactly one is configured")
	fmt.Fprintln(w, "  single: pass --mailbox <name> to run one mailbox when many are configured")
	fmt.Fprintln(w, "  all: pass --all-mailboxes to run all configured mailboxes")
	fmt.Fprintln(w, "  api-only: pass --http-only to disable pollers/workers")
}

func workerUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild worker [--mailbox <name> | --all-mailboxes] [--shard-index N --shard-count K]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Modes:")
	fmt.Fprintln(w, "  single-auto (default): runs only mailbox when exactly one is configured")
	fmt.Fprintln(w, "  single: pass --mailbox <name> to run one mailbox when many are configured")
	fmt.Fprintln(w, "  all: pass --all-mailboxes to run all configured mailboxes")
	fmt.Fprintln(w, "  shard: with --all-mailboxes, pass --shard-index/--shard-count for deterministic partitioning")
}

func doctorUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild doctor")
}

func depsUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild deps install")
}

func installUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild install [--env-file <path>] [--workdir <path>]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintln(w, "  macOS only; installs a LaunchAgent for current user")
	fmt.Fprintln(w, "  copies env file to ~/Library/Application Support/emaild/.env")
	fmt.Fprintln(w, "  default workdir is current directory and must contain migrations/")
}

func uninstallUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild uninstall")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintln(w, "  removes current-user LaunchAgent and plist file")
	fmt.Fprintln(w, "  keeps ~/Library/Application Support/emaild/.env intact")
}

func aliasUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild alias <add|list> [options]")
}

func aliasAddUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild alias add <alias_email>")
}

func aliasListUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild alias list")
}

func tokenUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild token <mint|revoke> [options]")
}

func tokenMintUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild token mint <alias_email> [--name <agent_name>] [--scopes read,send]")
}

func tokenRevokeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild token revoke <token_id>")
}

func statusUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild status")
}

func syncUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  emaild sync now")
}

func promptHiddenPassword(prompt string) (string, error) {
	if !isInteractiveStdin() {
		return "", fmt.Errorf("stdin is not interactive")
	}
	fmt.Fprint(os.Stderr, prompt)
	disableEcho := exec.Command("stty", "-echo")
	disableEcho.Stdin = os.Stdin
	if err := disableEcho.Run(); err != nil {
		return "", err
	}
	defer func() {
		enableEcho := exec.Command("stty", "echo")
		enableEcho.Stdin = os.Stdin
		_ = enableEcho.Run()
		fmt.Fprintln(os.Stderr)
	}()
	reader := bufio.NewReader(os.Stdin)
	pw, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(pw), nil
}

func isInteractiveStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func mintInteractiveAdminToken(password string) (string, error) {
	cfg := mustConfig()
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("password cannot be empty")
	}
	if len(password) != len(cfg.MasterKey) || subtle.ConstantTimeCompare([]byte(password), []byte(cfg.MasterKey)) != 1 {
		return "", fmt.Errorf("invalid password")
	}
	db := dbConnect(cfg.PostgresDSN)
	defer db.Close()
	raw, err := auth.GenerateToken()
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	tokenHash := auth.HashToken(cfg.MasterKey, raw)
	st := store.NewPostgres(db)
	if _, err := st.MintToken(context.Background(), nil, "interactive-admin", tokenHash, []string{string(model.ScopeAdmin)}); err != nil {
		return "", fmt.Errorf("mint admin token: %w", err)
	}
	return raw, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
