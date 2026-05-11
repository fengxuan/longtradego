package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var bookingAgentHTTPClientFactory = func(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

type bookingCommandResult struct {
	Mode         string                      `json:"mode"`
	Status       string                      `json:"status,omitempty"`
	Message      string                      `json:"message,omitempty"`
	Product      *bookingProduct             `json:"product,omitempty"`
	Products     []bookingProduct            `json:"products,omitempty"`
	Slot         *bookingSlot                `json:"slot,omitempty"`
	Slots        []bookingSlotView           `json:"slots,omitempty"`
	Reservation  *bookingReservationView     `json:"reservation,omitempty"`
	Reservations []bookingReservationView    `json:"reservations,omitempty"`
	Query        *bookingCatalogQueryResult  `json:"query,omitempty"`
	ServiceStart *bookingServiceStartResult  `json:"service_start,omitempty"`
	ServiceStop  *bookingServiceStopResult   `json:"service_stop,omitempty"`
	ServiceState *bookingServiceStatusResult `json:"service_status,omitempty"`
	AgentReserve *bookingAgentReserveResult  `json:"agent_reserve,omitempty"`
}

type bookingAgentReserveResult struct {
	PublicBaseURL      string                         `json:"public_base_url,omitempty"`
	ThirdPartyID       string                         `json:"third_party_id,omitempty"`
	Parse              bookingAgentReserveStepResult  `json:"parse"`
	Confirm            *bookingAgentReserveStepResult `json:"confirm,omitempty"`
	DraftID            string                         `json:"draft_id,omitempty"`
	ReservationID      string                         `json:"reservation_id,omitempty"`
	FinalStatus        string                         `json:"final_status,omitempty"`
	MissingFields      []string                       `json:"missing_fields,omitempty"`
	ParseExtracted     *bookingIntentParseExtracted   `json:"parse_extracted,omitempty"`
	ConfirmPayload     map[string]any                 `json:"confirm_payload,omitempty"`
	IdempotencyParse   string                         `json:"idempotency_key_parse,omitempty"`
	IdempotencyConfirm string                         `json:"idempotency_key_confirm,omitempty"`
}

type bookingAgentReserveStepResult struct {
	TargetURL      string `json:"target_url"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Code           string `json:"code,omitempty"`
	Message        string `json:"message,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type bookingAgentReserveParseEnvelopeData struct {
	DraftID string `json:"draft_id"`
	Draft   struct {
		ID            string                      `json:"id"`
		Extracted     bookingIntentParseExtracted `json:"extracted"`
		MissingFields []string                    `json:"missing_fields"`
	} `json:"draft"`
}

type bookingAgentReserveConfirmEnvelopeData struct {
	DraftID       string `json:"draft_id"`
	ReservationID string `json:"reservation_id"`
	Reservation   struct {
		Status string `json:"status"`
	} `json:"reservation"`
}

func newBookingCommand(app *AppContext) *cobra.Command {
	var (
		catalogPath      string
		reservationsPath string
	)

	bookingCmd := &cobra.Command{
		Use:   "booking",
		Short: "Manage booking products, slots, and reservations",
	}

	bookingCmd.PersistentFlags().StringVar(&catalogPath, "catalog", defaultBookingCatalogStatePath(), "Path to booking catalog state JSON")
	bookingCmd.PersistentFlags().StringVar(&reservationsPath, "reservations", defaultBookingReservationsStatePath(), "Path to booking reservations state JSON")

	newService := func() *bookingService {
		return newBookingService(catalogPath, reservationsPath)
	}

	bookingCmd.AddCommand(newBookingProductCommand(app, newService))
	bookingCmd.AddCommand(newBookingSlotCommand(app, newService))
	bookingCmd.AddCommand(newBookingReservationCommand(app, newService))
	bookingCmd.AddCommand(newBookingQueryCommand(app, newService))
	bookingCmd.AddCommand(newBookingAgentCommand(app))
	bookingCmd.AddCommand(newBookingMCPCommand(app))
	bookingCmd.AddCommand(newBookingServiceCommand(app, func() *bookingServiceServeConfig {
		cfg := newBookingServiceServeConfig()
		cfg.CatalogPath = strings.TrimSpace(catalogPath)
		cfg.ReservationsPath = strings.TrimSpace(reservationsPath)
		return cfg
	}))
	return bookingCmd
}

func newBookingServiceCommand(app *AppContext, configFactory func() *bookingServiceServeConfig) *cobra.Command {
	var (
		publicAddr       string
		adminAddr        string
		runtimePath      string
		logPath          string
		draftsPath       string
		securityKeysPath string
		llmConfigPath    string
		adminAuthPath    string
		timeout          time.Duration
		maxPortFallback  int
	)

	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Manage booking HTTP service lifecycle",
	}

	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start booking service in background",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := configFactory()
			cfg.PublicAddr = strings.TrimSpace(publicAddr)
			cfg.Addr = cfg.PublicAddr
			cfg.AdminAddr = strings.TrimSpace(adminAddr)
			cfg.RuntimePath = strings.TrimSpace(runtimePath)
			cfg.DraftsPath = strings.TrimSpace(draftsPath)
			if trimmed := strings.TrimSpace(securityKeysPath); trimmed != "" {
				cfg.APIKeysPath = trimmed
			}
			cfg.LLMConfigPath = strings.TrimSpace(llmConfigPath)
			cfg.AdminAuthPath = strings.TrimSpace(adminAuthPath)
			cfg.MaxPortFallback = maxPortFallback
			if err := cfg.validate(); err != nil {
				return err
			}
			var ownerClaim *bookingServiceStartOwnerClaim
			if claim, ok := bookingServiceStartOwnerClaimFromContext(cmd.Context()); ok {
				ownerClaim = &claim
			}
			result, err := startBookingServiceInBackgroundWithOwner(cfg.RuntimePath, strings.TrimSpace(logPath), cfg, ownerClaim)
			if app != nil {
				app.SetExecution("booking", []string{"service", "start"})
				app.SetResult(bookingCommandResult{
					Mode:         "booking_service_start",
					Status:       result.Status,
					Message:      strings.TrimSpace(result.Message),
					ServiceStart: &result,
				})
			}
			if result.Runtime != nil {
				fmt.Printf(
					"booking service %s: pid=%d public=%s admin=%s\n",
					result.Status,
					result.Runtime.PID,
					bookingRuntimePublicAddress(result.Runtime),
					bookingRuntimeAdminAddress(result.Runtime),
				)
			} else {
				fmt.Printf("booking service %s\n", result.Status)
			}
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			return err
		},
	}

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop booking background service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return fmt.Errorf("timeout must be > 0")
			}
			result, err := stopBookingService(strings.TrimSpace(runtimePath), timeout)
			if app != nil {
				app.SetExecution("booking", []string{"service", "stop"})
				app.SetResult(bookingCommandResult{
					Mode:        "booking_service_stop",
					Status:      result.Status,
					Message:     strings.TrimSpace(result.Message),
					ServiceStop: &result,
				})
			}
			if result.PID > 0 {
				fmt.Printf("booking service stop status=%s pid=%d\n", result.Status, result.PID)
			} else {
				fmt.Printf("booking service stop status=%s\n", result.Status)
			}
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			return err
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show booking service runtime status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := bookingServiceStatus(strings.TrimSpace(runtimePath))
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"service", "status"})
				app.SetResult(bookingCommandResult{
					Mode:         "booking_service_status",
					Status:       result.Status,
					Message:      strings.TrimSpace(result.Message),
					ServiceState: &result,
				})
			}
			if result.Runtime != nil {
				fmt.Printf(
					"booking service status=%s running=%t pid=%d public=%s admin=%s\n",
					result.Status,
					result.Running,
					result.Runtime.PID,
					bookingRuntimePublicAddress(result.Runtime),
					bookingRuntimeAdminAddress(result.Runtime),
				)
			} else {
				fmt.Printf("booking service status=%s running=%t\n", result.Status, result.Running)
			}
			if strings.TrimSpace(result.Message) != "" {
				fmt.Println(result.Message)
			}
			return nil
		},
	}

	serveCmd := &cobra.Command{
		Use:    "serve",
		Short:  "Start booking service in foreground (internal)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(os.Getenv(bookingServiceInternalEnv)) != "1" {
				return fmt.Errorf("booking service serve is internal; use `booking service start`")
			}
			cfg := configFactory()
			cfg.PublicAddr = strings.TrimSpace(publicAddr)
			cfg.Addr = cfg.PublicAddr
			cfg.AdminAddr = strings.TrimSpace(adminAddr)
			cfg.RuntimePath = strings.TrimSpace(runtimePath)
			cfg.DraftsPath = strings.TrimSpace(draftsPath)
			if trimmed := strings.TrimSpace(securityKeysPath); trimmed != "" {
				cfg.APIKeysPath = trimmed
			}
			cfg.LLMConfigPath = strings.TrimSpace(llmConfigPath)
			cfg.AdminAuthPath = strings.TrimSpace(adminAuthPath)
			cfg.MaxPortFallback = maxPortFallback
			handle, err := startBookingHTTPService(cmd.Context(), cfg, os.Stdout)
			if err != nil {
				return err
			}
			defer func() {
				_ = handle.Close()
			}()
			fmt.Printf("Booking service listening on public=%s admin=%s\n", cfg.PublicAddr, cfg.AdminAddr)
			return handle.Wait()
		},
	}

	serviceCmd.PersistentFlags().StringVar(&publicAddr, "public-addr", defaultBookingServiceAddr, "Public listen address, e.g. :18081")
	serviceCmd.PersistentFlags().StringVar(&adminAddr, "admin-addr", defaultBookingServiceAdminAddr, "Admin listen address, e.g. 127.0.0.1:18082")
	serviceCmd.PersistentFlags().StringVar(&runtimePath, "runtime", defaultBookingRuntimeStatePath(), "Path to booking runtime state JSON")
	serviceCmd.PersistentFlags().StringVar(&draftsPath, "drafts", defaultBookingIntakeDraftsPath(), "Path to booking intake drafts JSON")
	serviceCmd.PersistentFlags().StringVar(&securityKeysPath, "security-keys", defaultSecurityKeysPath(), "Path to unified security keys JSON")
	serviceCmd.PersistentFlags().StringVar(&llmConfigPath, "llm-config", defaultBookingLLMConfigPath(), "Path to booking LLM config JSON")
	serviceCmd.PersistentFlags().StringVar(&adminAuthPath, "admin-auth", defaultDaemonAdminAuthConfigPath(), "Path to admin basic auth config JSON")
	serviceCmd.PersistentFlags().IntVar(&maxPortFallback, "max-port-fallback", defaultBookingServicePortFallback, "Fallback attempts when preferred port is occupied")

	startCmd.Flags().StringVar(&logPath, "log-file", defaultBookingServiceLogPath(), "Path to booking service log file")
	stopCmd.Flags().DurationVar(&timeout, "timeout", defaultBookingServiceStopTimeout, "Graceful stop timeout, e.g. 5s")

	serviceCmd.AddCommand(startCmd, stopCmd, statusCmd, serveCmd)
	return serviceCmd
}

func newBookingProductCommand(app *AppContext, newService func() *bookingService) *cobra.Command {
	productCmd := &cobra.Command{
		Use:   "product",
		Short: "Manage booking products",
	}

	var (
		id          string
		name        string
		description string
		enabled     bool
	)
	addOrUpdate := func(mode string) *cobra.Command {
		cmd := &cobra.Command{
			Use:   mode,
			Short: "Upsert booking product",
			RunE: func(cmd *cobra.Command, _ []string) error {
				enabledInput := (*bool)(nil)
				if mode == "add" || cmd.Flags().Changed("enabled") {
					value := enabled
					enabledInput = &value
				}
				input := bookingProductUpsertInput{
					ID:          strings.TrimSpace(id),
					Name:        strings.TrimSpace(name),
					Description: strings.TrimSpace(description),
					Enabled:     enabledInput,
				}
				product, err := newService().UpsertProduct(input)
				if err != nil {
					return err
				}

				if app != nil {
					app.SetExecution("booking", []string{"product", mode, product.ID})
					app.SetResult(bookingCommandResult{
						Mode:    "booking_product_" + mode,
						Status:  "ok",
						Product: &product,
					})
				}
				fmt.Printf("booking product upserted: id=%s enabled=%t name=%s\n", product.ID, product.Enabled, product.Name)
				return nil
			},
		}
		cmd.Flags().StringVar(&id, "id", "", "Product ID")
		cmd.Flags().StringVar(&name, "name", "", "Product name")
		cmd.Flags().StringVar(&description, "description", "", "Product description")
		cmd.Flags().BoolVar(&enabled, "enabled", true, "Whether product is enabled")
		return cmd
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List booking products",
		RunE: func(cmd *cobra.Command, _ []string) error {
			products, err := newService().ListProducts()
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"product", "list"})
				app.SetResult(bookingCommandResult{
					Mode:     "booking_product_list",
					Status:   "ok",
					Products: products,
				})
			}
			fmt.Print(formatBookingProductList(products))
			return nil
		},
	}

	removeCmd := &cobra.Command{
		Use:   "remove <product-id>",
		Short: "Remove booking product",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			productID := strings.TrimSpace(args[0])
			removed, err := newService().RemoveProduct(productID)
			if err != nil {
				return err
			}
			status := "removed"
			if !removed {
				status = "not_found"
			}
			if app != nil {
				app.SetExecution("booking", []string{"product", "remove", productID})
				app.SetResult(bookingCommandResult{
					Mode:    "booking_product_remove",
					Status:  status,
					Message: fmt.Sprintf("product %s", status),
				})
			}
			fmt.Printf("booking product %s: id=%s\n", status, productID)
			return nil
		},
	}

	productCmd.AddCommand(addOrUpdate("add"))
	productCmd.AddCommand(addOrUpdate("update"))
	productCmd.AddCommand(listCmd)
	productCmd.AddCommand(removeCmd)
	return productCmd
}

func newBookingSlotCommand(app *AppContext, newService func() *bookingService) *cobra.Command {
	slotCmd := &cobra.Command{
		Use:   "slot",
		Short: "Manage booking slots",
	}

	var (
		id              string
		productID       string
		startAt         string
		endAt           string
		capacity        int
		enabled         bool
		includeFull     bool
		includeDisabled bool
		from            string
		to              string
	)

	addOrUpdate := func(mode string) *cobra.Command {
		cmd := &cobra.Command{
			Use:   mode,
			Short: "Upsert booking slot",
			RunE: func(cmd *cobra.Command, _ []string) error {
				var capacityInput *int
				if mode == "add" || cmd.Flags().Changed("capacity") {
					value := capacity
					capacityInput = &value
				}
				enabledInput := (*bool)(nil)
				if mode == "add" || cmd.Flags().Changed("enabled") {
					value := enabled
					enabledInput = &value
				}
				input := bookingSlotUpsertInput{
					ID:        strings.TrimSpace(id),
					ProductID: strings.TrimSpace(productID),
					StartAt:   strings.TrimSpace(startAt),
					EndAt:     strings.TrimSpace(endAt),
					Capacity:  capacityInput,
					Enabled:   enabledInput,
				}
				slot, err := newService().UpsertSlot(input)
				if err != nil {
					return err
				}

				if app != nil {
					app.SetExecution("booking", []string{"slot", mode, slot.ID})
					app.SetResult(bookingCommandResult{
						Mode:   "booking_slot_" + mode,
						Status: "ok",
						Slot:   &slot,
					})
				}
				fmt.Printf(
					"booking slot upserted: id=%s product_id=%s start=%s end=%s capacity=%d enabled=%t\n",
					slot.ID,
					slot.ProductID,
					slot.StartAt,
					slot.EndAt,
					slot.Capacity,
					slot.Enabled,
				)
				return nil
			},
		}
		cmd.Flags().StringVar(&id, "id", "", "Slot ID")
		cmd.Flags().StringVar(&productID, "product-id", "", "Product ID")
		cmd.Flags().StringVar(&startAt, "start", "", "Slot start time (RFC3339)")
		cmd.Flags().StringVar(&endAt, "end", "", "Slot end time (RFC3339)")
		cmd.Flags().IntVar(&capacity, "capacity", 0, "Slot capacity")
		cmd.Flags().BoolVar(&enabled, "enabled", true, "Whether slot is enabled")
		return cmd
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List booking slots",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fromValue, err := parseOptionalBookingTimeFlag(from, "from")
			if err != nil {
				return err
			}
			toValue, err := parseOptionalBookingTimeFlag(to, "to")
			if err != nil {
				return err
			}
			slots, err := newService().ListSlots(bookingSlotListFilter{
				ProductID:       strings.TrimSpace(productID),
				From:            fromValue,
				To:              toValue,
				IncludeFull:     includeFull,
				IncludeDisabled: includeDisabled,
			})
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"slot", "list"})
				app.SetResult(bookingCommandResult{
					Mode:   "booking_slot_list",
					Status: "ok",
					Slots:  slots,
				})
			}
			fmt.Print(formatBookingSlotList(slots))
			return nil
		},
	}
	listCmd.Flags().StringVar(&productID, "product-id", "", "Filter by product ID")
	listCmd.Flags().StringVar(&from, "from", "", "Filter from start time (RFC3339)")
	listCmd.Flags().StringVar(&to, "to", "", "Filter to start time (RFC3339)")
	listCmd.Flags().BoolVar(&includeFull, "include-full", true, "Include full slots with no available capacity")
	listCmd.Flags().BoolVar(&includeDisabled, "include-disabled", true, "Include disabled slots")

	removeCmd := &cobra.Command{
		Use:   "remove <slot-id>",
		Short: "Remove booking slot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slotID := strings.TrimSpace(args[0])
			removed, err := newService().RemoveSlot(slotID)
			if err != nil {
				return err
			}
			status := "removed"
			if !removed {
				status = "not_found"
			}
			if app != nil {
				app.SetExecution("booking", []string{"slot", "remove", slotID})
				app.SetResult(bookingCommandResult{
					Mode:    "booking_slot_remove",
					Status:  status,
					Message: fmt.Sprintf("slot %s", status),
				})
			}
			fmt.Printf("booking slot %s: id=%s\n", status, slotID)
			return nil
		},
	}

	slotCmd.AddCommand(addOrUpdate("add"))
	slotCmd.AddCommand(addOrUpdate("update"))
	slotCmd.AddCommand(listCmd)
	slotCmd.AddCommand(removeCmd)
	return slotCmd
}

func newBookingReservationCommand(app *AppContext, newService func() *bookingService) *cobra.Command {
	reservationCmd := &cobra.Command{
		Use:   "reservation",
		Short: "Manage booking reservations",
	}

	var (
		productID           string
		slotID              string
		userID              string
		partySize           int
		contactName         string
		contactPhone        string
		members             []string
		specialRequirements string
		status              string
		note                string
	)

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create booking reservation request",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reservation, err := newService().CreateReservation(bookingReservationCreateInput{
				ProductID: strings.TrimSpace(productID),
				SlotID:    strings.TrimSpace(slotID),
				UserID:    strings.TrimSpace(userID),
				PartySize: partySize,
				Personnel: bookingReservationPersonnel{
					ContactName:  strings.TrimSpace(contactName),
					ContactPhone: strings.TrimSpace(contactPhone),
					Members:      append([]string(nil), members...),
				},
				SpecialRequirements: strings.TrimSpace(specialRequirements),
			})
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"reservation", "create", reservation.ID})
				app.SetResult(bookingCommandResult{
					Mode:        "booking_reservation_create",
					Status:      "ok",
					Reservation: &reservation,
				})
			}
			fmt.Printf("booking reservation created: id=%s status=%s product_id=%s slot_id=%s user_id=%s party_size=%d\n",
				reservation.ID, reservation.Status, reservation.ProductID, reservation.SlotID, reservation.UserID, reservation.PartySize)
			return nil
		},
	}
	createCmd.Flags().StringVar(&productID, "product-id", "", "Product ID")
	createCmd.Flags().StringVar(&slotID, "slot-id", "", "Slot ID")
	createCmd.Flags().StringVar(&userID, "user-id", "", "User ID")
	createCmd.Flags().IntVar(&partySize, "party-size", 0, "Party size (>0)")
	createCmd.Flags().StringVar(&contactName, "contact-name", "", "Personnel contact name")
	createCmd.Flags().StringVar(&contactPhone, "contact-phone", "", "Personnel contact phone")
	createCmd.Flags().StringSliceVar(&members, "member", nil, "Optional member names (repeatable)")
	createCmd.Flags().StringVar(&specialRequirements, "special-requirements", "", "Special requirements")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List booking reservations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reservations, err := newService().ListReservations(bookingReservationListFilter{
				UserID: strings.TrimSpace(userID),
				Status: strings.TrimSpace(status),
			})
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"reservation", "list"})
				app.SetResult(bookingCommandResult{
					Mode:         "booking_reservation_list",
					Status:       "ok",
					Reservations: reservations,
				})
			}
			fmt.Print(formatBookingReservationList(reservations))
			return nil
		},
	}
	listCmd.Flags().StringVar(&userID, "user-id", "", "Filter by user ID")
	listCmd.Flags().StringVar(&status, "status", "", "Filter by reservation status (pending|confirmed|rejected|cancelled)")

	transition := func(mode string, action func(*bookingService, string, string) (bookingReservationView, error)) *cobra.Command {
		return &cobra.Command{
			Use:   mode + " <reservation-id>",
			Short: "Transition reservation status",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reservationID := strings.TrimSpace(args[0])
				reservation, err := action(newService(), reservationID, strings.TrimSpace(note))
				if err != nil {
					return err
				}
				if app != nil {
					app.SetExecution("booking", []string{"reservation", mode, reservation.ID})
					app.SetResult(bookingCommandResult{
						Mode:        "booking_reservation_" + mode,
						Status:      "ok",
						Reservation: &reservation,
					})
				}
				fmt.Printf("booking reservation %s: id=%s status=%s\n", mode, reservation.ID, reservation.Status)
				return nil
			},
		}
	}

	confirmCmd := transition("confirm", (*bookingService).ConfirmReservation)
	rejectCmd := transition("reject", (*bookingService).RejectReservation)
	cancelCmd := transition("cancel", (*bookingService).CancelReservation)
	confirmCmd.Flags().StringVar(&note, "note", "", "Optional system note")
	rejectCmd.Flags().StringVar(&note, "note", "", "Optional system note")
	cancelCmd.Flags().StringVar(&note, "note", "", "Optional system note")

	reservationCmd.AddCommand(createCmd)
	reservationCmd.AddCommand(listCmd)
	reservationCmd.AddCommand(confirmCmd)
	reservationCmd.AddCommand(rejectCmd)
	reservationCmd.AddCommand(cancelCmd)
	return reservationCmd
}

func newBookingQueryCommand(app *AppContext, newService func() *bookingService) *cobra.Command {
	var (
		productID   string
		from        string
		to          string
		includeFull bool
	)
	queryCmd := &cobra.Command{
		Use:   "query",
		Short: "Query available booking catalog (user view)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fromValue, err := parseOptionalBookingTimeFlag(from, "from")
			if err != nil {
				return err
			}
			toValue, err := parseOptionalBookingTimeFlag(to, "to")
			if err != nil {
				return err
			}
			result, err := newService().QueryCatalog(bookingCatalogQuery{
				ProductID:   strings.TrimSpace(productID),
				From:        fromValue,
				To:          toValue,
				IncludeFull: includeFull,
			})
			if err != nil {
				return err
			}
			if app != nil {
				app.SetExecution("booking", []string{"query"})
				app.SetResult(bookingCommandResult{
					Mode:   "booking_query",
					Status: "ok",
					Query:  &result,
				})
			}
			fmt.Print(formatBookingCatalogQueryResult(result))
			return nil
		},
	}
	queryCmd.Flags().StringVar(&productID, "product-id", "", "Filter by product ID")
	queryCmd.Flags().StringVar(&from, "from", "", "Filter from start time (RFC3339)")
	queryCmd.Flags().StringVar(&to, "to", "", "Filter to start time (RFC3339)")
	queryCmd.Flags().BoolVar(&includeFull, "include-full", false, "Include full slots with no available capacity")
	return queryCmd
}

func newBookingAgentCommand(app *AppContext) *cobra.Command {
	var (
		userID                      string
		content                     string
		channel                     string
		thirdPartyID                string
		rawToken                    string
		securityKeysPath            string
		publicURL                   string
		runtimePath                 string
		timeout                     time.Duration
		idempotencyKey              string
		overrideProductID           string
		overrideSlotID              string
		overridePartySize           int
		overrideContactName         string
		overrideContactPhone        string
		overrideMembers             []string
		overrideSpecialRequirements string
	)

	agentCmd := &cobra.Command{
		Use:   "agent",
		Short: "Simulate AI-driven booking flows via public APIs",
	}

	reserveCmd := &cobra.Command{
		Use:   "reserve",
		Short: "Run parse + confirm flow against booking public API",
		RunE: func(cmd *cobra.Command, _ []string) (runErr error) {
			result := bookingAgentReserveResult{}
			if app != nil {
				app.SetExecution("booking", []string{"agent", "reserve"})
				defer func() {
					status := "ok"
					message := ""
					if runErr != nil {
						status = "error"
						message = strings.TrimSpace(runErr.Error())
					}
					app.SetResult(bookingCommandResult{
						Mode:         "booking_agent_reserve",
						Status:       status,
						Message:      message,
						AgentReserve: &result,
					})
				}()
			}

			trimmedThirdPartyID := normalizeThirdPartyID(thirdPartyID)
			if trimmedThirdPartyID == "" {
				return fmt.Errorf("third-party-id is required")
			}
			trimmedUserID := strings.TrimSpace(userID)
			trimmedContent := strings.TrimSpace(content)
			trimmedChannel := strings.TrimSpace(channel)
			if trimmedUserID == "" {
				return fmt.Errorf("user-id is required")
			}
			if trimmedContent == "" {
				return fmt.Errorf("content is required")
			}
			if trimmedChannel == "" {
				trimmedChannel = "chat"
			}
			if timeout <= 0 {
				return fmt.Errorf("timeout must be > 0")
			}
			if cmd.Flags().Changed("party-size") && overridePartySize <= 0 {
				return fmt.Errorf("party-size must be > 0 when provided")
			}

			token, err := resolveBookingAgentToken(rawToken, securityKeysPath, trimmedThirdPartyID)
			if err != nil {
				return err
			}
			baseURL, err := resolveBookingAgentPublicBaseURL(publicURL, runtimePath)
			if err != nil {
				return err
			}
			parseURL, err := buildBookingAgentEndpointURL(baseURL, bookingPublicIntentParsePath)
			if err != nil {
				return err
			}
			confirmURL, err := buildBookingAgentEndpointURL(baseURL, bookingPublicIntentConfirmPath)
			if err != nil {
				return err
			}
			parseIdempotencyKey, confirmIdempotencyKey := bookingAgentIdempotencyPair(idempotencyKey)

			result.PublicBaseURL = baseURL
			result.ThirdPartyID = trimmedThirdPartyID
			result.IdempotencyParse = parseIdempotencyKey
			result.IdempotencyConfirm = confirmIdempotencyKey
			result.Parse = bookingAgentReserveStepResult{
				TargetURL:      parseURL,
				IdempotencyKey: parseIdempotencyKey,
			}

			parsePayload := bookingIntentParseRequest{
				UserID:  trimmedUserID,
				Content: trimmedContent,
				Channel: trimmedChannel,
			}
			parseRaw, err := json.Marshal(parsePayload)
			if err != nil {
				return err
			}
			client := bookingAgentHTTPClientFactory(timeout)
			parseEnvelope, parseHTTPStatus, err := bookingAgentSignedPOST(client, parseURL, trimmedThirdPartyID, token, parseRaw, parseIdempotencyKey, time.Now())
			if err != nil {
				return fmt.Errorf("booking parse request failed: %w", err)
			}
			result.Parse.HTTPStatus = parseHTTPStatus
			result.Parse.Code = strings.TrimSpace(parseEnvelope.Code)
			result.Parse.Message = strings.TrimSpace(parseEnvelope.Message)
			result.Parse.RequestID = strings.TrimSpace(parseEnvelope.RequestID)

			if strings.TrimSpace(parseEnvelope.Status) != publicAPIStatusOK {
				return bookingAgentPublicError("booking parse", parseHTTPStatus, parseEnvelope)
			}
			parseData, err := decodeBookingAgentParseEnvelopeData(parseEnvelope.Data)
			if err != nil {
				return fmt.Errorf("decode parse response failed: %w", err)
			}
			draftID := strings.TrimSpace(parseData.DraftID)
			if draftID == "" {
				draftID = strings.TrimSpace(parseData.Draft.ID)
			}
			if draftID == "" {
				return fmt.Errorf("parse response missing draft_id")
			}
			result.DraftID = draftID
			if len(parseData.Draft.MissingFields) > 0 {
				result.MissingFields = append([]string(nil), parseData.Draft.MissingFields...)
			}
			extracted := parseData.Draft.Extracted
			result.ParseExtracted = &extracted

			confirmPayload := bookingIntentConfirmRequest{
				DraftID: draftID,
			}
			if value := strings.TrimSpace(overrideProductID); value != "" {
				confirmPayload.ProductID = value
			}
			if value := strings.TrimSpace(overrideSlotID); value != "" {
				confirmPayload.SlotID = value
			}
			if cmd.Flags().Changed("party-size") {
				confirmPayload.PartySize = overridePartySize
			}
			trimmedContactName := strings.TrimSpace(overrideContactName)
			trimmedContactPhone := strings.TrimSpace(overrideContactPhone)
			trimmedMembers := make([]string, 0, len(overrideMembers))
			for _, member := range overrideMembers {
				member = strings.TrimSpace(member)
				if member == "" {
					continue
				}
				trimmedMembers = append(trimmedMembers, member)
			}
			if trimmedContactName != "" || trimmedContactPhone != "" || len(trimmedMembers) > 0 {
				confirmPayload.Personnel = &bookingReservationPersonnel{
					ContactName:  trimmedContactName,
					ContactPhone: trimmedContactPhone,
					Members:      trimmedMembers,
				}
			}
			if value := strings.TrimSpace(overrideSpecialRequirements); value != "" {
				confirmPayload.SpecialRequirements = value
			}
			merged := mergeBookingDraftForConfirm(extracted, confirmPayload)
			missingFields := bookingIntentMissingFields(merged)
			result.MissingFields = append([]string(nil), missingFields...)
			if len(missingFields) > 0 {
				result.ConfirmPayload = bookingAgentConfirmPayloadPreview(confirmPayload)
				return fmt.Errorf(
					"missing_fields: %s; provide overrides via %s",
					strings.Join(missingFields, ", "),
					strings.Join(bookingAgentSuggestedFlags(missingFields), ", "),
				)
			}

			result.Confirm = &bookingAgentReserveStepResult{
				TargetURL:      confirmURL,
				IdempotencyKey: confirmIdempotencyKey,
			}
			confirmRaw, err := json.Marshal(confirmPayload)
			if err != nil {
				return err
			}
			confirmEnvelope, confirmHTTPStatus, err := bookingAgentSignedPOST(client, confirmURL, trimmedThirdPartyID, token, confirmRaw, confirmIdempotencyKey, time.Now())
			if err != nil {
				return fmt.Errorf("booking confirm request failed: %w", err)
			}
			result.Confirm.HTTPStatus = confirmHTTPStatus
			result.Confirm.Code = strings.TrimSpace(confirmEnvelope.Code)
			result.Confirm.Message = strings.TrimSpace(confirmEnvelope.Message)
			result.Confirm.RequestID = strings.TrimSpace(confirmEnvelope.RequestID)

			if strings.TrimSpace(confirmEnvelope.Status) != publicAPIStatusOK {
				return bookingAgentPublicError("booking confirm", confirmHTTPStatus, confirmEnvelope)
			}
			confirmData, err := decodeBookingAgentConfirmEnvelopeData(confirmEnvelope.Data)
			if err != nil {
				return fmt.Errorf("decode confirm response failed: %w", err)
			}
			result.ReservationID = strings.TrimSpace(confirmData.ReservationID)
			result.FinalStatus = strings.TrimSpace(confirmData.Reservation.Status)
			if result.FinalStatus == "" {
				result.FinalStatus = "pending"
			}
			if len(result.MissingFields) == 0 {
				result.MissingFields = nil
			}

			fmt.Printf(
				"booking agent reserve parse_code=%s draft_id=%s confirm_code=%s reservation_id=%s status=%s\n",
				result.Parse.Code,
				result.DraftID,
				result.Confirm.Code,
				result.ReservationID,
				result.FinalStatus,
			)
			return nil
		},
	}

	reserveCmd.Flags().StringVar(&userID, "user-id", "", "Booking user ID")
	reserveCmd.Flags().StringVar(&content, "content", "", "Booking intent content text")
	reserveCmd.Flags().StringVar(&thirdPartyID, "third-party-id", "", "Third-party ID used for signed headers")
	reserveCmd.Flags().StringVar(&rawToken, "token", "", "Raw token used for request signing (overrides --security-keys)")
	reserveCmd.Flags().StringVar(&securityKeysPath, "security-keys", defaultSecurityKeysPath(), "Path to unified security keys JSON")
	reserveCmd.Flags().StringVar(&channel, "channel", "chat", "Request channel value for parse API")
	reserveCmd.Flags().StringVar(&publicURL, "url", "", "Booking public base URL, e.g. http://127.0.0.1:18081")
	reserveCmd.Flags().StringVar(&runtimePath, "runtime", defaultBookingRuntimeStatePath(), "Path to booking runtime state JSON")
	reserveCmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "HTTP timeout for parse/confirm requests")
	reserveCmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key prefix (command appends -parse/-confirm)")
	reserveCmd.Flags().StringVar(&overrideProductID, "product-id", "", "Override product_id before confirm")
	reserveCmd.Flags().StringVar(&overrideSlotID, "slot-id", "", "Override slot_id before confirm")
	reserveCmd.Flags().IntVar(&overridePartySize, "party-size", 0, "Override party_size before confirm")
	reserveCmd.Flags().StringVar(&overrideContactName, "contact-name", "", "Override personnel.contact_name before confirm")
	reserveCmd.Flags().StringVar(&overrideContactPhone, "contact-phone", "", "Override personnel.contact_phone before confirm")
	reserveCmd.Flags().StringSliceVar(&overrideMembers, "member", nil, "Override personnel.members before confirm (repeatable)")
	reserveCmd.Flags().StringVar(&overrideSpecialRequirements, "special-requirements", "", "Override special_requirements before confirm")

	agentCmd.AddCommand(reserveCmd)
	return agentCmd
}

func resolveBookingAgentToken(rawToken string, securityKeysPath string, thirdPartyID string) (string, error) {
	if token := strings.TrimSpace(rawToken); token != "" {
		return token, nil
	}
	records, err := loadSecurityTokenRecords(strings.TrimSpace(securityKeysPath), false)
	if err != nil {
		return "", err
	}
	record, exists := records[normalizeThirdPartyID(thirdPartyID)]
	if !exists {
		return "", fmt.Errorf("token not found for third-party-id %q in %s", thirdPartyID, strings.TrimSpace(securityKeysPath))
	}
	if !securityRecordHasScope(record, securityScopeBooking) {
		return "", fmt.Errorf("token scope not allowed for booking: third-party-id %q", thirdPartyID)
	}
	if strings.TrimSpace(record.Token) == "" {
		return "", fmt.Errorf("token is empty for third-party-id %q", thirdPartyID)
	}
	return record.Token, nil
}

func resolveBookingAgentPublicBaseURL(rawURL string, runtimePath string) (string, error) {
	trimmedURL := strings.TrimSpace(rawURL)
	if trimmedURL != "" {
		return normalizeBookingAgentBaseURL(trimmedURL)
	}
	status, err := bookingServiceStatus(strings.TrimSpace(runtimePath))
	if err != nil {
		return "", err
	}
	if !status.Running || status.Runtime == nil {
		return "", fmt.Errorf("booking service is not running; start it first with `go run . booking service start`")
	}
	publicAddress := bookingRuntimePublicAddress(status.Runtime)
	if strings.TrimSpace(publicAddress) == "" {
		return "", fmt.Errorf("booking runtime is missing public address; restart service with `go run . booking service start`")
	}
	return normalizeBookingAgentBaseURL(buildWebhookManagementURL(publicAddress, "/"))
}

func normalizeBookingAgentBaseURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if strings.TrimSpace(parsed.Scheme) == "" || strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("url must include scheme and host")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func buildBookingAgentEndpointURL(baseURL string, endpointPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	prefix := strings.TrimRight(parsed.Path, "/")
	parsed.Path = prefix + ensureWebhookPath(endpointPath)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func bookingAgentIdempotencyPair(raw string) (string, string) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" {
		prefix = fmt.Sprintf("booking-agent-reserve-%d", time.Now().UnixNano())
	}
	return prefix + "-parse", prefix + "-confirm"
}

func bookingAgentSignedPOST(
	client *http.Client,
	targetURL string,
	thirdPartyID string,
	token string,
	body []byte,
	idempotencyKey string,
	now time.Time,
) (publicAPIEnvelope, int, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := computeWebhookSignature(thirdPartyID, timestamp, token, body)
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return publicAPIEnvelope{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhookHeaderThirdPartyID, thirdPartyID)
	req.Header.Set(webhookHeaderTimestamp, timestamp)
	req.Header.Set(webhookHeaderSignature, signature)
	req.Header.Set(publicAPIHeaderIdempotencyKey, strings.TrimSpace(idempotencyKey))

	resp, err := client.Do(req)
	if err != nil {
		return publicAPIEnvelope{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return publicAPIEnvelope{}, resp.StatusCode, err
	}
	envelope, err := readPublicAPIEnvelope(raw)
	if err != nil {
		trimmed := strings.TrimSpace(string(raw))
		if len(trimmed) > 512 {
			trimmed = trimmed[:512]
		}
		return publicAPIEnvelope{}, resp.StatusCode, fmt.Errorf("response is not valid public envelope: http=%d body=%s", resp.StatusCode, trimmed)
	}
	return envelope, resp.StatusCode, nil
}

func bookingAgentPublicError(stage string, httpStatus int, envelope publicAPIEnvelope) error {
	code := strings.TrimSpace(envelope.Code)
	if code == "" {
		code = publicAPIErrorCodeInternalError
	}
	message := strings.TrimSpace(envelope.Message)
	if message == "" {
		message = "request failed"
	}
	requestID := strings.TrimSpace(envelope.RequestID)
	return fmt.Errorf("%s failed: code=%s message=%s request_id=%s http_status=%d", stage, code, message, requestID, httpStatus)
}

func decodeBookingAgentParseEnvelopeData(data any) (bookingAgentReserveParseEnvelopeData, error) {
	var payload bookingAgentReserveParseEnvelopeData
	encoded, err := json.Marshal(data)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return payload, err
	}
	payload.DraftID = strings.TrimSpace(payload.DraftID)
	payload.Draft.ID = strings.TrimSpace(payload.Draft.ID)
	payload.Draft.MissingFields = bookingAgentNormalizeFields(payload.Draft.MissingFields)
	return payload, nil
}

func decodeBookingAgentConfirmEnvelopeData(data any) (bookingAgentReserveConfirmEnvelopeData, error) {
	var payload bookingAgentReserveConfirmEnvelopeData
	encoded, err := json.Marshal(data)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return payload, err
	}
	payload.DraftID = strings.TrimSpace(payload.DraftID)
	payload.ReservationID = strings.TrimSpace(payload.ReservationID)
	payload.Reservation.Status = strings.TrimSpace(payload.Reservation.Status)
	return payload, nil
}

func bookingAgentNormalizeFields(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	out := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if _, exists := seen[field]; exists {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

func bookingAgentSuggestedFlags(missingFields []string) []string {
	if len(missingFields) == 0 {
		return nil
	}
	fieldToFlag := map[string]string{
		"product_id":              "--product-id",
		"slot_id":                 "--slot-id",
		"party_size":              "--party-size",
		"personnel.contact_name":  "--contact-name",
		"personnel.contact_phone": "--contact-phone",
	}
	flags := make([]string, 0, len(missingFields))
	seen := make(map[string]struct{}, len(missingFields))
	for _, field := range missingFields {
		flag, exists := fieldToFlag[strings.TrimSpace(field)]
		if !exists {
			continue
		}
		if _, ok := seen[flag]; ok {
			continue
		}
		seen[flag] = struct{}{}
		flags = append(flags, flag)
	}
	if len(flags) == 0 {
		return []string{"--product-id", "--slot-id", "--party-size", "--contact-name", "--contact-phone"}
	}
	sort.Strings(flags)
	return flags
}

func bookingAgentConfirmPayloadPreview(payload bookingIntentConfirmRequest) map[string]any {
	preview := map[string]any{
		"draft_id": strings.TrimSpace(payload.DraftID),
	}
	if value := strings.TrimSpace(payload.ProductID); value != "" {
		preview["product_id"] = value
	}
	if value := strings.TrimSpace(payload.SlotID); value != "" {
		preview["slot_id"] = value
	}
	if payload.PartySize > 0 {
		preview["party_size"] = payload.PartySize
	}
	if payload.Personnel != nil {
		preview["personnel"] = payload.Personnel
	}
	if value := strings.TrimSpace(payload.SpecialRequirements); value != "" {
		preview["special_requirements"] = value
	}
	return preview
}

func parseOptionalBookingTimeFlag(raw string, field string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := parseBookingRFC3339(trimmed, field)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func formatBookingProductList(products []bookingProduct) string {
	if len(products) == 0 {
		return "no booking product\n"
	}
	var builder strings.Builder
	tw := tabwriter.NewWriter(&builder, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tENABLED\tNAME\tDESCRIPTION\tUPDATED_AT")
	for _, product := range products {
		fmt.Fprintf(
			tw,
			"%s\t%t\t%s\t%s\t%s\n",
			product.ID,
			product.Enabled,
			product.Name,
			product.Description,
			product.UpdatedAt,
		)
	}
	_ = tw.Flush()
	return builder.String()
}

func formatBookingSlotList(slots []bookingSlotView) string {
	if len(slots) == 0 {
		return "no booking slot\n"
	}
	var builder strings.Builder
	tw := tabwriter.NewWriter(&builder, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRODUCT_ID\tPRODUCT\tSTART\tEND\tCAPACITY\tRESERVED\tAVAILABLE\tENABLED")
	for _, slot := range slots {
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%t\n",
			slot.ID,
			slot.ProductID,
			slot.ProductName,
			slot.StartAt,
			slot.EndAt,
			slot.Capacity,
			slot.Reserved,
			slot.AvailableCapacity,
			slot.Enabled,
		)
	}
	_ = tw.Flush()
	return builder.String()
}

func formatBookingReservationList(reservations []bookingReservationView) string {
	if len(reservations) == 0 {
		return "no booking reservation\n"
	}
	var builder strings.Builder
	tw := tabwriter.NewWriter(&builder, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tUSER_ID\tPRODUCT_ID\tSLOT_ID\tPARTY_SIZE\tCONTACT\tPHONE\tCREATED_AT\tUPDATED_AT")
	for _, reservation := range reservations {
		fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			reservation.ID,
			reservation.Status,
			reservation.UserID,
			reservation.ProductID,
			reservation.SlotID,
			reservation.PartySize,
			reservation.Personnel.ContactName,
			reservation.Personnel.ContactPhone,
			reservation.CreatedAt,
			reservation.UpdatedAt,
		)
	}
	_ = tw.Flush()
	return builder.String()
}

func formatBookingCatalogQueryResult(result bookingCatalogQueryResult) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("products=%d slots=%d\n", len(result.Products), len(result.Slots)))
	if len(result.Products) > 0 {
		builder.WriteString("\nproducts:\n")
		builder.WriteString(formatBookingProductList(result.Products))
	}
	if len(result.Slots) > 0 {
		builder.WriteString("\nslots:\n")
		builder.WriteString(formatBookingSlotList(result.Slots))
	}
	if len(result.Products) == 0 && len(result.Slots) == 0 {
		builder.WriteString("no available booking slot\n")
	}
	return builder.String()
}
