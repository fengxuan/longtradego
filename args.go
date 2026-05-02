package main

import (
	"strings"
)

func normalizeArgs(args []string) []string {
	if len(args) == 0 {
		return []string{"quote"}
	}

	first := strings.ToLower(args[0])
	if isKnownCommandName(first) || strings.HasPrefix(first, "-") {
		return append([]string(nil), args...)
	}

	if looksLikeSymbolArg(first) {
		normalized := make([]string, 0, len(args)+1)
		normalized = append(normalized, "quote")
		normalized = append(normalized, args...)
		return normalized
	}

	return append([]string(nil), args...)
}

func inferCommandMetadata(args []string) (command string, symbols []string) {
	if len(args) == 0 {
		return "quote", []string{"AAPL.US"}
	}

	first := strings.ToLower(args[0])
	switch first {
	case "quote", "q":
		parsed := parseSymbols(args[1:])
		if len(parsed) == 0 {
			parsed = []string{"AAPL.US"}
		}
		return "quote", parsed
	case "sys", "shell":
		return "sys", nil
	default:
		if looksLikeSymbolArg(first) {
			parsed := parseSymbols(args)
			if len(parsed) == 0 {
				parsed = []string{"AAPL.US"}
			}
			return "quote", parsed
		}
		return first, nil
	}
}

func isKnownCommandName(name string) bool {
	switch name {
	case "quote", "q", "email", "mail", "sys", "shell", "admin", "daemon", "d", "task", "webhook", "booking", "version", "upgrade", "help", "completion":
		return true
	default:
		return false
	}
}

func looksLikeSymbolArg(arg string) bool {
	return strings.Contains(arg, ".") || strings.Contains(arg, ",")
}

func parseSymbols(args []string) []string {
	symbols := make([]string, 0, len(args))
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			symbol := strings.TrimSpace(part)
			if symbol == "" {
				continue
			}
			symbols = append(symbols, symbol)
		}
	}
	return symbols
}
