package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bookingCatalogStateVersion      = 1
	bookingReservationsStateVersion = 1
	bookingCatalogStateFile         = "booking_catalog.json"
	bookingReservationsStateFile    = "booking_reservations.json"

	bookingReservationStatusPending   = "pending"
	bookingReservationStatusConfirmed = "confirmed"
	bookingReservationStatusRejected  = "rejected"
	bookingReservationStatusCancelled = "cancelled"
)

var bookingStateMu sync.Mutex

type bookingService struct {
	catalogPath      string
	reservationsPath string
	nowFn            func() time.Time
}

type bookingCatalogState struct {
	Version   int              `json:"version"`
	Products  []bookingProduct `json:"products"`
	Slots     []bookingSlot    `json:"slots"`
	UpdatedAt string           `json:"updated_at,omitempty"`
}

type bookingReservationsState struct {
	Version      int                  `json:"version"`
	NextID       int64                `json:"next_id,omitempty"`
	Reservations []bookingReservation `json:"reservations"`
	UpdatedAt    string               `json:"updated_at,omitempty"`
}

type bookingProduct struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type bookingSlot struct {
	ID        string `json:"id"`
	ProductID string `json:"product_id"`
	StartAt   string `json:"start_at"`
	EndAt     string `json:"end_at"`
	Capacity  int    `json:"capacity"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type bookingReservationPersonnel struct {
	ContactName  string   `json:"contact_name"`
	ContactPhone string   `json:"contact_phone"`
	Members      []string `json:"members,omitempty"`
}

type bookingReservation struct {
	ID                  string                      `json:"id"`
	ProductID           string                      `json:"product_id"`
	SlotID              string                      `json:"slot_id"`
	UserID              string                      `json:"user_id"`
	PartySize           int                         `json:"party_size"`
	Personnel           bookingReservationPersonnel `json:"personnel"`
	SpecialRequirements string                      `json:"special_requirements,omitempty"`
	Status              string                      `json:"status"`
	SystemNote          string                      `json:"system_note,omitempty"`
	CreatedAt           string                      `json:"created_at,omitempty"`
	UpdatedAt           string                      `json:"updated_at,omitempty"`
}

type bookingSlotView struct {
	bookingSlot
	ProductName       string `json:"product_name,omitempty"`
	Reserved          int    `json:"reserved"`
	AvailableCapacity int    `json:"available_capacity"`
}

type bookingReservationView struct {
	bookingReservation
	ProductName       string `json:"product_name,omitempty"`
	SlotStartAt       string `json:"slot_start_at,omitempty"`
	SlotEndAt         string `json:"slot_end_at,omitempty"`
	AvailableCapacity int    `json:"available_capacity,omitempty"`
}

type bookingCatalogQueryResult struct {
	Products []bookingProduct  `json:"products"`
	Slots    []bookingSlotView `json:"slots"`
}

type bookingAdminStatus struct {
	Summary      bookingAdminSummary      `json:"summary"`
	Products     []bookingProduct         `json:"products"`
	Slots        []bookingSlotView        `json:"slots"`
	Reservations []bookingReservationView `json:"reservations"`
}

type bookingAdminSummary struct {
	ProductCount     int `json:"product_count"`
	SlotCount        int `json:"slot_count"`
	ReservationCount int `json:"reservation_count"`
	Pending          int `json:"pending"`
	Confirmed        int `json:"confirmed"`
	Rejected         int `json:"rejected"`
	Cancelled        int `json:"cancelled"`
}

type bookingProductUpsertInput struct {
	ID          string
	Name        string
	Description string
	Enabled     *bool
}

type bookingSlotUpsertInput struct {
	ID        string
	ProductID string
	StartAt   string
	EndAt     string
	Capacity  *int
	Enabled   *bool
}

type bookingReservationCreateInput struct {
	ProductID           string
	SlotID              string
	UserID              string
	PartySize           int
	Personnel           bookingReservationPersonnel
	SpecialRequirements string
}

type bookingSlotListFilter struct {
	ProductID       string
	From            *time.Time
	To              *time.Time
	IncludeFull     bool
	IncludeDisabled bool
}

type bookingReservationListFilter struct {
	UserID string
	Status string
}

type bookingCatalogQuery struct {
	ProductID   string
	From        *time.Time
	To          *time.Time
	IncludeFull bool
}

type bookingValidationError struct {
	Message string
}

func (e bookingValidationError) Error() string {
	return strings.TrimSpace(e.Message)
}

type bookingConflictError struct {
	Message string
}

func (e bookingConflictError) Error() string {
	return strings.TrimSpace(e.Message)
}

func bookingValidationf(format string, args ...any) error {
	return bookingValidationError{Message: fmt.Sprintf(format, args...)}
}

func bookingConflictf(format string, args ...any) error {
	return bookingConflictError{Message: fmt.Sprintf(format, args...)}
}

func isBookingValidationError(err error) bool {
	var target bookingValidationError
	return errors.As(err, &target)
}

func isBookingConflictError(err error) bool {
	var target bookingConflictError
	return errors.As(err, &target)
}

func defaultBookingCatalogStatePath() string {
	return filepath.Join(daemonDataDir, bookingCatalogStateFile)
}

func defaultBookingReservationsStatePath() string {
	return filepath.Join(daemonDataDir, bookingReservationsStateFile)
}

func newBookingService(catalogPath string, reservationsPath string) *bookingService {
	catalog := strings.TrimSpace(catalogPath)
	reservations := strings.TrimSpace(reservationsPath)
	if catalog == "" {
		catalog = defaultBookingCatalogStatePath()
	}
	if reservations == "" {
		reservations = defaultBookingReservationsStatePath()
	}
	return &bookingService{
		catalogPath:      catalog,
		reservationsPath: reservations,
		nowFn:            time.Now,
	}
}

func (s *bookingService) now() time.Time {
	if s == nil || s.nowFn == nil {
		return time.Now()
	}
	return s.nowFn()
}

func (s *bookingService) UpsertProduct(input bookingProductUpsertInput) (bookingProduct, error) {
	id := strings.TrimSpace(input.ID)
	if id == "" {
		return bookingProduct{}, bookingValidationf("product id is required")
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingProduct{}, err
	}

	nowText := s.now().Format(time.RFC3339Nano)
	index := -1
	for i := range catalog.Products {
		if strings.EqualFold(strings.TrimSpace(catalog.Products[i].ID), id) {
			index = i
			break
		}
	}

	if index < 0 {
		name := strings.TrimSpace(input.Name)
		if name == "" {
			return bookingProduct{}, bookingValidationf("product name is required")
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		product := bookingProduct{
			ID:          id,
			Name:        name,
			Description: strings.TrimSpace(input.Description),
			Enabled:     enabled,
			CreatedAt:   nowText,
			UpdatedAt:   nowText,
		}
		catalog.Products = append(catalog.Products, product)
		if err := writeBookingCatalogState(s.catalogPath, catalog, nowText); err != nil {
			return bookingProduct{}, err
		}
		return product, nil
	}

	product := catalog.Products[index]
	if name := strings.TrimSpace(input.Name); name != "" {
		product.Name = name
	}
	if strings.TrimSpace(product.Name) == "" {
		return bookingProduct{}, bookingValidationf("product name is required")
	}
	product.Description = strings.TrimSpace(input.Description)
	if input.Enabled != nil {
		product.Enabled = *input.Enabled
	}
	if strings.TrimSpace(product.CreatedAt) == "" {
		product.CreatedAt = nowText
	}
	product.UpdatedAt = nowText
	catalog.Products[index] = product

	if err := writeBookingCatalogState(s.catalogPath, catalog, nowText); err != nil {
		return bookingProduct{}, err
	}
	return product, nil
}

func (s *bookingService) RemoveProduct(id string) (bool, error) {
	productID := strings.TrimSpace(id)
	if productID == "" {
		return false, bookingValidationf("product id is required")
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return false, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return false, err
	}

	index := -1
	for i := range catalog.Products {
		if strings.EqualFold(strings.TrimSpace(catalog.Products[i].ID), productID) {
			index = i
			break
		}
	}
	if index < 0 {
		return false, nil
	}

	for _, slot := range catalog.Slots {
		if strings.EqualFold(strings.TrimSpace(slot.ProductID), productID) {
			return false, bookingValidationf("cannot remove product %q: slots still exist", productID)
		}
	}
	for _, reservation := range reservations.Reservations {
		if strings.EqualFold(strings.TrimSpace(reservation.ProductID), productID) &&
			(strings.EqualFold(reservation.Status, bookingReservationStatusPending) || strings.EqualFold(reservation.Status, bookingReservationStatusConfirmed)) {
			return false, bookingValidationf("cannot remove product %q: active reservations still exist", productID)
		}
	}

	catalog.Products = append(catalog.Products[:index], catalog.Products[index+1:]...)
	if err := writeBookingCatalogState(s.catalogPath, catalog, s.now().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	return true, nil
}

func (s *bookingService) ListProducts() ([]bookingProduct, error) {
	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return nil, err
	}
	products := append([]bookingProduct(nil), catalog.Products...)
	sort.Slice(products, func(i int, j int) bool {
		return strings.ToLower(products[i].ID) < strings.ToLower(products[j].ID)
	})
	return products, nil
}

func (s *bookingService) UpsertSlot(input bookingSlotUpsertInput) (bookingSlot, error) {
	id := strings.TrimSpace(input.ID)
	if id == "" {
		return bookingSlot{}, bookingValidationf("slot id is required")
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingSlot{}, err
	}

	nowText := s.now().Format(time.RFC3339Nano)
	index := -1
	for i := range catalog.Slots {
		if strings.EqualFold(strings.TrimSpace(catalog.Slots[i].ID), id) {
			index = i
			break
		}
	}

	var slot bookingSlot
	if index >= 0 {
		slot = catalog.Slots[index]
	}

	productID := strings.TrimSpace(input.ProductID)
	if productID == "" {
		productID = strings.TrimSpace(slot.ProductID)
	}
	if productID == "" {
		return bookingSlot{}, bookingValidationf("slot product_id is required")
	}
	if !bookingProductExists(catalog.Products, productID) {
		return bookingSlot{}, bookingValidationf("product not found: %s", productID)
	}

	startAtText := strings.TrimSpace(input.StartAt)
	if startAtText == "" {
		startAtText = strings.TrimSpace(slot.StartAt)
	}
	startAt, err := parseBookingRFC3339(startAtText, "slot start_at")
	if err != nil {
		return bookingSlot{}, err
	}

	endAtText := strings.TrimSpace(input.EndAt)
	if endAtText == "" {
		endAtText = strings.TrimSpace(slot.EndAt)
	}
	endAt, err := parseBookingRFC3339(endAtText, "slot end_at")
	if err != nil {
		return bookingSlot{}, err
	}
	if !startAt.Before(endAt) {
		return bookingSlot{}, bookingValidationf("slot start_at must be earlier than end_at")
	}

	capacity := slot.Capacity
	if input.Capacity != nil {
		capacity = *input.Capacity
	}
	if capacity <= 0 {
		return bookingSlot{}, bookingValidationf("slot capacity must be greater than 0")
	}

	enabled := slot.Enabled
	if index < 0 {
		enabled = true
	}
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	slot.ID = id
	slot.ProductID = productID
	slot.StartAt = startAt.Format(time.RFC3339Nano)
	slot.EndAt = endAt.Format(time.RFC3339Nano)
	slot.Capacity = capacity
	slot.Enabled = enabled
	if strings.TrimSpace(slot.CreatedAt) == "" {
		slot.CreatedAt = nowText
	}
	slot.UpdatedAt = nowText

	if index < 0 {
		catalog.Slots = append(catalog.Slots, slot)
	} else {
		catalog.Slots[index] = slot
	}
	if err := writeBookingCatalogState(s.catalogPath, catalog, nowText); err != nil {
		return bookingSlot{}, err
	}
	return slot, nil
}

func (s *bookingService) RemoveSlot(id string) (bool, error) {
	slotID := strings.TrimSpace(id)
	if slotID == "" {
		return false, bookingValidationf("slot id is required")
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return false, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return false, err
	}

	index := -1
	for i := range catalog.Slots {
		if strings.EqualFold(strings.TrimSpace(catalog.Slots[i].ID), slotID) {
			index = i
			break
		}
	}
	if index < 0 {
		return false, nil
	}

	for _, reservation := range reservations.Reservations {
		if !strings.EqualFold(strings.TrimSpace(reservation.SlotID), slotID) {
			continue
		}
		if strings.EqualFold(reservation.Status, bookingReservationStatusPending) || strings.EqualFold(reservation.Status, bookingReservationStatusConfirmed) {
			return false, bookingValidationf("cannot remove slot %q: active reservations still exist", slotID)
		}
	}

	catalog.Slots = append(catalog.Slots[:index], catalog.Slots[index+1:]...)
	if err := writeBookingCatalogState(s.catalogPath, catalog, s.now().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	return true, nil
}

func (s *bookingService) ListSlots(filter bookingSlotListFilter) ([]bookingSlotView, error) {
	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return nil, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return nil, err
	}
	return buildBookingSlotViews(catalog, reservations, filter), nil
}

func (s *bookingService) CreateReservation(input bookingReservationCreateInput) (bookingReservationView, error) {
	productID := strings.TrimSpace(input.ProductID)
	slotID := strings.TrimSpace(input.SlotID)
	userID := strings.TrimSpace(input.UserID)
	partySize := input.PartySize
	if productID == "" {
		return bookingReservationView{}, bookingValidationf("product_id is required")
	}
	if slotID == "" {
		return bookingReservationView{}, bookingValidationf("slot_id is required")
	}
	if userID == "" {
		return bookingReservationView{}, bookingValidationf("user_id is required")
	}
	if partySize <= 0 {
		return bookingReservationView{}, bookingValidationf("party_size must be greater than 0")
	}
	personnel, err := normalizeBookingPersonnel(input.Personnel)
	if err != nil {
		return bookingReservationView{}, err
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingReservationView{}, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return bookingReservationView{}, err
	}

	product, productOK := bookingFindProduct(catalog.Products, productID)
	if !productOK {
		return bookingReservationView{}, bookingValidationf("product not found: %s", productID)
	}
	if !product.Enabled {
		return bookingReservationView{}, bookingValidationf("product is disabled: %s", productID)
	}

	slot, slotOK := bookingFindSlot(catalog.Slots, slotID)
	if !slotOK {
		return bookingReservationView{}, bookingValidationf("slot not found: %s", slotID)
	}
	if !strings.EqualFold(strings.TrimSpace(slot.ProductID), productID) {
		return bookingReservationView{}, bookingValidationf("slot %s does not belong to product %s", slotID, productID)
	}
	if !slot.Enabled {
		return bookingReservationView{}, bookingValidationf("slot is disabled: %s", slotID)
	}

	available := bookingSlotAvailableCapacity(slot.ID, slot.Capacity, reservations.Reservations, "")
	if available < partySize {
		return bookingReservationView{}, bookingConflictf("capacity conflict: slot %s available=%d required=%d", slotID, available, partySize)
	}

	nowText := s.now().Format(time.RFC3339Nano)
	if reservations.NextID <= 0 {
		reservations.NextID = bookingNextReservationID(reservations.Reservations)
	}
	reservationID := fmt.Sprintf("r-%d", reservations.NextID)
	reservations.NextID++

	reservation := bookingReservation{
		ID:                  reservationID,
		ProductID:           productID,
		SlotID:              slotID,
		UserID:              userID,
		PartySize:           partySize,
		Personnel:           personnel,
		SpecialRequirements: strings.TrimSpace(input.SpecialRequirements),
		Status:              bookingReservationStatusPending,
		CreatedAt:           nowText,
		UpdatedAt:           nowText,
	}
	reservations.Reservations = append(reservations.Reservations, reservation)
	if err := writeBookingReservationsState(s.reservationsPath, reservations, nowText); err != nil {
		return bookingReservationView{}, err
	}

	view := bookingBuildReservationView(reservation, product, slot, available)
	return view, nil
}

func (s *bookingService) ListReservations(filter bookingReservationListFilter) ([]bookingReservationView, error) {
	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return nil, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return nil, err
	}
	return bookingFilterReservationViews(catalog, reservations, filter)
}

func (s *bookingService) ConfirmReservation(id string, note string) (bookingReservationView, error) {
	return s.transitionReservation(id, bookingReservationStatusConfirmed, note)
}

func (s *bookingService) RejectReservation(id string, note string) (bookingReservationView, error) {
	return s.transitionReservation(id, bookingReservationStatusRejected, note)
}

func (s *bookingService) CancelReservation(id string, note string) (bookingReservationView, error) {
	return s.transitionReservation(id, bookingReservationStatusCancelled, note)
}

func (s *bookingService) transitionReservation(id string, targetStatus string, note string) (bookingReservationView, error) {
	reservationID := strings.TrimSpace(id)
	if reservationID == "" {
		return bookingReservationView{}, bookingValidationf("reservation id is required")
	}
	status, err := normalizeBookingReservationStatus(targetStatus)
	if err != nil {
		return bookingReservationView{}, err
	}
	if status != bookingReservationStatusConfirmed && status != bookingReservationStatusRejected && status != bookingReservationStatusCancelled {
		return bookingReservationView{}, bookingValidationf("unsupported reservation status transition target: %s", status)
	}

	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingReservationView{}, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return bookingReservationView{}, err
	}

	index := -1
	for i := range reservations.Reservations {
		if strings.EqualFold(strings.TrimSpace(reservations.Reservations[i].ID), reservationID) {
			index = i
			break
		}
	}
	if index < 0 {
		return bookingReservationView{}, bookingValidationf("reservation not found: %s", reservationID)
	}

	reservation := reservations.Reservations[index]
	currentStatus, err := normalizeBookingReservationStatus(reservation.Status)
	if err != nil {
		return bookingReservationView{}, err
	}
	if currentStatus != bookingReservationStatusPending {
		return bookingReservationView{}, bookingValidationf("reservation %s cannot transition from %s", reservation.ID, currentStatus)
	}

	product, productOK := bookingFindProduct(catalog.Products, reservation.ProductID)
	slot, slotOK := bookingFindSlot(catalog.Slots, reservation.SlotID)

	if status == bookingReservationStatusConfirmed {
		if !productOK {
			return bookingReservationView{}, bookingValidationf("product not found for reservation %s: %s", reservation.ID, reservation.ProductID)
		}
		if !slotOK {
			return bookingReservationView{}, bookingValidationf("slot not found for reservation %s: %s", reservation.ID, reservation.SlotID)
		}
		if !slot.Enabled {
			return bookingReservationView{}, bookingValidationf("slot is disabled: %s", slot.ID)
		}
		available := bookingSlotAvailableCapacity(slot.ID, slot.Capacity, reservations.Reservations, reservation.ID)
		if available < reservation.PartySize {
			return bookingReservationView{}, bookingConflictf(
				"capacity conflict: slot %s available=%d required=%d",
				slot.ID,
				available,
				reservation.PartySize,
			)
		}
	}

	reservation.Status = status
	reservation.SystemNote = strings.TrimSpace(note)
	reservation.UpdatedAt = s.now().Format(time.RFC3339Nano)
	reservations.Reservations[index] = reservation

	if err := writeBookingReservationsState(s.reservationsPath, reservations, reservation.UpdatedAt); err != nil {
		return bookingReservationView{}, err
	}

	available := 0
	if slotOK {
		available = bookingSlotAvailableCapacity(slot.ID, slot.Capacity, reservations.Reservations, "")
	}
	view := bookingBuildReservationView(reservation, product, slot, available)
	return view, nil
}

func (s *bookingService) QueryCatalog(query bookingCatalogQuery) (bookingCatalogQueryResult, error) {
	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingCatalogQueryResult{}, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return bookingCatalogQueryResult{}, err
	}
	if query.From != nil && query.To != nil && query.From.After(*query.To) {
		return bookingCatalogQueryResult{}, bookingValidationf("from must be earlier than or equal to to")
	}

	filter := bookingSlotListFilter{
		ProductID:       strings.TrimSpace(query.ProductID),
		From:            query.From,
		To:              query.To,
		IncludeFull:     query.IncludeFull,
		IncludeDisabled: false,
	}
	slots := buildBookingSlotViews(catalog, reservations, filter)

	products := make([]bookingProduct, 0)
	seen := make(map[string]struct{})
	for _, slot := range slots {
		productID := strings.TrimSpace(slot.ProductID)
		if productID == "" {
			continue
		}
		if _, ok := seen[strings.ToLower(productID)]; ok {
			continue
		}
		product, ok := bookingFindProduct(catalog.Products, productID)
		if !ok || !product.Enabled {
			continue
		}
		products = append(products, product)
		seen[strings.ToLower(productID)] = struct{}{}
	}
	sort.Slice(products, func(i int, j int) bool {
		return strings.ToLower(products[i].ID) < strings.ToLower(products[j].ID)
	})

	return bookingCatalogQueryResult{
		Products: products,
		Slots:    slots,
	}, nil
}

func (s *bookingService) AdminStatus() (bookingAdminStatus, error) {
	bookingStateMu.Lock()
	defer bookingStateMu.Unlock()

	catalog, err := readBookingCatalogState(s.catalogPath)
	if err != nil {
		return bookingAdminStatus{}, err
	}
	reservations, err := readBookingReservationsState(s.reservationsPath)
	if err != nil {
		return bookingAdminStatus{}, err
	}

	slots := buildBookingSlotViews(catalog, reservations, bookingSlotListFilter{
		IncludeFull:     true,
		IncludeDisabled: true,
	})
	reservationViews, err := bookingFilterReservationViews(catalog, reservations, bookingReservationListFilter{})
	if err != nil {
		return bookingAdminStatus{}, err
	}

	summary := bookingAdminSummary{
		ProductCount:     len(catalog.Products),
		SlotCount:        len(catalog.Slots),
		ReservationCount: len(reservations.Reservations),
	}
	for _, reservation := range reservations.Reservations {
		status, normalizeErr := normalizeBookingReservationStatus(reservation.Status)
		if normalizeErr != nil {
			continue
		}
		switch status {
		case bookingReservationStatusPending:
			summary.Pending++
		case bookingReservationStatusConfirmed:
			summary.Confirmed++
		case bookingReservationStatusRejected:
			summary.Rejected++
		case bookingReservationStatusCancelled:
			summary.Cancelled++
		}
	}

	products := append([]bookingProduct(nil), catalog.Products...)
	sort.Slice(products, func(i int, j int) bool {
		return strings.ToLower(products[i].ID) < strings.ToLower(products[j].ID)
	})
	return bookingAdminStatus{
		Summary:      summary,
		Products:     products,
		Slots:        slots,
		Reservations: reservationViews,
	}, nil
}

func readBookingCatalogState(path string) (bookingCatalogState, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return bookingCatalogState{}, fmt.Errorf("booking catalog path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bookingCatalogState{
				Version:  bookingCatalogStateVersion,
				Products: []bookingProduct{},
				Slots:    []bookingSlot{},
			}, nil
		}
		return bookingCatalogState{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return bookingCatalogState{
			Version:  bookingCatalogStateVersion,
			Products: []bookingProduct{},
			Slots:    []bookingSlot{},
		}, nil
	}

	var state bookingCatalogState
	if err := json.Unmarshal(raw, &state); err != nil {
		return bookingCatalogState{}, err
	}
	if state.Version == 0 {
		state.Version = bookingCatalogStateVersion
	}
	if state.Products == nil {
		state.Products = []bookingProduct{}
	}
	if state.Slots == nil {
		state.Slots = []bookingSlot{}
	}
	for i := range state.Products {
		state.Products[i].ID = strings.TrimSpace(state.Products[i].ID)
		state.Products[i].Name = strings.TrimSpace(state.Products[i].Name)
		state.Products[i].Description = strings.TrimSpace(state.Products[i].Description)
	}
	for i := range state.Slots {
		state.Slots[i].ID = strings.TrimSpace(state.Slots[i].ID)
		state.Slots[i].ProductID = strings.TrimSpace(state.Slots[i].ProductID)
		state.Slots[i].StartAt = strings.TrimSpace(state.Slots[i].StartAt)
		state.Slots[i].EndAt = strings.TrimSpace(state.Slots[i].EndAt)
	}
	return state, nil
}

func writeBookingCatalogState(path string, state bookingCatalogState, nowText string) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("booking catalog path is empty")
	}
	state.Version = bookingCatalogStateVersion
	state.UpdatedAt = strings.TrimSpace(nowText)
	if state.Products == nil {
		state.Products = []bookingProduct{}
	}
	if state.Slots == nil {
		state.Slots = []bookingSlot{}
	}
	sort.Slice(state.Products, func(i int, j int) bool {
		return strings.ToLower(strings.TrimSpace(state.Products[i].ID)) < strings.ToLower(strings.TrimSpace(state.Products[j].ID))
	})
	sort.Slice(state.Slots, func(i int, j int) bool {
		leftStart := parseBookingTimeOrZero(state.Slots[i].StartAt)
		rightStart := parseBookingTimeOrZero(state.Slots[j].StartAt)
		if leftStart.Equal(rightStart) {
			return strings.ToLower(strings.TrimSpace(state.Slots[i].ID)) < strings.ToLower(strings.TrimSpace(state.Slots[j].ID))
		}
		return leftStart.Before(rightStart)
	})

	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(encoded, '\n'), 0o644)
}

func readBookingReservationsState(path string) (bookingReservationsState, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return bookingReservationsState{}, fmt.Errorf("booking reservations path is empty")
	}
	raw, err := os.ReadFile(trimmedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bookingReservationsState{
				Version:      bookingReservationsStateVersion,
				NextID:       1,
				Reservations: []bookingReservation{},
			}, nil
		}
		return bookingReservationsState{}, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return bookingReservationsState{
			Version:      bookingReservationsStateVersion,
			NextID:       1,
			Reservations: []bookingReservation{},
		}, nil
	}

	var state bookingReservationsState
	if err := json.Unmarshal(raw, &state); err != nil {
		return bookingReservationsState{}, err
	}
	if state.Version == 0 {
		state.Version = bookingReservationsStateVersion
	}
	if state.Reservations == nil {
		state.Reservations = []bookingReservation{}
	}
	if state.NextID <= 0 {
		state.NextID = bookingNextReservationID(state.Reservations)
	}
	for i := range state.Reservations {
		state.Reservations[i].ID = strings.TrimSpace(state.Reservations[i].ID)
		state.Reservations[i].ProductID = strings.TrimSpace(state.Reservations[i].ProductID)
		state.Reservations[i].SlotID = strings.TrimSpace(state.Reservations[i].SlotID)
		state.Reservations[i].UserID = strings.TrimSpace(state.Reservations[i].UserID)
		state.Reservations[i].Status = strings.TrimSpace(state.Reservations[i].Status)
		state.Reservations[i].Personnel.ContactName = strings.TrimSpace(state.Reservations[i].Personnel.ContactName)
		state.Reservations[i].Personnel.ContactPhone = strings.TrimSpace(state.Reservations[i].Personnel.ContactPhone)
		state.Reservations[i].SpecialRequirements = strings.TrimSpace(state.Reservations[i].SpecialRequirements)
	}
	return state, nil
}

func writeBookingReservationsState(path string, state bookingReservationsState, nowText string) error {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return fmt.Errorf("booking reservations path is empty")
	}
	state.Version = bookingReservationsStateVersion
	state.UpdatedAt = strings.TrimSpace(nowText)
	if state.NextID <= 0 {
		state.NextID = bookingNextReservationID(state.Reservations)
	}
	if state.Reservations == nil {
		state.Reservations = []bookingReservation{}
	}
	sort.Slice(state.Reservations, func(i int, j int) bool {
		leftCreated := parseBookingTimeOrZero(state.Reservations[i].CreatedAt)
		rightCreated := parseBookingTimeOrZero(state.Reservations[j].CreatedAt)
		if leftCreated.Equal(rightCreated) {
			return strings.ToLower(strings.TrimSpace(state.Reservations[i].ID)) < strings.ToLower(strings.TrimSpace(state.Reservations[j].ID))
		}
		return leftCreated.Before(rightCreated)
	})

	encoded, err := json.MarshalIndent(state, "", defaultWebhookResponseIndent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(trimmedPath, append(encoded, '\n'), 0o644)
}

func parseBookingRFC3339(value string, field string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, bookingValidationf("%s is required", strings.TrimSpace(field))
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return time.Time{}, bookingValidationf("%s must be RFC3339: %v", strings.TrimSpace(field), err)
	}
	return parsed, nil
}

func parseBookingTimeOrZero(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func normalizeBookingPersonnel(personnel bookingReservationPersonnel) (bookingReservationPersonnel, error) {
	normalized := bookingReservationPersonnel{
		ContactName:  strings.TrimSpace(personnel.ContactName),
		ContactPhone: strings.TrimSpace(personnel.ContactPhone),
		Members:      make([]string, 0, len(personnel.Members)),
	}
	if normalized.ContactName == "" {
		return bookingReservationPersonnel{}, bookingValidationf("personnel.contact_name is required")
	}
	if normalized.ContactPhone == "" {
		return bookingReservationPersonnel{}, bookingValidationf("personnel.contact_phone is required")
	}
	for _, member := range personnel.Members {
		trimmed := strings.TrimSpace(member)
		if trimmed == "" {
			continue
		}
		normalized.Members = append(normalized.Members, trimmed)
	}
	return normalized, nil
}

func normalizeBookingReservationStatus(status string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(status))
	if normalized == "" {
		return "", bookingValidationf("reservation status is required")
	}
	switch normalized {
	case bookingReservationStatusPending,
		bookingReservationStatusConfirmed,
		bookingReservationStatusRejected,
		bookingReservationStatusCancelled:
		return normalized, nil
	default:
		return "", bookingValidationf("invalid reservation status %q", status)
	}
}

func bookingProductExists(products []bookingProduct, productID string) bool {
	_, ok := bookingFindProduct(products, productID)
	return ok
}

func bookingFindProduct(products []bookingProduct, productID string) (bookingProduct, bool) {
	target := strings.TrimSpace(productID)
	for _, product := range products {
		if strings.EqualFold(strings.TrimSpace(product.ID), target) {
			return product, true
		}
	}
	return bookingProduct{}, false
}

func bookingFindSlot(slots []bookingSlot, slotID string) (bookingSlot, bool) {
	target := strings.TrimSpace(slotID)
	for _, slot := range slots {
		if strings.EqualFold(strings.TrimSpace(slot.ID), target) {
			return slot, true
		}
	}
	return bookingSlot{}, false
}

func bookingNextReservationID(reservations []bookingReservation) int64 {
	maxID := int64(0)
	for _, reservation := range reservations {
		id := strings.TrimSpace(reservation.ID)
		if id == "" {
			continue
		}
		value := id
		if strings.HasPrefix(strings.ToLower(id), "r-") {
			value = id[2:]
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		if parsed > maxID {
			maxID = parsed
		}
	}
	return maxID + 1
}

func bookingSlotAvailableCapacity(slotID string, capacity int, reservations []bookingReservation, excludeReservationID string) int {
	if capacity <= 0 {
		return 0
	}
	reserved := 0
	targetSlot := strings.TrimSpace(slotID)
	excludeID := strings.TrimSpace(excludeReservationID)
	for _, reservation := range reservations {
		if !strings.EqualFold(strings.TrimSpace(reservation.SlotID), targetSlot) {
			continue
		}
		if excludeID != "" && strings.EqualFold(strings.TrimSpace(reservation.ID), excludeID) {
			continue
		}
		status, err := normalizeBookingReservationStatus(reservation.Status)
		if err != nil {
			continue
		}
		if status != bookingReservationStatusConfirmed {
			continue
		}
		if reservation.PartySize <= 0 {
			continue
		}
		reserved += reservation.PartySize
	}
	available := capacity - reserved
	if available < 0 {
		return 0
	}
	return available
}

func buildBookingSlotViews(catalog bookingCatalogState, reservations bookingReservationsState, filter bookingSlotListFilter) []bookingSlotView {
	productFilter := strings.TrimSpace(filter.ProductID)
	productNames := make(map[string]string, len(catalog.Products))
	productEnabled := make(map[string]bool, len(catalog.Products))
	for _, product := range catalog.Products {
		key := strings.ToLower(strings.TrimSpace(product.ID))
		productNames[key] = strings.TrimSpace(product.Name)
		productEnabled[key] = product.Enabled
	}

	views := make([]bookingSlotView, 0, len(catalog.Slots))
	for _, slot := range catalog.Slots {
		slotID := strings.TrimSpace(slot.ID)
		if slotID == "" {
			continue
		}
		productID := strings.TrimSpace(slot.ProductID)
		productKey := strings.ToLower(productID)
		if productFilter != "" && !strings.EqualFold(productFilter, productID) {
			continue
		}
		if !filter.IncludeDisabled {
			if !slot.Enabled {
				continue
			}
			if enabled, ok := productEnabled[productKey]; !ok || !enabled {
				continue
			}
		}

		startAt := parseBookingTimeOrZero(slot.StartAt)
		if filter.From != nil && !startAt.IsZero() && startAt.Before(*filter.From) {
			continue
		}
		if filter.To != nil && !startAt.IsZero() && startAt.After(*filter.To) {
			continue
		}

		reserved := slot.Capacity - bookingSlotAvailableCapacity(slotID, slot.Capacity, reservations.Reservations, "")
		if reserved < 0 {
			reserved = 0
		}
		available := slot.Capacity - reserved
		if available < 0 {
			available = 0
		}
		if !filter.IncludeFull && available <= 0 {
			continue
		}

		view := bookingSlotView{
			bookingSlot:       slot,
			ProductName:       productNames[productKey],
			Reserved:          reserved,
			AvailableCapacity: available,
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i int, j int) bool {
		leftStart := parseBookingTimeOrZero(views[i].StartAt)
		rightStart := parseBookingTimeOrZero(views[j].StartAt)
		if leftStart.Equal(rightStart) {
			return strings.ToLower(strings.TrimSpace(views[i].ID)) < strings.ToLower(strings.TrimSpace(views[j].ID))
		}
		return leftStart.Before(rightStart)
	})
	return views
}

func bookingFilterReservationViews(catalog bookingCatalogState, reservations bookingReservationsState, filter bookingReservationListFilter) ([]bookingReservationView, error) {
	userFilter := strings.TrimSpace(filter.UserID)
	statusFilter := strings.TrimSpace(filter.Status)
	if statusFilter != "" {
		normalized, err := normalizeBookingReservationStatus(statusFilter)
		if err != nil {
			return nil, err
		}
		statusFilter = normalized
	}

	views := make([]bookingReservationView, 0, len(reservations.Reservations))
	for _, reservation := range reservations.Reservations {
		if userFilter != "" && !strings.EqualFold(strings.TrimSpace(reservation.UserID), userFilter) {
			continue
		}
		status, err := normalizeBookingReservationStatus(reservation.Status)
		if err != nil {
			continue
		}
		if statusFilter != "" && statusFilter != status {
			continue
		}

		product, _ := bookingFindProduct(catalog.Products, reservation.ProductID)
		slot, slotOK := bookingFindSlot(catalog.Slots, reservation.SlotID)
		available := 0
		if slotOK {
			available = bookingSlotAvailableCapacity(slot.ID, slot.Capacity, reservations.Reservations, "")
		}
		view := bookingBuildReservationView(reservation, product, slot, available)
		views = append(views, view)
	}

	sort.Slice(views, func(i int, j int) bool {
		leftCreated := parseBookingTimeOrZero(views[i].CreatedAt)
		rightCreated := parseBookingTimeOrZero(views[j].CreatedAt)
		if leftCreated.Equal(rightCreated) {
			return strings.ToLower(strings.TrimSpace(views[i].ID)) < strings.ToLower(strings.TrimSpace(views[j].ID))
		}
		return leftCreated.After(rightCreated)
	})
	return views, nil
}

func bookingBuildReservationView(reservation bookingReservation, product bookingProduct, slot bookingSlot, available int) bookingReservationView {
	view := bookingReservationView{
		bookingReservation: reservation,
		ProductName:        strings.TrimSpace(product.Name),
		SlotStartAt:        strings.TrimSpace(slot.StartAt),
		SlotEndAt:          strings.TrimSpace(slot.EndAt),
	}
	if available > 0 {
		view.AvailableCapacity = available
	}
	return view
}
