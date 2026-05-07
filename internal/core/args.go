package core

import (
	"strings"
)

func NormalizeArgs(args []string) []string {
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

func InferCommandMetadata(args []string) (command string, symbols []string) {
	if len(args) == 0 {
		return "quote", []string{"AAPL.US"}
	}

	first := strings.ToLower(args[0])
	switch first {
	case "quote", "q":
		parsed := ParseSymbols(args[1:])
		if len(parsed) == 0 {
			parsed = []string{"AAPL.US"}
		}
		return "quote", parsed
	case "longbridge", "lb":
		forwarded := append([]string(nil), args[1:]...)
		if len(forwarded) == 0 {
			forwarded = []string{"--help"}
		}
		return "longbridge", forwarded
	case "skill":
		if len(args) >= 2 && strings.EqualFold(strings.TrimSpace(args[1]), "run") {
			return "skill", parseSkillSymbolsFromArgs(args[2:])
		}
		return "skill", nil
	case "sys", "shell":
		return "sys", nil
	default:
		if looksLikeSymbolArg(first) {
			parsed := ParseSymbols(args)
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
	case "quote", "q", "longbridge", "lb", "skill", "email", "mail", "sys", "shell", "admin", "daemon", "d", "task", "webhook", "booking", "version", "upgrade", "help", "completion":
		return true
	default:
		return false
	}
}

func parseSkillSymbolsFromArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	symbolTokens := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		token := strings.TrimSpace(args[index])
		if token == "" {
			continue
		}
		if strings.HasPrefix(token, "--symbols=") {
			symbolTokens = append(symbolTokens, strings.TrimSpace(strings.TrimPrefix(token, "--symbols=")))
			continue
		}
		if token == "--symbols" && index+1 < len(args) {
			index++
			symbolTokens = append(symbolTokens, strings.TrimSpace(args[index]))
		}
	}
	return ParseSymbols(symbolTokens)
}

func looksLikeSymbolArg(arg string) bool {
	return strings.Contains(arg, ".") || strings.Contains(arg, ",")
}

func ParseSymbols(args []string) []string {
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
