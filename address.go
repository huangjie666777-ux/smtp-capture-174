package main

import (
	"errors"
	"regexp"
	"strings"
)

var ErrInvalidAddress = errors.New("invalid electronic mail address")

var dotAtomLocal = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-/=?^_\x60{|}~]+(?:\.[A-Za-z0-9!#$%&'*+\-/=?^_\x60{|}~]+)*$`)
var domainLabel = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func normalizeRecipient(address string) (string, error) {
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" || strings.Contains(local, " ") || strings.Contains(domain, " ") {
		return "", ErrInvalidAddress
	}
	if !dotAtomLocal.MatchString(local) || len(local) > 64 {
		return "", ErrInvalidAddress
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", ErrInvalidAddress
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return "", ErrInvalidAddress
		}
	}
	if len(domain) > 253 {
		return "", ErrInvalidAddress
	}
	return local + "@" + strings.ToLower(domain), nil
}

func parseAddressArgument(parameter string) (string, bool, error) {
	if parameter == "" {
		return "", false, ErrInvalidAddress
	}
	if parameter == "<>" {
		return "", true, nil
	}
	rest, ok := strings.CutPrefix(parameter, "<")
	if !ok {
		return "", false, ErrInvalidAddress
	}
	address, ok := strings.CutSuffix(rest, ">")
	if !ok {
		return "", false, ErrInvalidAddress
	}
	normalized, err := normalizeRecipient(address)
	if err != nil {
		return "", false, err
	}
	return normalized, false, nil
}
