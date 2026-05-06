package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chzyer/readline"
)

func TestDaemonCompletionQuoteWithMultipleSymbols(t *testing.T) {
	candidates := completionCandidatesFromLine("quote AAPL.US TSLA.US ")
	if !slices.Contains(candidates, "700.HK") {
		t.Fatalf("expected quote completion to include 700.HK, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailRemainingFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("email send --to alice@example.com --")
	if !slices.Contains(candidates, "--subject") {
		t.Fatalf("expected email completion to include --subject, got: %v", candidates)
	}
	if slices.Contains(candidates, "--to") {
		t.Fatalf("expected used flag --to to be skipped, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailAwaitingValue(t *testing.T) {
	candidates := completionCandidatesFromLine("email send --to ")
	if !slices.Contains(candidates, "recipient@example.com") {
		t.Fatalf("expected email completion to suggest recipient value, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailReceiveSubcommand(t *testing.T) {
	candidates := completionCandidatesFromLine("email ")
	if !slices.Contains(candidates, "receive") {
		t.Fatalf("expected email completion to include receive, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailReceiveFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("email receive --")
	if !slices.Contains(candidates, "--subject-contains") || !slices.Contains(candidates, "--with-files") {
		t.Fatalf("expected email receive completion flags, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailMonitorFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("mail monitor --")
	if !slices.Contains(candidates, "--poll-interval") || !slices.Contains(candidates, "--since-uid") || !slices.Contains(candidates, "--mode") || !slices.Contains(candidates, "--once") {
		t.Fatalf("expected email monitor completion flags, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailMonitorControlSubcommands(t *testing.T) {
	candidates := completionCandidatesFromLine("mail monitor ")
	if !slices.Contains(candidates, "list") || !slices.Contains(candidates, "start") || !slices.Contains(candidates, "stop") || !slices.Contains(candidates, "remove") {
		t.Fatalf("expected email monitor control completions, got: %v", candidates)
	}
}

func TestDaemonCompletionEmailAnalyzeFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("email analyze --")
	if !slices.Contains(candidates, "--with-files") || !slices.Contains(candidates, "--body-max-bytes") {
		t.Fatalf("expected email analyze completion flags, got: %v", candidates)
	}
}

func TestDaemonCompletionMailAliasWithMultipleFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("mail send --to alice@example.com --subject Notice --")
	if !slices.Contains(candidates, "--body") || !slices.Contains(candidates, "--body-file") {
		t.Fatalf("expected email completion to include remaining body flags, got: %v", candidates)
	}
	if slices.Contains(candidates, "--to") || slices.Contains(candidates, "--subject") {
		t.Fatalf("expected used flags to be skipped, got: %v", candidates)
	}
}

func TestDaemonAutoCompleterQuoteAfterMultipleArgs(t *testing.T) {
	completer := newDaemonCompleter()
	line := []rune("quote AAPL.US TSLA.US ")
	suffixes, _ := completer.Do(line, len(line))
	values := runeSlicesToStrings(suffixes)
	if !slices.Contains(values, "700.HK ") {
		t.Fatalf("expected completer to suggest symbol suffix, got: %v", values)
	}
}

func TestDaemonAutoCompleterEmailLaterFlag(t *testing.T) {
	completer := newDaemonCompleter()
	line := []rune("email send --to alice@example.com --")
	suffixes, _ := completer.Do(line, len(line))
	values := runeSlicesToStrings(suffixes)
	if !slices.Contains(values, "subject ") {
		t.Fatalf("expected completer to suggest later flag suffix, got: %v", values)
	}
}

func TestDaemonCompletionPipelineRootAfterPipe(t *testing.T) {
	candidates := completionCandidatesFromLine("quote AAPL.US | ")
	if !slices.Contains(candidates, "email") || !slices.Contains(candidates, "quote") {
		t.Fatalf("expected root command completion after pipe, got: %v", candidates)
	}
	if !slices.Contains(candidates, "task") || !slices.Contains(candidates, "sys") || !slices.Contains(candidates, "admin") {
		t.Fatalf("expected root command completion after pipe to include task/sys/admin, got: %v", candidates)
	}
	if !slices.Contains(candidates, "booking") {
		t.Fatalf("expected root command completion after pipe to include booking, got: %v", candidates)
	}
	if !slices.Contains(candidates, "longbridge") || !slices.Contains(candidates, "lb") {
		t.Fatalf("expected root command completion after pipe to include longbridge/lb, got: %v", candidates)
	}
	if !slices.Contains(candidates, "version") || !slices.Contains(candidates, "upgrade") {
		t.Fatalf("expected root command completion after pipe to include version/upgrade, got: %v", candidates)
	}
	if slices.Contains(candidates, "quit") {
		t.Fatalf("expected quit removed from daemon completion, got: %v", candidates)
	}
}

func TestDaemonAutoCompleterPipelineNextCommand(t *testing.T) {
	completer := newDaemonCompleter()
	line := []rune("quote AAPL.US | em")
	suffixes, _ := completer.Do(line, len(line))
	values := runeSlicesToStrings(suffixes)
	if !slices.Contains(values, "ail ") {
		t.Fatalf("expected completer to suggest email suffix after pipe, got: %v", values)
	}
}

func TestDaemonAutoCompleterPipelineEmailFlags(t *testing.T) {
	completer := newDaemonCompleter()
	line := []rune("quote AAPL.US | email send --to alice@example.com --")
	suffixes, _ := completer.Do(line, len(line))
	values := runeSlicesToStrings(suffixes)
	if !slices.Contains(values, "subject ") {
		t.Fatalf("expected completer to suggest email flags after pipe, got: %v", values)
	}
}

func TestDaemonCompletionTaskSubcommands(t *testing.T) {
	candidates := completionCandidatesFromLine("task ")
	if !slices.Contains(candidates, "add") ||
		!slices.Contains(candidates, "list") ||
		!slices.Contains(candidates, "pause") ||
		!slices.Contains(candidates, "global-pause") ||
		!slices.Contains(candidates, "resume") ||
		!slices.Contains(candidates, "global-resume") ||
		!slices.Contains(candidates, "remove") {
		t.Fatalf("expected task subcommand completion, got: %v", candidates)
	}
}

func TestDaemonCompletionTaskAddScheduleFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("task add ")
	if !slices.Contains(candidates, "--every") || !slices.Contains(candidates, "--cron") || !slices.Contains(candidates, "--auto-resume") {
		t.Fatalf("expected task add completion to include --every/--cron/--auto-resume, got: %v", candidates)
	}
}

func TestDaemonCompletionSystemCommandFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("sys ")
	if !slices.Contains(candidates, "--shell") || !slices.Contains(candidates, "--timeout") {
		t.Fatalf("expected sys completion to include flags, got: %v", candidates)
	}
}

func TestDaemonCompletionAdminFlagsAndSubcommands(t *testing.T) {
	candidates := completionCandidatesFromLine("admin ")
	if !slices.Contains(candidates, "status") || !slices.Contains(candidates, "auth") || !slices.Contains(candidates, "--runtime") {
		t.Fatalf("expected admin completion includes status/runtime, got: %v", candidates)
	}

	authSub := completionCandidatesFromLine("admin auth ")
	if !slices.Contains(authSub, "set-password") ||
		!slices.Contains(authSub, "migrate") ||
		!slices.Contains(authSub, "reset-password") ||
		!slices.Contains(authSub, "verify") ||
		!slices.Contains(authSub, "--config") {
		t.Fatalf("expected admin auth completion includes subcommands/flags, got: %v", authSub)
	}

	setFlags := completionCandidatesFromLine("admin auth set-password --")
	if !slices.Contains(setFlags, "--username") || !slices.Contains(setFlags, "--password") || !slices.Contains(setFlags, "--cost") {
		t.Fatalf("expected admin auth set-password flags, got: %v", setFlags)
	}

	resetFlags := completionCandidatesFromLine("admin auth reset-password --")
	if !slices.Contains(resetFlags, "--generate") || !slices.Contains(resetFlags, "--length") {
		t.Fatalf("expected admin auth reset-password flags, got: %v", resetFlags)
	}
}

func TestDaemonCompletionWebhookSubcommands(t *testing.T) {
	candidates := completionCandidatesFromLine("webhook ")
	if !slices.Contains(candidates, "start") ||
		!slices.Contains(candidates, "status") ||
		!slices.Contains(candidates, "stop") ||
		!slices.Contains(candidates, "kill-port") ||
		!slices.Contains(candidates, "token") ||
		!slices.Contains(candidates, "sign") ||
		!slices.Contains(candidates, "send") ||
		!slices.Contains(candidates, "debug-pipeline") {
		t.Fatalf("expected webhook subcommand completion, got: %v", candidates)
	}
	if slices.Contains(candidates, "serve") {
		t.Fatalf("expected webhook serve hidden from completion candidates, got: %v", candidates)
	}
}

func TestDaemonCompletionWebhookFlags(t *testing.T) {
	startCandidates := completionCandidatesFromLine("webhook start --")
	if !slices.Contains(startCandidates, "--runtime") || !slices.Contains(startCandidates, "--log-file") || !slices.Contains(startCandidates, "--security-keys") {
		t.Fatalf("expected webhook start completion flags, got: %v", startCandidates)
	}

	killPortCandidates := completionCandidatesFromLine("webhook kill-port --")
	if !slices.Contains(killPortCandidates, "--addr") || !slices.Contains(killPortCandidates, "--timeout") || !slices.Contains(killPortCandidates, "--runtime") || !slices.Contains(killPortCandidates, "--dry-run") {
		t.Fatalf("expected webhook kill-port completion flags, got: %v", killPortCandidates)
	}

	signCandidates := completionCandidatesFromLine("webhook sign --")
	if !slices.Contains(signCandidates, "--third-party-id") || !slices.Contains(signCandidates, "--token") || !slices.Contains(signCandidates, "--data-file") {
		t.Fatalf("expected webhook sign completion flags, got: %v", signCandidates)
	}

	sendCandidates := completionCandidatesFromLine("webhook send --")
	if !slices.Contains(sendCandidates, "--url") || !slices.Contains(sendCandidates, "--timestamp") || !slices.Contains(sendCandidates, "--timeout") {
		t.Fatalf("expected webhook send completion flags, got: %v", sendCandidates)
	}

	debugCandidates := completionCandidatesFromLine("webhook debug-pipeline --")
	if !slices.Contains(debugCandidates, "--audit-log") || !slices.Contains(debugCandidates, "--event-id") || !slices.Contains(debugCandidates, "--route-id") {
		t.Fatalf("expected webhook debug-pipeline completion flags, got: %v", debugCandidates)
	}

	tokenCandidates := completionCandidatesFromLine("webhook token ")
	if !slices.Contains(tokenCandidates, "generate") || !slices.Contains(tokenCandidates, "query") || !slices.Contains(tokenCandidates, "reset") {
		t.Fatalf("expected webhook token completion subcommands, got: %v", tokenCandidates)
	}
	tokenFlagCandidates := completionCandidatesFromLine("webhook token generate --")
	if !slices.Contains(tokenFlagCandidates, "--security-keys") || !slices.Contains(tokenFlagCandidates, "--scope") {
		t.Fatalf("expected webhook token generate completion flags, got: %v", tokenFlagCandidates)
	}

	routeCandidates := completionCandidatesFromLine("webhook route ")
	if !slices.Contains(routeCandidates, "add") || !slices.Contains(routeCandidates, "update") || !slices.Contains(routeCandidates, "--routes-file") {
		t.Fatalf("expected webhook route completion candidates, got: %v", routeCandidates)
	}

	routeFlagCandidates := completionCandidatesFromLine("webhook route add --")
	if !slices.Contains(routeFlagCandidates, "--path") || !slices.Contains(routeFlagCandidates, "--pipeline") || !slices.Contains(routeFlagCandidates, "--timeout") {
		t.Fatalf("expected webhook route add flags, got: %v", routeFlagCandidates)
	}
}

func TestDaemonCompletionUpgradeFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("upgrade --")
	if !slices.Contains(candidates, "--version") || !slices.Contains(candidates, "--yes") || !slices.Contains(candidates, "--dry-run") {
		t.Fatalf("expected upgrade completion flags, got: %v", candidates)
	}

	checkCandidates := completionCandidatesFromLine("upgrade check --")
	if !slices.Contains(checkCandidates, "--version") {
		t.Fatalf("expected upgrade check completion flags, got: %v", checkCandidates)
	}
}

func TestDaemonCompletionBookingSubcommandsAndFlags(t *testing.T) {
	candidates := completionCandidatesFromLine("booking ")
	if !slices.Contains(candidates, "product") ||
		!slices.Contains(candidates, "agent") ||
		!slices.Contains(candidates, "slot") ||
		!slices.Contains(candidates, "reservation") ||
		!slices.Contains(candidates, "service") ||
		!slices.Contains(candidates, "query") {
		t.Fatalf("expected booking completion subcommands, got: %v", candidates)
	}
	if !slices.Contains(candidates, "--catalog") || !slices.Contains(candidates, "--reservations") {
		t.Fatalf("expected booking global flags, got: %v", candidates)
	}

	productCandidates := completionCandidatesFromLine("booking product add --")
	if !slices.Contains(productCandidates, "--id") || !slices.Contains(productCandidates, "--name") || !slices.Contains(productCandidates, "--enabled") {
		t.Fatalf("expected booking product flags, got: %v", productCandidates)
	}

	slotCandidates := completionCandidatesFromLine("booking slot list --")
	if !slices.Contains(slotCandidates, "--product-id") || !slices.Contains(slotCandidates, "--from") || !slices.Contains(slotCandidates, "--include-full") {
		t.Fatalf("expected booking slot list flags, got: %v", slotCandidates)
	}

	reservationCandidates := completionCandidatesFromLine("booking reservation create --")
	if !slices.Contains(reservationCandidates, "--party-size") || !slices.Contains(reservationCandidates, "--contact-name") || !slices.Contains(reservationCandidates, "--special-requirements") {
		t.Fatalf("expected booking reservation create flags, got: %v", reservationCandidates)
	}

	queryCandidates := completionCandidatesFromLine("booking query --")
	if !slices.Contains(queryCandidates, "--product-id") || !slices.Contains(queryCandidates, "--from") || !slices.Contains(queryCandidates, "--include-full") {
		t.Fatalf("expected booking query flags, got: %v", queryCandidates)
	}

	serviceCandidates := completionCandidatesFromLine("booking service start --")
	if !slices.Contains(serviceCandidates, "--public-addr") ||
		!slices.Contains(serviceCandidates, "--runtime") ||
		!slices.Contains(serviceCandidates, "--security-keys") ||
		!slices.Contains(serviceCandidates, "--llm-config") {
		t.Fatalf("expected booking service start flags, got: %v", serviceCandidates)
	}

	agentReserveCandidates := completionCandidatesFromLine("booking agent reserve --")
	if !slices.Contains(agentReserveCandidates, "--user-id") ||
		!slices.Contains(agentReserveCandidates, "--content") ||
		!slices.Contains(agentReserveCandidates, "--third-party-id") ||
		!slices.Contains(agentReserveCandidates, "--token") ||
		!slices.Contains(agentReserveCandidates, "--security-keys") ||
		!slices.Contains(agentReserveCandidates, "--url") ||
		!slices.Contains(agentReserveCandidates, "--idempotency-key") ||
		!slices.Contains(agentReserveCandidates, "--slot-id") ||
		!slices.Contains(agentReserveCandidates, "--contact-phone") {
		t.Fatalf("expected booking agent reserve flags, got: %v", agentReserveCandidates)
	}
}

func TestRewriteDaemonWebhookServeToStart(t *testing.T) {
	rewritten, ok := rewriteDaemonWebhookServeToStart([]string{"webhook", "serve", "--addr", ":8081"})
	if !ok {
		t.Fatalf("expected webhook serve command to be rewritten")
	}
	if len(rewritten) < 2 || rewritten[0] != "webhook" || rewritten[1] != "start" {
		t.Fatalf("unexpected rewritten args: %v", rewritten)
	}
	if !slices.Contains(rewritten, "--addr") || !slices.Contains(rewritten, ":8081") {
		t.Fatalf("expected rewritten args to preserve flags, got: %v", rewritten)
	}

	if _, ok := rewriteDaemonWebhookServeToStart([]string{"webhook", "start"}); ok {
		t.Fatalf("expected webhook start not to be rewritten")
	}
}

func TestRewriteDaemonBookingServeToStart(t *testing.T) {
	rewritten, ok := rewriteDaemonBookingServeToStart([]string{"booking", "service", "serve", "--addr", ":18091"})
	if !ok {
		t.Fatalf("expected booking service serve command to be rewritten")
	}
	if len(rewritten) < 3 || rewritten[0] != "booking" || rewritten[1] != "service" || rewritten[2] != "start" {
		t.Fatalf("unexpected rewritten args: %v", rewritten)
	}
	if !slices.Contains(rewritten, "--addr") || !slices.Contains(rewritten, ":18091") {
		t.Fatalf("expected rewritten args to preserve flags, got: %v", rewritten)
	}

	if _, ok := rewriteDaemonBookingServeToStart([]string{"booking", "service", "start"}); ok {
		t.Fatalf("expected booking service start not to be rewritten")
	}
}

func TestTrackDaemonOwnedWebhookLifecycle(t *testing.T) {
	owned := map[int]daemonOwnedWebhookRuntime{}
	sessionID := "daemon-session-1"

	trackDaemonOwnedWebhookLifecycle(
		[]string{"webhook", "start"},
		webhookStartResult{
			Status: "started",
			Runtime: &webhookRuntimeInfo{
				PID:             321,
				OwnerSessionID:  sessionID,
				OwnerStartToken: "owner-token-1",
			},
		},
		owned,
		sessionID,
	)
	if _, ok := owned[321]; !ok {
		t.Fatalf("expected webhook pid tracked after start, got: %v", owned)
	}

	trackDaemonOwnedWebhookLifecycle(
		[]string{"webhook", "start"},
		webhookStartResult{
			Status: "already_running",
			Runtime: &webhookRuntimeInfo{
				PID:             999,
				OwnerSessionID:  sessionID,
				OwnerStartToken: "owner-token-2",
			},
		},
		owned,
		sessionID,
	)
	if len(owned) != 1 {
		t.Fatalf("expected already_running webhook pid not tracked, got: %v", owned)
	}

	trackDaemonOwnedWebhookLifecycle(
		[]string{"webhook", "stop"},
		webhookStopResult{
			Status: "stopped",
			PID:    321,
		},
		owned,
		sessionID,
	)
	if len(owned) != 0 {
		t.Fatalf("expected tracked webhook pids cleared after stop, got: %v", owned)
	}
}

func TestStopDaemonOwnedWebhookOnDaemonExitWithPath(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)
	sessionID := "daemon-session-1"
	startToken := "owner-token-1"

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:             999999,
		Address:         ":8080",
		Path:            "/webhook/events",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  sessionID,
		OwnerStartToken: startToken,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}

	var output bytes.Buffer
	stopDaemonOwnedWebhookOnDaemonExitWithPath(
		map[int]daemonOwnedWebhookRuntime{999999: {StartToken: startToken}},
		runtimePath,
		sessionID,
		&output,
	)

	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if exists {
		t.Fatalf("expected runtime state removed when pid is owned")
	}

	// Re-create runtime and verify non-owned pid won't be stopped/removed.
	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:             999998,
		Address:         ":8080",
		Path:            "/webhook/events",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  sessionID,
		OwnerStartToken: startToken,
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}
	stopDaemonOwnedWebhookOnDaemonExitWithPath(
		map[int]daemonOwnedWebhookRuntime{1: {StartToken: startToken}},
		runtimePath,
		sessionID,
		nil,
	)
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if !exists {
		t.Fatalf("expected runtime state kept when pid is not owned")
	}
}

func TestStopDaemonOwnedWebhookOnDaemonExitSkipsWhenOwnerMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:             999997,
		Address:         ":8080",
		Path:            "/webhook/events",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  "daemon-a",
		OwnerStartToken: "token-a",
	}); err != nil {
		t.Fatalf("write runtime state failed: %v", err)
	}

	var output bytes.Buffer
	stopDaemonOwnedWebhookOnDaemonExitWithPath(
		map[int]daemonOwnedWebhookRuntime{999997: {StartToken: "token-b"}},
		runtimePath,
		"daemon-b",
		&output,
	)
	if _, exists, err := readWebhookRuntimeState(runtimePath); err != nil {
		t.Fatalf("read runtime state failed: %v", err)
	} else if !exists {
		t.Fatalf("expected runtime state kept when owner mismatch")
	}
	if !strings.Contains(output.String(), "skip: not owned by current daemon") {
		t.Fatalf("expected skip message for owner mismatch, got: %s", output.String())
	}
}

func TestTrackDaemonOwnedBookingLifecycle(t *testing.T) {
	owned := map[int]daemonOwnedBookingRuntime{}
	sessionID := "daemon-session-booking-1"

	trackDaemonOwnedBookingLifecycle(
		[]string{"booking", "service", "start"},
		bookingCommandResult{
			ServiceStart: &bookingServiceStartResult{
				Status: "started",
				Runtime: &bookingServiceRuntimeInfo{
					PID:             431,
					OwnerSessionID:  sessionID,
					OwnerStartToken: "booking-owner-token-1",
				},
			},
		},
		owned,
		sessionID,
	)
	if _, ok := owned[431]; !ok {
		t.Fatalf("expected booking pid tracked after start, got: %v", owned)
	}

	trackDaemonOwnedBookingLifecycle(
		[]string{"booking", "service", "start"},
		bookingCommandResult{
			ServiceStart: &bookingServiceStartResult{
				Status: "already_running",
				Runtime: &bookingServiceRuntimeInfo{
					PID:             999,
					OwnerSessionID:  sessionID,
					OwnerStartToken: "booking-owner-token-2",
				},
			},
		},
		owned,
		sessionID,
	)
	if len(owned) != 1 {
		t.Fatalf("expected already_running booking pid not tracked, got: %v", owned)
	}

	trackDaemonOwnedBookingLifecycle(
		[]string{"booking", "service", "stop"},
		bookingCommandResult{
			ServiceStop: &bookingServiceStopResult{
				Status: "stopped",
				PID:    431,
			},
		},
		owned,
		sessionID,
	)
	if len(owned) != 0 {
		t.Fatalf("expected tracked booking pids cleared after stop, got: %v", owned)
	}
}

func TestStopDaemonOwnedBookingOnDaemonExitWithPath(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "booking_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)
	sessionID := "daemon-booking-session-1"
	startToken := "booking-owner-token-1"

	if err := writeBookingRuntimeState(runtimePath, bookingServiceRuntimeInfo{
		PID:             999995,
		Address:         "127.0.0.1:18081",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  sessionID,
		OwnerStartToken: startToken,
	}); err != nil {
		t.Fatalf("write booking runtime failed: %v", err)
	}

	var output bytes.Buffer
	stopDaemonOwnedBookingOnDaemonExitWithPath(
		map[int]daemonOwnedBookingRuntime{999995: {StartToken: startToken}},
		runtimePath,
		sessionID,
		&output,
	)

	if _, exists, err := readBookingRuntimeState(runtimePath); err != nil {
		t.Fatalf("read booking runtime failed: %v", err)
	} else if exists {
		t.Fatalf("expected booking runtime removed when pid is owned")
	}

	if err := writeBookingRuntimeState(runtimePath, bookingServiceRuntimeInfo{
		PID:             999994,
		Address:         "127.0.0.1:18081",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  sessionID,
		OwnerStartToken: startToken,
	}); err != nil {
		t.Fatalf("write booking runtime failed: %v", err)
	}
	stopDaemonOwnedBookingOnDaemonExitWithPath(
		map[int]daemonOwnedBookingRuntime{1: {StartToken: startToken}},
		runtimePath,
		sessionID,
		nil,
	)
	if _, exists, err := readBookingRuntimeState(runtimePath); err != nil {
		t.Fatalf("read booking runtime failed: %v", err)
	} else if !exists {
		t.Fatalf("expected booking runtime kept when pid is not owned")
	}
}

func TestStopDaemonOwnedBookingOnDaemonExitSkipsOwnerMismatchAndOwnerless(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "booking_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)

	if err := writeBookingRuntimeState(runtimePath, bookingServiceRuntimeInfo{
		PID:             999993,
		Address:         "127.0.0.1:18081",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  "daemon-a",
		OwnerStartToken: "token-a",
	}); err != nil {
		t.Fatalf("write booking runtime failed: %v", err)
	}
	var mismatchOut bytes.Buffer
	stopDaemonOwnedBookingOnDaemonExitWithPath(
		map[int]daemonOwnedBookingRuntime{999993: {StartToken: "token-b"}},
		runtimePath,
		"daemon-b",
		&mismatchOut,
	)
	if _, exists, err := readBookingRuntimeState(runtimePath); err != nil {
		t.Fatalf("read booking runtime failed: %v", err)
	} else if !exists {
		t.Fatalf("expected booking runtime kept when owner mismatch")
	}
	if !strings.Contains(mismatchOut.String(), "skip: not owned by current daemon") {
		t.Fatalf("expected owner mismatch skip message, got: %s", mismatchOut.String())
	}

	if err := writeBookingRuntimeState(runtimePath, bookingServiceRuntimeInfo{
		PID:       999992,
		Address:   "127.0.0.1:18081",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write ownerless booking runtime failed: %v", err)
	}
	var ownerlessOut bytes.Buffer
	stopDaemonOwnedBookingOnDaemonExitWithPath(
		map[int]daemonOwnedBookingRuntime{999992: {StartToken: "ignored"}},
		runtimePath,
		"daemon-session",
		&ownerlessOut,
	)
	if _, exists, err := readBookingRuntimeState(runtimePath); err != nil {
		t.Fatalf("read booking runtime failed: %v", err)
	} else if !exists {
		t.Fatalf("expected ownerless booking runtime kept")
	}
	if !strings.Contains(ownerlessOut.String(), "runtime has no owner metadata") {
		t.Fatalf("expected ownerless skip message, got: %s", ownerlessOut.String())
	}
}

func TestPrintDaemonWebhookStartupHintWithPath(t *testing.T) {
	tmpDir := t.TempDir()
	runtimePath := filepath.Join(tmpDir, "webhook_runtime.json")
	nowText := time.Now().Format(time.RFC3339Nano)

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:             os.Getpid(),
		Address:         ":8080",
		Path:            "/webhook/events",
		StartedAt:       nowText,
		UpdatedAt:       nowText,
		OwnerSessionID:  "daemon-session",
		OwnerDaemonPID:  os.Getpid(),
		OwnerClaimedAt:  nowText,
		OwnerStartToken: "owner-token",
	}); err != nil {
		t.Fatalf("write running runtime failed: %v", err)
	}
	var runningOut bytes.Buffer
	printDaemonWebhookStartupHintWithPath(runtimePath, &runningOut)
	runningText := runningOut.String()
	if !strings.Contains(runningText, "running pid=") || !strings.Contains(runningText, "owner=session=daemon-session") {
		t.Fatalf("expected running hint with owner summary, got: %s", runningText)
	}

	if err := writeWebhookRuntimeState(runtimePath, webhookRuntimeInfo{
		PID:       999996,
		Address:   ":8080",
		Path:      "/webhook/events",
		StartedAt: nowText,
		UpdatedAt: nowText,
	}); err != nil {
		t.Fatalf("write stale runtime failed: %v", err)
	}
	var staleOut bytes.Buffer
	printDaemonWebhookStartupHintWithPath(runtimePath, &staleOut)
	staleText := staleOut.String()
	if !strings.Contains(staleText, "stale") || !strings.Contains(staleText, "webhook stop") {
		t.Fatalf("expected stale hint text, got: %s", staleText)
	}

	if err := os.Remove(runtimePath); err != nil {
		t.Fatalf("remove runtime path failed: %v", err)
	}
	var stoppedOut bytes.Buffer
	printDaemonWebhookStartupHintWithPath(runtimePath, &stoppedOut)
	if strings.TrimSpace(stoppedOut.String()) != "" {
		t.Fatalf("expected no hint for stopped status, got: %s", stoppedOut.String())
	}
}

func TestDaemonCompletionTaskActionIDs(t *testing.T) {
	oldProvider := daemonTaskIDCandidates
	daemonTaskIDCandidates = func() []string { return []string{"task-1", "task-2"} }
	defer func() {
		daemonTaskIDCandidates = oldProvider
	}()

	for _, line := range []string{"task pause ", "task resume ", "task remove "} {
		candidates := completionCandidatesFromLine(line)
		if !slices.Contains(candidates, "task-1") || !slices.Contains(candidates, "task-2") {
			t.Fatalf("expected task id completion for %q, got: %v", line, candidates)
		}
	}
}

func TestDaemonCompletionMonitorSubcommands(t *testing.T) {
	candidates := completionCandidatesFromLine("monitor ")
	if !slices.Contains(candidates, "list") || !slices.Contains(candidates, "start") || !slices.Contains(candidates, "stop") || !slices.Contains(candidates, "remove") {
		t.Fatalf("expected monitor completion to include list/stop, got: %v", candidates)
	}
}

func TestDaemonCompletionMonitorStopIDs(t *testing.T) {
	oldProvider := daemonMonitorIDCandidates
	daemonMonitorIDCandidates = func() []string { return []string{"1", "2"} }
	defer func() {
		daemonMonitorIDCandidates = oldProvider
	}()

	candidates := completionCandidatesFromLine("monitor stop ")
	if !slices.Contains(candidates, "all") || !slices.Contains(candidates, "1") || !slices.Contains(candidates, "2") {
		t.Fatalf("expected monitor stop completion to include all and monitor ids, got: %v", candidates)
	}
}

func TestDaemonCompletionMonitorRemoveConfirmID(t *testing.T) {
	candidates := completionCandidatesFromLine("monitor remove m-9 ")
	if len(candidates) != 1 || candidates[0] != "m-9" {
		t.Fatalf("expected monitor remove confirmation completion for m-9, got: %v", candidates)
	}
}

func TestDaemonCompletionTaskRemoveConfirmID(t *testing.T) {
	candidates := completionCandidatesFromLine("task remove task-9 ")
	if len(candidates) != 1 || candidates[0] != "task-9" {
		t.Fatalf("expected remove confirmation completion for task-9, got: %v", candidates)
	}
}

func TestIsTaskListPipelineLine(t *testing.T) {
	if !isTaskListPipelineLine("task list | email send --to a@example.com --subject X") {
		t.Fatalf("expected task list pipeline to be detected")
	}
	if !isTaskListPipelineLine("task ls | email send --to a@example.com --subject X") {
		t.Fatalf("expected task ls pipeline to be detected")
	}
	if isTaskListPipelineLine("task add --every 1m -- quote AAPL.US | email send --to a@example.com --subject X") {
		t.Fatalf("expected task add schedule command not to be treated as task list pipeline")
	}
}

func TestExecuteTaskPipelineStageList(t *testing.T) {
	manager := newDaemonTaskManager(func(ctx context.Context, commands [][]string) error {
		return nil
	})
	defer manager.Close()

	_, err := manager.addTask(context.Background(), time.Hour, "quote AAPL.US", [][]string{{"quote", "AAPL.US"}})
	if err != nil {
		t.Fatalf("addTask failed: %v", err)
	}

	result, err := executeTaskPipelineStage(manager, []string{"task", "list"})
	if err != nil {
		t.Fatalf("executeTaskPipelineStage list failed: %v", err)
	}

	data, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if _, ok := data["tasks"]; !ok {
		t.Fatalf("expected tasks key in result")
	}
}

func TestParsePipelineCommandLine(t *testing.T) {
	commands, err := parsePipelineCommandLine(`quote AAPL.US TSLA.US | email send --to a@example.com --subject "Price Alert"`)
	if err != nil {
		t.Fatalf("parse pipeline failed: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(commands))
	}
	if commands[0][0] != "quote" {
		t.Fatalf("expected first command to be quote, got %v", commands[0])
	}
	if commands[1][0] != "email" || commands[1][1] != "send" {
		t.Fatalf("expected second command to be email send, got %v", commands[1])
	}
}

func TestParsePipelineCommandLinePipeInsideQuotes(t *testing.T) {
	commands, err := parsePipelineCommandLine(`quote AAPL.US | email send --to a@example.com --subject "A|B"`)
	if err != nil {
		t.Fatalf("parse pipeline with quoted pipe failed: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(commands))
	}
	if !slices.Contains(commands[1], "A|B") {
		t.Fatalf("expected quoted pipe value to be preserved, got %v", commands[1])
	}
}

func TestParsePipelineCommandLineEscapedPipe(t *testing.T) {
	commands, err := parsePipelineCommandLine(`email send --to a@example.com --subject A\|B`)
	if err != nil {
		t.Fatalf("parse escaped pipe failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}
	if !slices.Contains(commands[0], "A|B") {
		t.Fatalf("expected escaped pipe to be parsed as literal, got %v", commands[0])
	}
}

func TestParsePipelineCommandLineEmptyStage(t *testing.T) {
	if _, err := parsePipelineCommandLine(`quote AAPL.US || email send --to a@example.com --subject X`); err == nil {
		t.Fatalf("expected empty stage error for double pipe")
	}
}

func TestPreparePipelineArgsInjectEmailBody(t *testing.T) {
	prev := &daemonPipelineStage{
		commandLine: "quote AAPL.US",
		result: struct {
			Message string `json:"message"`
		}{
			Message: "ok",
		},
	}

	args := preparePipelineArgs(
		[]string{"email", "send", "--to", "a@example.com", "--subject", "Notice"},
		prev,
	)

	bodyIndex := slices.Index(args, "--body")
	if bodyIndex < 0 || bodyIndex+1 >= len(args) {
		t.Fatalf("expected injected --body argument, got %v", args)
	}
	body := args[bodyIndex+1]
	if !strings.Contains(body, "source_command: quote AAPL.US") {
		t.Fatalf("expected source command in injected body, got %q", body)
	}
	if !strings.Contains(body, `"message": "ok"`) {
		t.Fatalf("expected result json in injected body, got %q", body)
	}
}

func TestPreparePipelineArgsKeepExplicitBody(t *testing.T) {
	prev := &daemonPipelineStage{
		commandLine: "quote AAPL.US",
		result:      map[string]any{"price": "123"},
	}
	args := preparePipelineArgs(
		[]string{"email", "send", "--to", "a@example.com", "--subject", "Notice", "--body", "manual"},
		prev,
	)
	if slices.Index(args, "--body") != len(args)-2 {
		t.Fatalf("expected explicit body to be kept, got %v", args)
	}
	if args[len(args)-1] != "manual" {
		t.Fatalf("expected explicit body value to stay unchanged, got %v", args)
	}
}

func TestPreparePipelineArgsInjectAnalyzeInputJSON(t *testing.T) {
	prev := &daemonPipelineStage{
		commandLine: "mail monitor --wait-timeout 1m",
		result: map[string]any{
			"messages": []map[string]any{
				{"uid": 101, "subject": "hello", "body_text": "<p>hello</p>"},
			},
		},
	}

	args := preparePipelineArgs([]string{"mail", "analyze"}, prev)
	index := slices.Index(args, "--input-json")
	if index < 0 || index+1 >= len(args) {
		t.Fatalf("expected injected --input-json argument, got %v", args)
	}
	if !strings.Contains(args[index+1], "mail monitor") {
		t.Fatalf("expected source command in input json, got %q", args[index+1])
	}
	if strings.Contains(args[index+1], `\u003c`) {
		t.Fatalf("expected input json without html escape, got %q", args[index+1])
	}
}

func TestBuildDaemonPipelineInput(t *testing.T) {
	prev := &daemonPipelineStage{
		commandLine: "mail monitor --wait-timeout 1m",
		result: map[string]any{
			"matched_count": 1,
			"messages": []map[string]any{
				{"uid": 101, "subject": "hello", "body_text": "<p>hello</p>"},
			},
		},
	}

	input, ok := buildDaemonPipelineInput(prev)
	if !ok {
		t.Fatalf("expected pipeline input to be built")
	}
	if input.SourceCommand != prev.commandLine {
		t.Fatalf("unexpected source command: %q", input.SourceCommand)
	}
	if !strings.Contains(input.ResultJSON, `"matched_count":1`) {
		t.Fatalf("expected result json payload, got %q", input.ResultJSON)
	}
	if !strings.Contains(input.InputJSON, `"source_command":"mail monitor --wait-timeout 1m"`) {
		t.Fatalf("expected wrapped input json payload, got %q", input.InputJSON)
	}
	if strings.Contains(input.ResultJSON, `\u003c`) || strings.Contains(input.InputJSON, `\u003c`) {
		t.Fatalf("expected pipeline json without html escape, got result=%q input=%q", input.ResultJSON, input.InputJSON)
	}
}

func TestBuildDaemonPipelineInputMarshalFallback(t *testing.T) {
	prev := &daemonPipelineStage{
		commandLine: "quote AAPL.US",
		result: map[string]any{
			"bad": make(chan int),
		},
	}

	input, ok := buildDaemonPipelineInput(prev)
	if !ok {
		t.Fatalf("expected pipeline input to be built")
	}
	if input.ResultJSON != "null" {
		t.Fatalf("expected result json fallback null, got %q", input.ResultJSON)
	}
	if !strings.Contains(input.InputJSON, `"result_error":`) {
		t.Fatalf("expected wrapped input json fallback error, got %q", input.InputJSON)
	}
}

func TestDaemonPipelineInputContextRoundTrip(t *testing.T) {
	ctx := withDaemonPipelineInput(context.Background(), daemonPipelineInput{
		SourceCommand: "quote AAPL.US",
		ResultJSON:    `{"price":123}`,
		InputJSON:     `{"source_command":"quote AAPL.US","result":{"price":123}}`,
	})

	input, ok := daemonPipelineInputFromContext(ctx)
	if !ok {
		t.Fatalf("expected context to contain pipeline input")
	}
	if input.SourceCommand != "quote AAPL.US" {
		t.Fatalf("unexpected context source command: %q", input.SourceCommand)
	}
	if input.ResultJSON != `{"price":123}` {
		t.Fatalf("unexpected context result json: %q", input.ResultJSON)
	}
	if _, ok := daemonPipelineInputFromContext(context.Background()); ok {
		t.Fatalf("did not expect pipeline input in empty context")
	}
}

func TestBuildPipelineEmailBodyWithoutHTMLEscape(t *testing.T) {
	body := buildPipelineEmailBody("mail monitor --mode hybrid", map[string]any{
		"messages": []map[string]any{
			{"body_text": "<p>hello</p>"},
		},
	})
	if strings.Contains(body, `\u003c`) {
		t.Fatalf("expected email body json without html escape, got %q", body)
	}
	if !strings.Contains(body, "<p>hello</p>") {
		t.Fatalf("expected html tag kept readable, got %q", body)
	}
}

func TestBuildPipelineEmailBodyUsesWebhookRawBody(t *testing.T) {
	body := buildPipelineEmailBody("webhook route sayhelloMail", map[string]any{
		"raw_body": "{\n  \"hello\": \"world\"\n}",
		"data": map[string]any{
			"hello": "world",
		},
	})
	if body != "{\n  \"hello\": \"world\"\n}" {
		t.Fatalf("expected raw webhook body to be used as email body, got %q", body)
	}
}

func TestShouldStopPipelineAfterStageMonitorStopped(t *testing.T) {
	stop := shouldStopPipelineAfterStage(
		[]string{"mail", "monitor", "--once"},
		map[string]any{
			"mode":           "hybrid",
			"mailbox":        "INBOX",
			"total_detected": 0,
			"stopped":        true,
		},
	)
	if !stop {
		t.Fatalf("expected monitor stopped result to stop pipeline")
	}
}

func TestShouldStopPipelineAfterStageMonitorHasResult(t *testing.T) {
	stop := shouldStopPipelineAfterStage(
		[]string{"mail", "monitor", "--once"},
		map[string]any{
			"messages": []map[string]any{
				{"uid": 1},
			},
			"matched_count": 1,
		},
	)
	if stop {
		t.Fatalf("expected pipeline to continue when monitor returns messages")
	}
}

func TestShouldStopPipelineAfterStageNonMonitor(t *testing.T) {
	stop := shouldStopPipelineAfterStage(
		[]string{"quote", "AAPL.US"},
		map[string]any{"stopped": true},
	)
	if stop {
		t.Fatalf("expected non-monitor stage not to stop pipeline")
	}
}

func TestIsEmailMonitorCommand(t *testing.T) {
	if !isEmailMonitorCommand([]string{"mail", "monitor"}) {
		t.Fatalf("expected mail monitor to be recognized")
	}
	if !isEmailMonitorCommand([]string{"email", "watch"}) {
		t.Fatalf("expected email watch to be recognized")
	}
	if isEmailMonitorCommand([]string{"mail", "monitor", "list"}) {
		t.Fatalf("mail monitor list should be control command, not monitor runner")
	}
	if isEmailMonitorCommand([]string{"mail", "receive"}) {
		t.Fatalf("mail receive should not be recognized as monitor")
	}
}

func TestNormalizeEmailMonitorControlCommand(t *testing.T) {
	normalized, ok := normalizeEmailMonitorControlCommand([]string{"mail", "monitor", "stop", "3"})
	if !ok {
		t.Fatalf("expected mail monitor stop to be normalized")
	}
	if len(normalized) != 3 || normalized[0] != "monitor" || normalized[1] != "stop" || normalized[2] != "3" {
		t.Fatalf("unexpected normalized args: %v", normalized)
	}
}

func TestBuildEmailMonitorIdentityKey(t *testing.T) {
	key := buildEmailMonitorIdentityKey([]string{
		"mail", "monitor",
		"--mail-alias", "work",
		"--mailbox", "INBOX",
		"--subject-contains", "ALERT",
		"--from-contains=ops@example.com",
		"--unread-only=false",
		"--wait-timeout", "5m",
	})
	if !strings.Contains(key, "mail_alias=work") || !strings.Contains(key, "mailbox=inbox") || !strings.Contains(key, "subject=alert") || !strings.Contains(key, "unread=false") {
		t.Fatalf("unexpected monitor identity key: %q", key)
	}
}

func TestParseDaemonMonitorRuntimeID(t *testing.T) {
	id, ok := parseDaemonMonitorRuntimeID("run-7")
	if !ok || id != 7 {
		t.Fatalf("expected run-7 parse to 7, got id=%d ok=%v", id, ok)
	}
	if _, ok := parseDaemonMonitorRuntimeID("abc"); ok {
		t.Fatalf("expected invalid runtime id parse failure")
	}
}

func TestParseDaemonMonitorPersistentID(t *testing.T) {
	id, ok := parseDaemonMonitorPersistentID("m-9")
	if !ok || id != 9 {
		t.Fatalf("expected m-9 parse to 9, got id=%d ok=%v", id, ok)
	}
	if _, ok := parseDaemonMonitorPersistentID("monitor-9"); ok {
		t.Fatalf("expected invalid persistent id parse failure")
	}
}

func TestEnsureMonitorIDArg(t *testing.T) {
	args := []string{"mail", "monitor", "--once", "--monitor-id", "old"}
	got := ensureMonitorIDArg(args, "m-1")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--monitor-id m-1") {
		t.Fatalf("expected monitor-id replaced, got %v", got)
	}
	if strings.Contains(joined, "old") {
		t.Fatalf("expected old monitor-id removed, got %v", got)
	}
}

func TestStripMonitorOnceArgs(t *testing.T) {
	args := []string{"mail", "monitor", "--once", "--mode", "hybrid", "--once=true", "--wait-timeout", "10m"}
	got := stripMonitorOnceArgs(args)
	joined := strings.Join(got, " ")
	if strings.Contains(strings.ToLower(joined), "--once") {
		t.Fatalf("expected once flags removed, got %v", got)
	}
	if !strings.Contains(joined, "--mode hybrid") || !strings.Contains(joined, "--wait-timeout 10m") {
		t.Fatalf("expected other args kept, got %v", got)
	}
}

func TestHandleDaemonMonitorControlCommandStopByID(t *testing.T) {
	var (
		monitorMu      sync.Mutex
		monitors       = map[int]*daemonMonitorRuntime{}
		monitorRecords = map[string]daemonMonitorRecord{}
		called         bool
	)
	monitors[3] = &daemonMonitorRuntime{
		id:          3,
		key:         "mailbox=inbox|unread=true|subject=|from=",
		commandLine: "mail monitor --wait-timeout 1m",
		startedAt:   time.Unix(1, 0).UTC(),
		cancel: func() {
			called = true
		},
	}
	monitorRecords["mailbox=inbox|unread=true|subject=|from="] = daemonMonitorRecord{
		Key:         "mailbox=inbox|unread=true|subject=|from=",
		CommandLine: "mail monitor --wait-timeout 1m",
		CreatedAt:   "2026-04-29T11:00:00Z",
	}
	saveFn := func() error { return nil }
	startOne := func(string) error { return nil }
	startAll := func() (int, error) { return 0, nil }
	handled, err := handleDaemonMonitorControlCommand([]string{"monitor", "stop", "3"}, &monitorMu, monitors, monitorRecords, saveFn, startOne, startAll)
	if !handled || err != nil {
		t.Fatalf("expected handled monitor stop, err=%v", err)
	}
	if !called {
		t.Fatalf("expected monitor cancel function to be called")
	}
	if _, exists := monitors[3]; exists {
		t.Fatalf("expected monitor entry removed after stop")
	}
	if len(monitorRecords) != 1 {
		t.Fatalf("expected persisted monitor record kept after stop")
	}
	record := monitorRecords["mailbox=inbox|unread=true|subject=|from="]
	if !record.Paused {
		t.Fatalf("expected monitor record to be paused after stop")
	}
}

func TestHandleDaemonMonitorControlCommandStartByID(t *testing.T) {
	var (
		monitorMu      sync.Mutex
		monitors       = map[int]*daemonMonitorRuntime{}
		monitorRecords = map[string]daemonMonitorRecord{
			"mailbox=inbox|unread=true|subject=|from=": {
				ID:          "m-1",
				Key:         "mailbox=inbox|unread=true|subject=|from=",
				CommandLine: "mail monitor --wait-timeout 1m",
				Paused:      true,
			},
		}
		started string
	)
	saveFn := func() error { return nil }
	startOne := func(id string) error {
		started = id
		return nil
	}
	startAll := func() (int, error) { return 0, nil }

	handled, err := handleDaemonMonitorControlCommand([]string{"monitor", "start", "m-1"}, &monitorMu, monitors, monitorRecords, saveFn, startOne, startAll)
	if !handled || err != nil {
		t.Fatalf("expected handled monitor start, err=%v", err)
	}
	if started != "m-1" {
		t.Fatalf("expected start callback with m-1, got %q", started)
	}
}

func TestHandleDaemonMonitorControlCommandRemoveByIDConfirm(t *testing.T) {
	var (
		monitorMu      sync.Mutex
		monitors       = map[int]*daemonMonitorRuntime{}
		monitorRecords = map[string]daemonMonitorRecord{
			"mailbox=inbox|unread=true|subject=|from=": {
				ID:          "m-1",
				Key:         "mailbox=inbox|unread=true|subject=|from=",
				CommandLine: "mail monitor --wait-timeout 1m",
				Paused:      true,
			},
		}
	)
	saveFn := func() error { return nil }
	startOne := func(string) error { return nil }
	startAll := func() (int, error) { return 0, nil }

	handled, err := handleDaemonMonitorControlCommand([]string{"monitor", "remove", "m-1", "m-1"}, &monitorMu, monitors, monitorRecords, saveFn, startOne, startAll)
	if !handled || err != nil {
		t.Fatalf("expected handled monitor remove, err=%v", err)
	}
	if len(monitorRecords) != 0 {
		t.Fatalf("expected monitor record removed")
	}
}

func TestHandleDaemonMonitorControlCommandRemoveConfirmMismatch(t *testing.T) {
	var (
		monitorMu      sync.Mutex
		monitors       = map[int]*daemonMonitorRuntime{}
		monitorRecords = map[string]daemonMonitorRecord{
			"mailbox=inbox|unread=true|subject=|from=": {
				ID:          "m-1",
				Key:         "mailbox=inbox|unread=true|subject=|from=",
				CommandLine: "mail monitor --wait-timeout 1m",
				Paused:      true,
			},
		}
	)
	saveFn := func() error { return nil }
	startOne := func(string) error { return nil }
	startAll := func() (int, error) { return 0, nil }

	handled, err := handleDaemonMonitorControlCommand([]string{"monitor", "remove", "m-1", "m-2"}, &monitorMu, monitors, monitorRecords, saveFn, startOne, startAll)
	if !handled || err == nil {
		t.Fatalf("expected remove confirmation mismatch error")
	}
}

func TestFormatDaemonMonitorListOutput(t *testing.T) {
	text := formatDaemonMonitorListOutput([]daemonMonitorSnapshot{
		{MonitorID: "m-2", RuntimeID: 2, Key: "mailbox=inbox|unread=true|subject=|from=", CommandLine: "mail monitor --once", StartedAt: "2026-04-29T12:00:00Z", Status: "running"},
	})
	if strings.Contains(text, "run-2") {
		t.Fatalf("unexpected runtime id in list output: %q", text)
	}
	if !strings.Contains(text, "m-2") || !strings.Contains(text, "mail monitor --once") {
		t.Fatalf("unexpected monitor list output: %q", text)
	}
}

func TestListDaemonMonitorsIncludesPausedRecords(t *testing.T) {
	var monitorMu sync.Mutex
	monitors := map[int]*daemonMonitorRuntime{}
	records := map[string]daemonMonitorRecord{
		"mailbox=inbox|unread=true|subject=|from=": {
			ID:          "m-1",
			Key:         "mailbox=inbox|unread=true|subject=|from=",
			CommandLine: "mail monitor --mailbox INBOX",
			CreatedAt:   "2026-04-29T12:00:00Z",
			Paused:      true,
		},
	}
	snapshots := listDaemonMonitors(&monitorMu, monitors, records)
	if len(snapshots) != 1 {
		t.Fatalf("expected one snapshot, got %d", len(snapshots))
	}
	if snapshots[0].Status != "paused" {
		t.Fatalf("expected paused status, got %q", snapshots[0].Status)
	}
}

func TestWriteAndReadDaemonMonitorState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "daemon_monitors.json")
	input := map[string]daemonMonitorRecord{
		"mailbox=inbox|unread=true|subject=|from=": {
			ID:          "m-1",
			Key:         "mailbox=inbox|unread=true|subject=|from=",
			CommandLine: "mail monitor --wait-timeout 1m",
			CreatedAt:   "2026-04-29T11:00:00Z",
			Paused:      true,
		},
	}
	if err := writeDaemonMonitorState(path, input, 2); err != nil {
		t.Fatalf("writeDaemonMonitorState failed: %v", err)
	}
	got, nextID, err := readDaemonMonitorState(path)
	if err != nil {
		t.Fatalf("readDaemonMonitorState failed: %v", err)
	}
	if nextID != 2 {
		t.Fatalf("expected nextID=2, got %d", nextID)
	}
	if len(got) != 1 {
		t.Fatalf("expected one monitor record, got %d", len(got))
	}
	if got["mailbox=inbox|unread=true|subject=|from="].CommandLine != "mail monitor --wait-timeout 1m" {
		t.Fatalf("unexpected command line in restored state: %+v", got)
	}
	if !got["mailbox=inbox|unread=true|subject=|from="].Paused {
		t.Fatalf("expected paused field restored")
	}
}

func TestReadDaemonMonitorStateInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf", "daemon_monitors.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid json failed: %v", err)
	}
	if _, _, err := readDaemonMonitorState(path); err == nil {
		t.Fatalf("expected invalid json error")
	}
}

func TestFinalizeDaemonExit(t *testing.T) {
	app := NewAppContext()
	if err := finalizeDaemonExit(app, "stopped"); err != nil {
		t.Fatalf("finalize daemon exit failed: %v", err)
	}

	command, symbols, result := app.ExecutionSnapshot()
	if command != "daemon" {
		t.Fatalf("expected command to be daemon, got %q", command)
	}
	if len(symbols) != 0 {
		t.Fatalf("expected no symbols on daemon exit, got %v", symbols)
	}

	state, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected daemon state result map, got %T", result)
	}
	if state["mode"] != "daemon" || state["state"] != "stopped" {
		t.Fatalf("unexpected daemon state result: %v", state)
	}
}

func completionCandidatesFromLine(line string) []string {
	segments, _ := readline.SplitSegment([]rune(line), len([]rune(line)))
	runeCandidates := daemonCompletionCandidates(segments)
	return runeSlicesToStrings(runeCandidates)
}

func runeSlicesToStrings(values [][]rune) []string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, string(value))
	}
	return items
}
