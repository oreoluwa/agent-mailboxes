package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr             string
	ConnectListenAddr      string
	PostgresDSN            string
	MasterKey              string
	MasterKeyOlds          []string
	MailboxCredentialKey   string
	MailboxCredentialOlds  []string
	SyncFolders            []string
	ConnectEnabled         bool
	PollInterval           time.Duration
	FetchMaxMessages       int
	InitialSyncMaxMessages int
	ThreadContext          ThreadContextConfig
	CatchallAdmin          bool
	InboxAllEnabled        bool
	OpenAIBaseURL          string
	OpenAIAPIKey           string
	ModelName              string
	AgentModelName         string
	SummaryEnabled         bool
	SearchParserModel      string
	SearchParserTimeout    time.Duration
	SearchDefaultTZ        string
	ActorEnvelopeSecret    string
	ActorReplayWindow      time.Duration
	SummarizeWorkers       int
	SummaryTimeout         time.Duration
	EmbeddingModel         string
	EmbeddingBatch         int
	EmbeddingFlush         time.Duration
	VisionModelName        string
	AttachSummary          AttachmentSummaryConfig
	AttachS3               S3Config
}

type ThreadContextConfig struct {
	Mode     string
	MaxChars int
}

type AttachmentSummaryConfig struct {
	Enabled             bool
	MaxFiles            int
	MaxFileBytes        int64
	VisionProviderMode  string
	VisionMaxRetries    int
	VisionTimeoutSec    int
	VisionMaxImageBytes int64
	PDFRenderToolBin    string
	OfficeConverterBin  string
	RequirePDFRenderer  bool
	PromptInstallDeps   bool
}

type S3Config struct {
	Endpoint           string
	Region             string
	Bucket             string
	AccessKey          string
	SecretKey          string
	Prefix             string
	PathStyle          bool
	InsecureSkipVerify bool
}

func Load() (Config, error) {
	mailboxCredentialKey, err := loadMailboxCredentialKey()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenAddr:        getenv("EMAILD_LISTEN", "127.0.0.1:8787"),
		ConnectListenAddr: getenv("EMAILD_CONNECT_LISTEN", "127.0.0.1:8788"),
		PostgresDSN:       os.Getenv("POSTGRES_DSN"),
		MasterKey:         os.Getenv("MASTER_KEY"),
		MasterKeyOlds: splitCSV(
			getenv("MASTER_KEY_OLDS", os.Getenv("MASTER_KEY_OLD")),
		),
		MailboxCredentialKey: mailboxCredentialKey,
		MailboxCredentialOlds: splitCSV(
			os.Getenv("MAILBOX_CREDENTIAL_KEY_OLDS"),
		),
		SyncFolders:            splitCSV(os.Getenv("SYNC_FOLDERS")),
		ConnectEnabled:         getenvBool("EMAILD_CONNECT_ENABLED", false),
		FetchMaxMessages:       getenvInt("FETCH_MAX_MESSAGES_PER_SYNC", 200),
		InitialSyncMaxMessages: getenvInt("INITIAL_SYNC_MAX_MESSAGES", 200),
		ThreadContext: ThreadContextConfig{
			Mode:     normalizeThreadContextMode(getenv("SUMMARY_THREAD_CONTEXT_MODE", "full")),
			MaxChars: getenvInt("SUMMARY_THREAD_CONTEXT_MAX_CHARS", 0),
		},
		CatchallAdmin:       getenvBool("CATCHALL_ADMIN", false),
		InboxAllEnabled:     getenvBool("INBOX_FOLDER_ALL_ENABLED", false),
		OpenAIBaseURL:       getenv("OPENAI_BASE_URL", getenv("OLLAMA_HOST", "http://127.0.0.1:11434")),
		OpenAIAPIKey:        getenv("OPENAI_API_KEY", ""),
		ModelName:           getenv("MODEL_NAME", "llama3:latest"),
		AgentModelName:      strings.TrimSpace(getenv("AGENT_MODEL_NAME", getenv("MODEL_NAME", "llama3:latest"))),
		SummaryEnabled:      getenvBool("SUMMARY_ENABLED", true),
		SearchParserModel:   strings.TrimSpace(getenv("SEARCH_PARSER_MODEL", getenv("MODEL_NAME", "llama3:latest"))),
		SearchParserTimeout: time.Duration(getenvInt("SEARCH_PARSER_TIMEOUT_SEC", 45)) * time.Second,
		SearchDefaultTZ:     strings.TrimSpace(getenv("SEARCH_DEFAULT_TIMEZONE", "")),
		ActorEnvelopeSecret: strings.TrimSpace(os.Getenv("ACTOR_ENVELOPE_SECRET")),
		ActorReplayWindow:   time.Duration(getenvInt("ACTOR_REPLAY_WINDOW_SEC", 300)) * time.Second,
		SummarizeWorkers:    getenvInt("SUMMARY_WORKER_CONCURRENCY", 4),
		SummaryTimeout:      time.Duration(getenvInt("SUMMARY_TIMEOUT_SEC", 90)) * time.Second,
		EmbeddingModel:      strings.TrimSpace(os.Getenv("EMBEDDING_MODEL_NAME")),
		EmbeddingBatch:      getenvInt("EMBEDDING_BATCH_MAX_INPUTS", 96),
		EmbeddingFlush:      time.Duration(getenvInt("EMBEDDING_FLUSH_INTERVAL_SEC", 1800)) * time.Second,
		VisionModelName:     strings.TrimSpace(os.Getenv("VISION_MODEL_NAME")),
		AttachSummary: AttachmentSummaryConfig{
			Enabled:             getenvBool("ATTACH_SUMMARY_ENABLED", true),
			MaxFiles:            getenvInt("ATTACH_SUMMARY_MAX_FILES", 3),
			MaxFileBytes:        getenvInt64("ATTACH_SUMMARY_MAX_FILE_BYTES", 8*1024*1024),
			VisionProviderMode:  strings.ToLower(getenv("VISION_PROVIDER_MODE", "auto")),
			VisionMaxRetries:    getenvInt("VISION_MAX_RETRIES", 2),
			VisionTimeoutSec:    getenvInt("VISION_TIMEOUT_SEC", 60),
			VisionMaxImageBytes: getenvInt64("VISION_MAX_IMAGE_BYTES", 2*1024*1024),
			PDFRenderToolBin:    getenv("PDF_RENDER_TOOL", "pdftoppm"),
			OfficeConverterBin:  getenv("OFFICE_CONVERTER_TOOL", "soffice"),
			RequirePDFRenderer:  getenvBool("ATTACH_REQUIRE_PDF_RENDERER", false),
			PromptInstallDeps:   getenvBool("ATTACH_PROMPT_INSTALL_DEPS", true),
		},
		AttachS3: S3Config{
			Endpoint:           os.Getenv("ATTACHMENT_S3_ENDPOINT"),
			Region:             getenv("ATTACHMENT_S3_REGION", "us-east-1"),
			Bucket:             os.Getenv("ATTACHMENT_S3_BUCKET"),
			AccessKey:          os.Getenv("ATTACHMENT_S3_ACCESS_KEY"),
			SecretKey:          os.Getenv("ATTACHMENT_S3_SECRET_KEY"),
			Prefix:             os.Getenv("ATTACHMENT_S3_PREFIX"),
			PathStyle:          getenvBool("ATTACHMENT_S3_PATH_STYLE", true),
			InsecureSkipVerify: getenvBool("ATTACHMENT_S3_INSECURE_SKIP_VERIFY", false),
		},
	}
	pollSec := getenvInt("POLL_INTERVAL_SEC", 30)
	cfg.PollInterval = time.Duration(pollSec) * time.Second

	if cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("POSTGRES_DSN is required")
	}
	if cfg.MasterKey == "" {
		return Config{}, fmt.Errorf("MASTER_KEY is required")
	}
	if cfg.InitialSyncMaxMessages < -1 {
		return Config{}, fmt.Errorf("INITIAL_SYNC_MAX_MESSAGES must be -1, 0, or a positive integer")
	}
	return cfg, nil
}

func loadMailboxCredentialKey() (string, error) {
	direct := strings.TrimSpace(os.Getenv("MAILBOX_CREDENTIAL_KEY"))
	path := strings.TrimSpace(os.Getenv("MAILBOX_CREDENTIAL_KEY_FILE"))
	if direct != "" && path != "" {
		return "", fmt.Errorf("MAILBOX_CREDENTIAL_KEY and MAILBOX_CREDENTIAL_KEY_FILE cannot both be set")
	}
	if path == "" {
		return direct, nil
	}
	data, err := readMailboxCredentialKeyFile(path)
	if err != nil {
		return "", fmt.Errorf("read MAILBOX_CREDENTIAL_KEY_FILE: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", fmt.Errorf("MAILBOX_CREDENTIAL_KEY_FILE is empty")
	}
	return key, nil
}

func (c Config) MasterKeysForVerify() []string {
	out := []string{c.MasterKey}
	out = append(out, c.MasterKeyOlds...)
	return out
}

func normalizeThreadContextMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "full":
		return "full"
	case "none":
		return "none"
	default:
		return "full"
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func getenvInt(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}

func getenvInt64(k string, d int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return d
	}
	return n
}

func getenvBool(k string, d bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return d
	}
	return b
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}
