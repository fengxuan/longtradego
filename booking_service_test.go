package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBookingServiceLifecycleAndCapacity(t *testing.T) {
	service := newBookingService(
		filepath.Join(t.TempDir(), "booking_catalog.json"),
		filepath.Join(t.TempDir(), "booking_reservations.json"),
	)

	trueValue := true
	capacityValue := 2
	if _, err := service.UpsertProduct(bookingProductUpsertInput{
		ID:      "p-1",
		Name:    "Product One",
		Enabled: &trueValue,
	}); err != nil {
		t.Fatalf("UpsertProduct failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-1",
		ProductID: "p-1",
		StartAt:   "2026-05-03T10:00:00+08:00",
		EndAt:     "2026-05-03T11:00:00+08:00",
		Capacity:  &capacityValue,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("UpsertSlot failed: %v", err)
	}

	reservation, err := service.CreateReservation(bookingReservationCreateInput{
		ProductID: "p-1",
		SlotID:    "slot-1",
		UserID:    "u-1",
		PartySize: 2,
		Personnel: bookingReservationPersonnel{
			ContactName:  "Alice",
			ContactPhone: "13800138000",
			Members:      []string{"Alice", "Bob"},
		},
	})
	if err != nil {
		t.Fatalf("CreateReservation failed: %v", err)
	}
	if reservation.Status != bookingReservationStatusPending {
		t.Fatalf("expected pending reservation, got %+v", reservation)
	}

	confirmed, err := service.ConfirmReservation(reservation.ID, "confirmed")
	if err != nil {
		t.Fatalf("ConfirmReservation failed: %v", err)
	}
	if confirmed.Status != bookingReservationStatusConfirmed {
		t.Fatalf("expected confirmed reservation, got %+v", confirmed)
	}

	_, err = service.CreateReservation(bookingReservationCreateInput{
		ProductID: "p-1",
		SlotID:    "slot-1",
		UserID:    "u-2",
		PartySize: 1,
		Personnel: bookingReservationPersonnel{
			ContactName:  "Tom",
			ContactPhone: "13800138001",
		},
	})
	if err == nil || !isBookingConflictError(err) {
		t.Fatalf("expected capacity conflict error, got %v", err)
	}

	queryDefault, err := service.QueryCatalog(bookingCatalogQuery{})
	if err != nil {
		t.Fatalf("QueryCatalog default failed: %v", err)
	}
	if len(queryDefault.Slots) != 0 {
		t.Fatalf("expected full slot hidden by default, got %+v", queryDefault.Slots)
	}

	queryAll, err := service.QueryCatalog(bookingCatalogQuery{IncludeFull: true})
	if err != nil {
		t.Fatalf("QueryCatalog include_full failed: %v", err)
	}
	if len(queryAll.Slots) != 1 || queryAll.Slots[0].AvailableCapacity != 0 {
		t.Fatalf("expected include_full to show slot with available=0, got %+v", queryAll.Slots)
	}
}

func TestBookingServiceStatusTransitionsValidation(t *testing.T) {
	service := newBookingService(
		filepath.Join(t.TempDir(), "booking_catalog.json"),
		filepath.Join(t.TempDir(), "booking_reservations.json"),
	)
	trueValue := true
	capacityValue := 3
	if _, err := service.UpsertProduct(bookingProductUpsertInput{
		ID:      "p-2",
		Name:    "Product Two",
		Enabled: &trueValue,
	}); err != nil {
		t.Fatalf("UpsertProduct failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-2",
		ProductID: "p-2",
		StartAt:   "2026-05-04T10:00:00+08:00",
		EndAt:     "2026-05-04T11:00:00+08:00",
		Capacity:  &capacityValue,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("UpsertSlot failed: %v", err)
	}

	reservation, err := service.CreateReservation(bookingReservationCreateInput{
		ProductID: "p-2",
		SlotID:    "slot-2",
		UserID:    "u-3",
		PartySize: 1,
		Personnel: bookingReservationPersonnel{
			ContactName:  "Ken",
			ContactPhone: "13800138002",
		},
	})
	if err != nil {
		t.Fatalf("CreateReservation failed: %v", err)
	}
	if _, err := service.RejectReservation(reservation.ID, "rejected"); err != nil {
		t.Fatalf("RejectReservation failed: %v", err)
	}
	if _, err := service.ConfirmReservation(reservation.ID, "should-fail"); err == nil || !isBookingValidationError(err) {
		t.Fatalf("expected validation error for non-pending confirm, got %v", err)
	}
}

func TestBookingServiceRFC3339Validation(t *testing.T) {
	service := newBookingService(
		filepath.Join(t.TempDir(), "booking_catalog.json"),
		filepath.Join(t.TempDir(), "booking_reservations.json"),
	)

	trueValue := true
	if _, err := service.UpsertProduct(bookingProductUpsertInput{
		ID:      "p-3",
		Name:    "Product Three",
		Enabled: &trueValue,
	}); err != nil {
		t.Fatalf("UpsertProduct failed: %v", err)
	}

	capacityValue := 2
	_, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-bad",
		ProductID: "p-3",
		StartAt:   "2026/05/05 10:00",
		EndAt:     "2026-05-05T11:00:00+08:00",
		Capacity:  &capacityValue,
		Enabled:   &trueValue,
	})
	if err == nil || !isBookingValidationError(err) {
		t.Fatalf("expected RFC3339 validation error, got %v", err)
	}

	from, err := parseBookingRFC3339("2026-05-06T12:00:00+08:00", "from")
	if err != nil {
		t.Fatalf("parseBookingRFC3339 from failed: %v", err)
	}
	to, err := parseBookingRFC3339("2026-05-06T10:00:00+08:00", "to")
	if err != nil {
		t.Fatalf("parseBookingRFC3339 to failed: %v", err)
	}
	_, err = service.QueryCatalog(bookingCatalogQuery{From: &from, To: &to})
	if err == nil || !isBookingValidationError(err) {
		t.Fatalf("expected query time range validation error, got %v", err)
	}
}

func TestBookingServiceQueryDefaultsToEnabledAndAvailable(t *testing.T) {
	service := newBookingService(
		filepath.Join(t.TempDir(), "booking_catalog.json"),
		filepath.Join(t.TempDir(), "booking_reservations.json"),
	)

	trueValue := true
	falseValue := false
	capacityValue := 1

	if _, err := service.UpsertProduct(bookingProductUpsertInput{ID: "p-enabled", Name: "Enabled", Enabled: &trueValue}); err != nil {
		t.Fatalf("upsert enabled product failed: %v", err)
	}
	if _, err := service.UpsertProduct(bookingProductUpsertInput{ID: "p-disabled", Name: "Disabled", Enabled: &falseValue}); err != nil {
		t.Fatalf("upsert disabled product failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-enabled",
		ProductID: "p-enabled",
		StartAt:   "2026-05-07T10:00:00+08:00",
		EndAt:     "2026-05-07T11:00:00+08:00",
		Capacity:  &capacityValue,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("upsert enabled slot failed: %v", err)
	}
	if _, err := service.UpsertSlot(bookingSlotUpsertInput{
		ID:        "slot-disabled-product",
		ProductID: "p-disabled",
		StartAt:   "2026-05-07T12:00:00+08:00",
		EndAt:     "2026-05-07T13:00:00+08:00",
		Capacity:  &capacityValue,
		Enabled:   &trueValue,
	}); err != nil {
		t.Fatalf("upsert disabled-product slot failed: %v", err)
	}

	query, err := service.QueryCatalog(bookingCatalogQuery{})
	if err != nil {
		t.Fatalf("QueryCatalog failed: %v", err)
	}
	if len(query.Products) != 1 || len(query.Slots) != 1 {
		t.Fatalf("expected only enabled+available catalog entries, got products=%d slots=%d", len(query.Products), len(query.Slots))
	}
	if !strings.EqualFold(query.Products[0].ID, "p-enabled") || !strings.EqualFold(query.Slots[0].ID, "slot-enabled") {
		t.Fatalf("unexpected query entries: products=%+v slots=%+v", query.Products, query.Slots)
	}
}
