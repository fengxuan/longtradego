package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

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
}

func newBookingCommand(app *appContext) *cobra.Command {
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
	bookingCmd.AddCommand(newBookingServiceCommand(app, func() *bookingServiceServeConfig {
		cfg := newBookingServiceServeConfig()
		cfg.CatalogPath = strings.TrimSpace(catalogPath)
		cfg.ReservationsPath = strings.TrimSpace(reservationsPath)
		return cfg
	}))
	return bookingCmd
}

func newBookingServiceCommand(app *appContext, configFactory func() *bookingServiceServeConfig) *cobra.Command {
	var (
		publicAddr        string
		adminAddr         string
		runtimePath       string
		logPath           string
		draftsPath        string
		securityKeysPath  string
		legacyAPIKeysPath string
		llmConfigPath     string
		adminAuthPath     string
		timeout           time.Duration
		maxPortFallback   int
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
			resolvedSecurityPath, err := resolveSecurityKeysPath(securityKeysPath, legacyAPIKeysPath, "api-keys")
			if err != nil {
				return err
			}
			cfg.PublicAddr = strings.TrimSpace(publicAddr)
			cfg.Addr = cfg.PublicAddr
			cfg.AdminAddr = strings.TrimSpace(adminAddr)
			cfg.RuntimePath = strings.TrimSpace(runtimePath)
			cfg.DraftsPath = strings.TrimSpace(draftsPath)
			cfg.APIKeysPath = resolvedSecurityPath
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
			resolvedSecurityPath, err := resolveSecurityKeysPath(securityKeysPath, legacyAPIKeysPath, "api-keys")
			if err != nil {
				return err
			}
			cfg.PublicAddr = strings.TrimSpace(publicAddr)
			cfg.Addr = cfg.PublicAddr
			cfg.AdminAddr = strings.TrimSpace(adminAddr)
			cfg.RuntimePath = strings.TrimSpace(runtimePath)
			cfg.DraftsPath = strings.TrimSpace(draftsPath)
			cfg.APIKeysPath = resolvedSecurityPath
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
	serviceCmd.PersistentFlags().StringVar(&publicAddr, "addr", defaultBookingServiceAddr, "Public listen address (legacy alias of --public-addr)")
	serviceCmd.PersistentFlags().StringVar(&adminAddr, "admin-addr", defaultBookingServiceAdminAddr, "Admin listen address, e.g. 127.0.0.1:18082")
	serviceCmd.PersistentFlags().StringVar(&runtimePath, "runtime", defaultBookingRuntimeStatePath(), "Path to booking runtime state JSON")
	serviceCmd.PersistentFlags().StringVar(&draftsPath, "drafts", defaultBookingIntakeDraftsPath(), "Path to booking intake drafts JSON")
	serviceCmd.PersistentFlags().StringVar(&securityKeysPath, "security-keys", "", "Path to unified security keys JSON")
	serviceCmd.PersistentFlags().StringVar(&legacyAPIKeysPath, "api-keys", "", "Path to unified security keys JSON (legacy alias)")
	serviceCmd.PersistentFlags().StringVar(&llmConfigPath, "llm-config", defaultBookingLLMConfigPath(), "Path to booking LLM config JSON")
	serviceCmd.PersistentFlags().StringVar(&adminAuthPath, "admin-auth", defaultDaemonAdminAuthConfigPath(), "Path to admin basic auth config JSON")
	serviceCmd.PersistentFlags().IntVar(&maxPortFallback, "max-port-fallback", defaultBookingServicePortFallback, "Fallback attempts when preferred port is occupied")

	startCmd.Flags().StringVar(&logPath, "log-file", defaultBookingServiceLogPath(), "Path to booking service log file")
	stopCmd.Flags().DurationVar(&timeout, "timeout", defaultBookingServiceStopTimeout, "Graceful stop timeout, e.g. 5s")

	serviceCmd.AddCommand(startCmd, stopCmd, statusCmd, serveCmd)
	return serviceCmd
}

func newBookingProductCommand(app *appContext, newService func() *bookingService) *cobra.Command {
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

func newBookingSlotCommand(app *appContext, newService func() *bookingService) *cobra.Command {
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

func newBookingReservationCommand(app *appContext, newService func() *bookingService) *cobra.Command {
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

func newBookingQueryCommand(app *appContext, newService func() *bookingService) *cobra.Command {
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
