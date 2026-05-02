package main

import (
	"path/filepath"
	"testing"
)

func TestBookingCommandFlow(t *testing.T) {
	tempDir := t.TempDir()
	catalogPath := filepath.Join(tempDir, "booking_catalog.json")
	reservationsPath := filepath.Join(tempDir, "booking_reservations.json")
	app := newAppContext()

	exec := func(args ...string) bookingCommandResult {
		t.Helper()
		cmd := newBookingCommand(app)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("booking command %v failed: %v", args, err)
		}
		_, _, resultAny := app.ExecutionSnapshot()
		result, ok := resultAny.(bookingCommandResult)
		if !ok {
			t.Fatalf("expected bookingCommandResult for %v, got %T", args, resultAny)
		}
		return result
	}

	exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"product", "add",
		"--id", "p-1",
		"--name", "Product One",
		"--enabled=true",
	)

	exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"slot", "add",
		"--id", "slot-1",
		"--product-id", "p-1",
		"--start", "2026-05-03T10:00:00+08:00",
		"--end", "2026-05-03T11:00:00+08:00",
		"--capacity", "1",
		"--enabled=true",
	)

	createResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "create",
		"--product-id", "p-1",
		"--slot-id", "slot-1",
		"--user-id", "u-1",
		"--party-size", "1",
		"--contact-name", "Alice",
		"--contact-phone", "13800138000",
		"--member", "Alice",
	)
	if createResult.Reservation == nil {
		t.Fatalf("expected reservation result after create, got %+v", createResult)
	}
	if createResult.Reservation.Status != bookingReservationStatusPending {
		t.Fatalf("expected pending reservation after create, got %+v", createResult.Reservation)
	}

	reservationID := createResult.Reservation.ID
	confirmResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "confirm",
		reservationID,
		"--note", "confirmed",
	)
	if confirmResult.Reservation == nil || confirmResult.Reservation.Status != bookingReservationStatusConfirmed {
		t.Fatalf("expected confirmed reservation result, got %+v", confirmResult)
	}

	listResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "list",
		"--user-id", "u-1",
		"--status", "confirmed",
	)
	if len(listResult.Reservations) != 1 || listResult.Reservations[0].ID != reservationID {
		t.Fatalf("expected confirmed reservation listed, got %+v", listResult.Reservations)
	}

	queryResult := exec(
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"query",
	)
	if queryResult.Query == nil {
		t.Fatalf("expected query result payload, got %+v", queryResult)
	}
	if len(queryResult.Query.Slots) != 0 {
		t.Fatalf("expected query default to hide full slot, got %+v", queryResult.Query.Slots)
	}
}

func TestBookingCommandValidationErrors(t *testing.T) {
	tempDir := t.TempDir()
	catalogPath := filepath.Join(tempDir, "booking_catalog.json")
	reservationsPath := filepath.Join(tempDir, "booking_reservations.json")
	app := newAppContext()

	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"--catalog", catalogPath,
		"--reservations", reservationsPath,
		"reservation", "create",
		"--product-id", "p-1",
		"--slot-id", "slot-1",
		"--user-id", "u-1",
		"--party-size", "1",
	})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("expected reservation create validation error for missing personnel fields")
	}
}

func TestBookingCommandServiceStatus(t *testing.T) {
	tempDir := t.TempDir()
	runtimePath := filepath.Join(tempDir, "booking_runtime.json")
	app := newAppContext()

	cmd := newBookingCommand(app)
	cmd.SetArgs([]string{
		"service", "status",
		"--runtime", runtimePath,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("booking service status command failed: %v", err)
	}

	_, _, resultAny := app.ExecutionSnapshot()
	result, ok := resultAny.(bookingCommandResult)
	if !ok {
		t.Fatalf("expected bookingCommandResult for service status, got %T", resultAny)
	}
	if result.ServiceState == nil {
		t.Fatalf("expected service status payload, got %+v", result)
	}
	if result.ServiceState.Status != "stopped" || result.ServiceState.Running {
		t.Fatalf("expected stopped booking service status, got %+v", result.ServiceState)
	}
}
