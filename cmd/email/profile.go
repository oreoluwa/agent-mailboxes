package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"strings"
)

const maxAgentProfileBytes = 64 * 1024

type agentProfile struct {
	Version int    `json:"version"`
	APIURL  string `json:"api_url"`
	Mailbox string `json:"mailbox"`
	Alias   string `json:"alias"`
	Token   string `json:"token"`
}

type cliConfig struct {
	APIURL  string
	Mailbox string
	Token   string
	Args    []string
}

func resolveCLIConfig(args []string, getenv func(string) string) (cliConfig, error) {
	profilePath, commandArgs, err := parseProfileFlag(args)
	if err != nil {
		return cliConfig{}, err
	}
	if profilePath == "" {
		profilePath = strings.TrimSpace(getenv("EMAIL_PROFILE"))
	}
	if profilePath != "" {
		profile, err := loadAgentProfile(profilePath)
		if err != nil {
			return cliConfig{}, err
		}
		return cliConfig{APIURL: profile.APIURL, Mailbox: profile.Mailbox, Token: profile.Token, Args: commandArgs}, nil
	}
	apiURL := strings.TrimSpace(getenv("EMAIL_API"))
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8787"
	}
	token := getenv("EMAIL_TOKEN")
	if token == "" {
		return cliConfig{}, errors.New("EMAIL_TOKEN is required (or configure --profile / EMAIL_PROFILE)")
	}
	return cliConfig{APIURL: apiURL, Token: token, Args: commandArgs}, nil
}

func parseProfileFlag(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", args, nil
	}
	if args[0] == "--profile" {
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return "", nil, errors.New("--profile requires a file path")
		}
		return strings.TrimSpace(args[1]), args[2:], nil
	}
	if strings.HasPrefix(args[0], "--profile=") {
		path := strings.TrimSpace(strings.TrimPrefix(args[0], "--profile="))
		if path == "" {
			return "", nil, errors.New("--profile requires a file path")
		}
		return path, args[1:], nil
	}
	return "", args, nil
}

func loadAgentProfile(path string) (agentProfile, error) {
	data, err := readPrivateAgentProfile(path, maxAgentProfileBytes)
	if err != nil {
		return agentProfile{}, fmt.Errorf("load email profile: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var profile agentProfile
	if err := decoder.Decode(&profile); err != nil {
		return agentProfile{}, errors.New("load email profile: invalid JSON profile")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return agentProfile{}, errors.New("load email profile: invalid JSON profile")
	}
	if err := validateAgentProfile(profile); err != nil {
		return agentProfile{}, err
	}
	profile.APIURL = strings.TrimRight(strings.TrimSpace(profile.APIURL), "/")
	profile.Mailbox = strings.TrimSpace(profile.Mailbox)
	profile.Alias = strings.ToLower(strings.TrimSpace(profile.Alias))
	return profile, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("trailing JSON value")
}

func validateAgentProfile(profile agentProfile) error {
	if profile.Version != 1 {
		return errors.New("load email profile: unsupported profile version")
	}
	if err := validateProfileAPIURL(profile.APIURL); err != nil {
		return fmt.Errorf("load email profile: %w", err)
	}
	if strings.TrimSpace(profile.Mailbox) == "" {
		return errors.New("load email profile: mailbox is required")
	}
	alias := strings.TrimSpace(profile.Alias)
	parsed, err := mail.ParseAddress(alias)
	if err != nil || !strings.EqualFold(parsed.Address, alias) {
		return errors.New("load email profile: alias must be a plain email address")
	}
	if strings.TrimSpace(profile.Token) == "" {
		return errors.New("load email profile: token is required")
	}
	return nil
}

func validateProfileAPIURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("api_url must be an origin URL without credentials, path, query, or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("api_url HTTP endpoints must use a literal loopback IP")
		}
		return nil
	default:
		return errors.New("api_url must use HTTPS or loopback HTTP")
	}
}
