package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	SMTPAddr        string
	HTTPAddr        string
	DBPath          string
	AllowedMailbox  map[string]struct{}
	MaxConnections  int
	ReadDeadlineSec int
	MaxLineBytes    int
	MaxRecipients   int
	MaxMessageBytes int64
}

func defaultConfig() Config {
	return Config{
		SMTPAddr:        "127.0.0.1:8025",
		HTTPAddr:        "127.0.0.1:8080",
		DBPath:          "mail.db",
		AllowedMailbox:  map[string]struct{}{},
		MaxConnections:  50,
		ReadDeadlineSec: 300,
		MaxLineBytes:    2048,
		MaxRecipients:   100,
		MaxMessageBytes: 25 * 1024 * 1024,
	}
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	var recipients string
	var err error

	if value := os.Getenv("SMTP_ADDR"); value != "" {
		cfg.SMTPAddr, err = parseLoopbackAddr(value, "SMTP_ADDR")
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("HTTP_ADDR"); value != "" {
		cfg.HTTPAddr, err = parseLoopbackAddr(value, "HTTP_ADDR")
		if err != nil {
			return cfg, err
		}
	}
	if value := os.Getenv("DB_PATH"); value != "" {
		cfg.DBPath = value
	}
	if cfg.DBPath == "" || cfg.DBPath == ":memory:" {
		return cfg, errors.New("DB_PATH must name a durable file")
	}
	if value := os.Getenv("ALLOWED_RECIPIENTS"); value != "" {
		recipients = value
	}
	if err := setIntFromEnv("MAX_CONNECTIONS", &cfg.MaxConnections); err != nil {
		return cfg, err
	}
	if err := setIntFromEnv("READ_TIMEOUT_SECONDS", &cfg.ReadDeadlineSec); err != nil {
		return cfg, err
	}
	if err := setIntFromEnv("MAX_LINE_BYTES", &cfg.MaxLineBytes); err != nil {
		return cfg, err
	}
	if err := setIntFromEnv("MAX_RECIPIENTS", &cfg.MaxRecipients); err != nil {
		return cfg, err
	}
	var maxMessage int = int(cfg.MaxMessageBytes)
	if err := setIntFromEnv("MAX_MESSAGE_BYTES", &maxMessage); err != nil {
		return cfg, err
	}
	cfg.MaxMessageBytes = int64(maxMessage)

	for _, item := range strings.Split(recipients, ",") {
		address := strings.TrimSpace(item)
		if address == "" {
			continue
		}
		normalized, err := normalizeRecipient(address)
		if err != nil {
			return cfg, fmt.Errorf("allowed recipient %q: %w", address, err)
		}
		cfg.AllowedMailbox[normalized] = struct{}{}
	}
	if len(cfg.AllowedMailbox) == 0 {
		return cfg, errors.New("ALLOWED_RECIPIENTS must contain at least one address")
	}
	if cfg.MaxConnections < 1 || cfg.ReadDeadlineSec < 1 || cfg.MaxLineBytes < 3 || cfg.MaxRecipients < 1 || cfg.MaxMessageBytes < 1 {
		return cfg, errors.New("limits must be positive and MAX_LINE_BYTES must be at least 3")
	}
	return cfg, nil
}

func parseLoopbackAddr(value, name string) (string, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return "", fmt.Errorf("%s must use a loopback host", name)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("%s has invalid port", name)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, portText), nil
}

func setIntFromEnv(name string, target *int) error {
	value := os.Getenv(name)
	if value == "" {
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = n
	return nil
}
