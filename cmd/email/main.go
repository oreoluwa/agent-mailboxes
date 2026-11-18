package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oreoluwa/agent-mailboxes/internal/cli"
	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func main() {
	profilePath, commandArgs, err := parseProfileFlag(os.Args[1:])
	if err != nil {
		fatal(err.Error())
	}
	if len(commandArgs) < 1 {
		usage(1)
	}
	if maybeHandleHelp(commandArgs) {
		return
	}

	configArgs := commandArgs
	if profilePath != "" {
		configArgs = append([]string{"--profile", profilePath}, commandArgs...)
	}
	config, err := resolveCLIConfig(configArgs, os.Getenv)
	if err != nil {
		fatal(err.Error())
	}
	c := cli.NewForMailbox(config.APIURL, config.Token, config.Mailbox)

	switch config.Args[0] {
	case "inbox":
		mailboxGroupCmd(c, config.Args[1:], mailboxGroupConfig{
			Name:        "inbox",
			Folder:      "inbox",
			AllowUnread: true,
			AllowAck:    true,
		})
	case "junk":
		mailboxGroupCmd(c, config.Args[1:], mailboxGroupConfig{
			Name:        "junk",
			Folder:      "junk",
			AllowUnread: true,
			AllowAck:    true,
		})
	case "sent":
		mailboxGroupCmd(c, config.Args[1:], mailboxGroupConfig{
			Name:        "sent",
			Folder:      "sent",
			AllowUnread: false,
			AllowAck:    false,
		})
	case "send":
		sendCmd(c, config.Args[1:])
	case "reply":
		replyCmd(c, config.Args[1:])
	case "search":
		searchCmd(c, config.Args[1:])
	default:
		usage(1)
	}
}

type mailboxGroupConfig struct {
	Name        string
	Folder      string
	AllowUnread bool
	AllowAck    bool
}

func mailboxGroupCmd(c *cli.Client, args []string, cfg mailboxGroupConfig) {
	if len(args) == 0 || args[0] == "help" {
		mailboxUsage(os.Stdout, cfg)
		return
	}
	switch args[0] {
	case "list":
		knownFlags := map[string]bool{
			"--limit": true,
			"--json":  false,
		}
		if cfg.AllowUnread {
			knownFlags["--unread"] = false
		}
		listArgs := normalizeFlagsAnywhere(args[1:], knownFlags)
		fs := flag.NewFlagSet("email "+cfg.Name+" list", flag.ContinueOnError)
		fs.SetOutput(os.Stdout)
		fs.Usage = func() { mailboxListUsage(fs.Output(), cfg) }
		var unreadPtr *bool
		if cfg.AllowUnread {
			unreadPtr = fs.Bool("unread", false, "Only show unacknowledged messages")
		}
		limit := fs.Int("limit", 20, "Maximum number of messages to return")
		jsonOut := fs.Bool("json", false, "Print raw JSON output")
		if err := fs.Parse(listArgs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fatal(err.Error())
		}
		unread := false
		if unreadPtr != nil {
			unread = *unreadPtr
		}
		var out []model.ListItem
		err := c.Get("/v1/inbox/list", map[string]string{
			"unread": fmt.Sprintf("%v", unread),
			"limit":  fmt.Sprintf("%d", *limit),
			"folder": cfg.Folder,
		}, &out)
		if err != nil {
			fatal(err.Error())
		}
		if *jsonOut {
			cli.PrintJSON(out)
			return
		}
		cli.PrintInboxList(out)
	case "read":
		readArgs := normalizeFlagsAnywhere(args[1:], map[string]bool{
			"--json": false,
		})
		fs := flag.NewFlagSet("email "+cfg.Name+" read", flag.ContinueOnError)
		fs.SetOutput(os.Stdout)
		fs.Usage = func() { mailboxReadUsage(fs.Output(), cfg.Name) }
		jsonOut := fs.Bool("json", false, "Print raw JSON output")
		if err := fs.Parse(readArgs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fatal(err.Error())
		}
		if fs.NArg() < 1 {
			mailboxReadUsage(os.Stderr, cfg.Name)
			os.Exit(1)
		}
		id := fs.Arg(0)
		var out model.Message
		if err := c.Get("/v1/inbox/read", map[string]string{"id": id}, &out); err != nil {
			fatal(err.Error())
		}
		if *jsonOut {
			cli.PrintJSON(out)
			return
		}
		cli.PrintMessage(out)
	case "body":
		bodyArgs := normalizeFlagsAnywhere(args[1:], map[string]bool{
			"--json": false,
		})
		fs := flag.NewFlagSet("email "+cfg.Name+" body", flag.ContinueOnError)
		fs.SetOutput(os.Stdout)
		fs.Usage = func() { mailboxBodyUsage(fs.Output(), cfg.Name) }
		jsonOut := fs.Bool("json", false, "Print raw JSON output")
		if err := fs.Parse(bodyArgs); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fatal(err.Error())
		}
		if fs.NArg() < 1 {
			mailboxBodyUsage(os.Stderr, cfg.Name)
			os.Exit(1)
		}
		id := fs.Arg(0)
		var out model.Body
		if err := c.Get("/v1/inbox/body", map[string]string{"id": id}, &out); err != nil {
			fatal(err.Error())
		}
		if *jsonOut {
			cli.PrintJSON(out)
			return
		}
		if out.ExtractedText != nil {
			fmt.Println(*out.ExtractedText)
		}
	case "ack":
		if !cfg.AllowAck {
			mailboxUsage(os.Stderr, cfg)
			os.Exit(1)
		}
		if len(args) > 1 && (args[1] == "help" || isHelpFlag(args[1])) {
			mailboxAckUsage(os.Stdout, cfg.Name)
			return
		}
		if len(args) < 2 {
			mailboxAckUsage(os.Stderr, cfg.Name)
			os.Exit(1)
		}
		id := args[1]
		if err := c.Post("/v1/inbox/ack", map[string]string{"message_id": id}, nil); err != nil {
			fatal(err.Error())
		}
		fmt.Println("ok")
	default:
		mailboxUsage(os.Stderr, cfg)
		os.Exit(1)
	}
}

func sendCmd(c *cli.Client, args []string) {
	args = normalizeFlagsAnywhere(args, map[string]bool{
		"--to":        true,
		"--subject":   true,
		"--body-file": true,
		"--json":      false,
	})
	fs := flag.NewFlagSet("email send", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { sendUsage(fs.Output()) }
	to := fs.String("to", "", "Recipient email(s), comma-separated")
	subject := fs.String("subject", "", "Email subject")
	bodyFile := fs.String("body-file", "", "Path to a text/markdown file used as message body")
	jsonOut := fs.Bool("json", false, "Print raw JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err.Error())
	}
	if *to == "" || *bodyFile == "" {
		sendUsage(os.Stderr)
		os.Exit(1)
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fatal(err.Error())
	}
	payload := map[string]any{"to": splitCSV(*to), "subject": *subject, "body": string(body)}
	var out map[string]any
	if err := c.Post("/v1/send", payload, &out); err != nil {
		fatal(err.Error())
	}
	if *jsonOut {
		cli.PrintJSON(out)
		return
	}
	fmt.Println("sent")
}

func replyCmd(c *cli.Client, args []string) {
	args = normalizeFlagsAnywhere(args, map[string]bool{
		"--body-file": true,
		"--json":      false,
	})
	fs := flag.NewFlagSet("email reply", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { replyUsage(fs.Output()) }
	bodyFile := fs.String("body-file", "", "Path to a text/markdown file used as reply body")
	jsonOut := fs.Bool("json", false, "Print raw JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err.Error())
	}
	if fs.NArg() < 1 || *bodyFile == "" {
		replyUsage(os.Stderr)
		os.Exit(1)
	}
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fatal(err.Error())
	}
	var out map[string]any
	if err := c.Post("/v1/reply", map[string]any{"message_id": fs.Arg(0), "body": string(body)}, &out); err != nil {
		fatal(err.Error())
	}
	if *jsonOut {
		cli.PrintJSON(out)
		return
	}
	fmt.Println("sent")
}

func searchCmd(c *cli.Client, args []string) {
	args = normalizeFlagsAnywhere(args, map[string]bool{
		"--limit":  true,
		"--folder": true,
		"--json":   false,
	})
	fs := flag.NewFlagSet("email search", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { searchUsage(fs.Output()) }
	limit := fs.Int("limit", 50, "Maximum number of messages to return")
	folder := fs.String("folder", "", "Optional folder filter: all, inbox, junk|spam, sent")
	jsonOut := fs.Bool("json", false, "Print raw JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err.Error())
	}
	if fs.NArg() < 1 {
		searchUsage(os.Stderr)
		os.Exit(1)
	}
	q := fs.Arg(0)
	var out []model.ListItem
	params := map[string]string{"q": q, "limit": fmt.Sprintf("%d", *limit)}
	if strings.TrimSpace(*folder) != "" {
		params["folder"] = strings.TrimSpace(*folder)
	}
	if err := c.Get("/v1/search", params, &out); err != nil {
		fatal(err.Error())
	}
	if *jsonOut {
		cli.PrintJSON(out)
		return
	}
	cli.PrintInboxList(out)
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

func maybeHandleHelp(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "help" || isHelpFlag(args[0]) {
		usage(0)
		return true
	}
	if containsHelp(args[1:]) {
		switch args[0] {
		case "inbox":
			printMailboxHelp(os.Stdout, args, mailboxGroupConfig{Name: "inbox", Folder: "inbox", AllowUnread: true, AllowAck: true})
		case "junk":
			printMailboxHelp(os.Stdout, args, mailboxGroupConfig{Name: "junk", Folder: "junk", AllowUnread: true, AllowAck: true})
		case "sent":
			printMailboxHelp(os.Stdout, args, mailboxGroupConfig{Name: "sent", Folder: "sent", AllowUnread: false, AllowAck: false})
		case "send":
			sendUsage(os.Stdout)
		case "reply":
			replyUsage(os.Stdout)
		case "search":
			searchUsage(os.Stdout)
		default:
			usage(0)
		}
		return true
	}
	return false
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
	fmt.Fprintln(w, "  email <command> [options]")
	fmt.Fprintln(w, "  email --profile <agent-profile.json> <command> [options]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  inbox    Manage inbox messages (list/read/body/ack)")
	fmt.Fprintln(w, "  junk     Manage junk/spam messages (list/read/body/ack)")
	fmt.Fprintln(w, "  sent     Manage sent messages (list/read/body)")
	fmt.Fprintln(w, "  send     Send a new email")
	fmt.Fprintln(w, "  reply    Reply to a message")
	fmt.Fprintln(w, "  search   Search messages")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global help:")
	fmt.Fprintln(w, "  email --help")
	fmt.Fprintln(w, "  email <command> --help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Environment:")
	fmt.Fprintln(w, "  EMAIL_PROFILE         Private agent profile file (alternative to --profile)")
	fmt.Fprintln(w, "  EMAIL_TOKEN           Legacy API bearer token (required without a profile)")
	fmt.Fprintln(w, "  EMAIL_API             Legacy API base URL (default http://127.0.0.1:8787)")
	os.Exit(exitCode)
}

func printMailboxHelp(w io.Writer, args []string, cfg mailboxGroupConfig) {
	if len(args) < 2 {
		mailboxUsage(w, cfg)
		return
	}
	switch args[1] {
	case "list":
		mailboxListUsage(w, cfg)
	case "read":
		mailboxReadUsage(w, cfg.Name)
	case "body":
		mailboxBodyUsage(w, cfg.Name)
	case "ack":
		if cfg.AllowAck {
			mailboxAckUsage(w, cfg.Name)
			return
		}
		mailboxUsage(w, cfg)
	default:
		mailboxUsage(w, cfg)
	}
}

func mailboxUsage(w io.Writer, cfg mailboxGroupConfig) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  email %s <subcommand> [options]\n", cfg.Name)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintf(w, "  list       List %s messages\n", cfg.Name)
	fmt.Fprintln(w, "  read       Read message metadata and summary")
	fmt.Fprintln(w, "  body       Print extracted message body text")
	if cfg.AllowAck {
		fmt.Fprintln(w, "  ack        Mark a message as acknowledged")
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Examples:")
	if cfg.AllowUnread {
		fmt.Fprintf(w, "  email %s list --unread --limit 10\n", cfg.Name)
	} else {
		fmt.Fprintf(w, "  email %s list --limit 10\n", cfg.Name)
	}
	fmt.Fprintf(w, "  email %s read <msg_id>\n", cfg.Name)
	fmt.Fprintf(w, "  email %s body <msg_id>\n", cfg.Name)
	if cfg.AllowAck {
		fmt.Fprintf(w, "  email %s ack <msg_id>\n", cfg.Name)
	}
}

func mailboxListUsage(w io.Writer, cfg mailboxGroupConfig) {
	fmt.Fprintln(w, "Usage:")
	if cfg.AllowUnread {
		fmt.Fprintf(w, "  email %s list [--unread] [--limit N] [--json]\n", cfg.Name)
		return
	}
	fmt.Fprintf(w, "  email %s list [--limit N] [--json]\n", cfg.Name)
}

func mailboxReadUsage(w io.Writer, mailbox string) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  email %s read <msg_id> [--json]\n", mailbox)
}

func mailboxBodyUsage(w io.Writer, mailbox string) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  email %s body <msg_id> [--json]\n", mailbox)
}

func mailboxAckUsage(w io.Writer, mailbox string) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  email %s ack <msg_id>\n", mailbox)
}

func sendUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  email send --to <email[,email]> [--subject <subj>] --body-file <path> [--json]")
}

func replyUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  email reply <msg_id> --body-file <path> [--json]")
}

func searchUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  email search <query> [--folder all|inbox|junk|spam|sent] [--limit N] [--json]")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
