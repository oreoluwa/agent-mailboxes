package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const launchAgentLabel = "com.agentemails.emaild"

func runInstall() {
	fs := flag.NewFlagSet("emaild install", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	fs.Usage = func() { installUsage(fs.Output()) }
	envFile := fs.String("env-file", ".env", "Source env file to install")
	workdir := fs.String("workdir", "", "Working directory for `emaild serve` (must contain migrations)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fatal(err)
	}
	if runtime.GOOS != "darwin" {
		fatal(fmt.Errorf("install is currently supported on macOS only"))
	}
	wd := strings.TrimSpace(*workdir)
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			fatal(err)
		}
	}
	if err := installLaunchAgent(strings.TrimSpace(*envFile), wd); err != nil {
		fatal(err)
	}
}

func runUninstall() {
	if runtime.GOOS != "darwin" {
		fatal(fmt.Errorf("uninstall is currently supported on macOS only"))
	}
	if err := uninstallLaunchAgent(); err != nil {
		fatal(err)
	}
}

func installLaunchAgent(sourceEnvPath, workdir string) error {
	workdirAbs, err := filepath.Abs(workdir)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	migrationsDir := filepath.Join(workdirAbs, "migrations")
	if fi, err := os.Stat(migrationsDir); err != nil || !fi.IsDir() {
		if err == nil {
			return fmt.Errorf("migrations path is not a directory: %s", migrationsDir)
		}
		return fmt.Errorf("workdir must contain migrations directory: %s", migrationsDir)
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolve user config dir: %w", err)
	}
	envDest := filepath.Join(cfgDir, "emaild", ".env")
	if err := os.MkdirAll(filepath.Dir(envDest), 0o755); err != nil {
		return fmt.Errorf("create env directory: %w", err)
	}
	if err := stageEnvFile(sourceEnvPath, envDest); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home dir: %w", err)
	}
	logDir := filepath.Join(home, "Library", "Logs", "emaild")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents directory: %w", err)
	}

	exePath, err := resolveExecutablePath()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(launchAgentsDir, launchAgentLabel+".plist")
	plist := buildLaunchAgentPlist(exePath, workdirAbs, envDest, filepath.Join(logDir, "emaild.log"), filepath.Join(logDir, "emaild.err.log"))
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = runLaunchctl("bootout", domain+"/"+launchAgentLabel)
	if err := runLaunchctl("bootstrap", domain, plistPath); err != nil {
		return err
	}
	_ = runLaunchctl("enable", domain+"/"+launchAgentLabel)
	if err := runLaunchctl("kickstart", "-k", domain+"/"+launchAgentLabel); err != nil {
		return err
	}

	fmt.Printf("installed LaunchAgent %s\n", launchAgentLabel)
	fmt.Printf("plist: %s\n", plistPath)
	fmt.Printf("env: %s\n", envDest)
	fmt.Printf("workdir: %s\n", workdirAbs)
	return nil
}

func uninstallLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home dir: %w", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = runLaunchctl("bootout", domain+"/"+launchAgentLabel)
	_ = runLaunchctl("disable", domain+"/"+launchAgentLabel)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	fmt.Printf("uninstalled LaunchAgent %s\n", launchAgentLabel)
	fmt.Printf("removed plist: %s\n", plistPath)
	return nil
}

func stageEnvFile(sourceEnvPath, destination string) error {
	sourceEnvPath = strings.TrimSpace(sourceEnvPath)
	if sourceEnvPath == "" {
		sourceEnvPath = ".env"
	}
	srcAbs, err := filepath.Abs(sourceEnvPath)
	if err != nil {
		return fmt.Errorf("resolve env path: %w", err)
	}
	data, err := os.ReadFile(srcAbs)
	if err != nil {
		if os.IsNotExist(err) {
			if _, dstErr := os.Stat(destination); dstErr == nil {
				return nil
			}
			return fmt.Errorf("env file not found: %s (and no existing installed env at %s)", srcAbs, destination)
		}
		return fmt.Errorf("read env file: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("write installed env file: %w", err)
	}
	return nil
}

func resolveExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exePath)
	if err == nil {
		exePath = resolved
	}
	return exePath, nil
}

func runLaunchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			return fmt.Errorf("launchctl %s: %w", strings.Join(args, " "), err)
		}
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return nil
}

func buildLaunchAgentPlist(programPath, workdir, envPath, stdoutPath, stderrPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>EMAILD_ENV_FILE</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(launchAgentLabel), xmlEscape(programPath), xmlEscape(workdir), xmlEscape(envPath), xmlEscape(stdoutPath), xmlEscape(stderrPath))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
