package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gochathub/gochathub-server/internal/app"
	"github.com/gochathub/gochathub-server/internal/httpapi"
	"github.com/gochathub/gochathub-server/internal/model"
	"github.com/gochathub/gochathub-server/internal/service"
	"github.com/gochathub/gochathub-server/internal/storage"
)

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the API + WebSocket server",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if os.Getenv("MIGRATIONS_ON_SERVE") == "1" {
				if err := migrateUp(ctx, b); err != nil {
					return err
				}
			}
			svc, hub, err := app.Assemble(b.q, b.cfg, b.log)
			if err != nil {
				return err
			}
			api := httpapi.New(svc, hub, b.log, b.cfg)
			srv := &http.Server{
				Addr:              b.cfg.ListenAddr,
				Handler:           api,
				ReadHeaderTimeout: 10 * time.Second,
			}
			b.log.Info("listening", "addr", b.cfg.ListenAddr)
			errCh := make(chan error, 1)
			go func() { errCh <- srv.ListenAndServe() }()
			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
	}
}

// assembleFor builds the service graph for CLI commands (same layer as HTTP).
// Push/storage failures degrade: CLI ops work without external infra.
func assembleFor(b *boot) (*service.App, error) {
	svc, _, err := app.Assemble(b.q, b.cfg, b.log)
	if err != nil {
		b.log.Warn("partial assembly; continuing without push/storage", "err", err)
		return minimalApp(b), nil
	}
	return svc, nil
}

// minimalApp wires services that need no external infra.
func minimalApp(b *boot) *service.App {
	app := service.App{Store: b.q, Log: b.log, Storage: storage.Disabled{}}
	app.Users = service.UserService{App: &app}
	app.Auth = &service.AuthService{App: &app, SessionTTL: b.cfg.SessionTTL}
	app.Rooms = &service.RoomService{App: &app}
	app.Messages = &service.MessageService{App: &app}
	app.Contacts = &service.ContactService{App: &app}
	app.Invites = &service.InviteService{App: &app}
	app.Attachments = &service.AttachmentService{App: &app}
	app.Devices = &service.DeviceService{App: &app}
	return &app
}

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := migrateUp(cmd.Context(), b); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "migrations applied")
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
		},
	}
}

// --- user commands ---

func userCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "Manage user accounts"}
	c.AddCommand(userCreate(), userList(), userPasswd(), userEnable(), userDisable(), userDelete())
	return c
}

func userCreate() *cobra.Command {
	var email, displayName, role, pwFile string
	var yes bool
	cmd := &cobra.Command{
		Use:   "create <username>",
		Short: "Create a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := readPassword(pwFile)
			if err != nil || pw == "" {
				return fmt.Errorf("password required via --password-stdin")
			}
			_ = yes
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			display := displayName
			if display == "" {
				display = args[0]
			}
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			row, err := app.Users.CreateUser(cmd.Context(), args[0], display, email, pw, role)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), row.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "optional email")
	cmd.Flags().StringVar(&displayName, "display-name", "", "display name (defaults to username)")
	cmd.Flags().StringVar(&role, "role", "user", "user|moderator|admin")
	cmd.Flags().StringVar(&pwFile, "password-stdin", "", "read password from file or '-' for stdin")
	return cmd
}

func userList() *cobra.Command {
	var asJSON bool
	return &cobra.Command{
		Use: "list", Short: "List users",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			rows, err := b.q.ListUsers(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
			}
			for _, u := range rows {
				state := "enabled"
				if !u.Enabled {
					state = "disabled"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", u.ID, u.Username, u.DisplayName, u.Role, state)
			}
			return nil
		},
	}
}

func userPasswd() *cobra.Command {
	var pwFile string
	cmd := &cobra.Command{
		Use: "passwd <username>", Short: "Reset a user's password (revokes sessions)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pw, err := readPassword(pwFile)
			if err != nil || pw == "" {
				return fmt.Errorf("password required via --password-stdin")
			}
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			return app.Users.SetPassword(cmd.Context(), args[0], pw)
		},
	}
	cmd.Flags().StringVar(&pwFile, "password-stdin", "", "read password from file or '-' for stdin")
	return cmd
}

func userEnable() *cobra.Command {
	return &cobra.Command{
		Use: "enable <username>", Short: "Enable an account", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			return app.Users.SetEnabled(cmd.Context(), args[0], true)
		},
	}
}

func userDisable() *cobra.Command {
	return &cobra.Command{
		Use: "disable <username>", Short: "Disable an account", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			return app.Users.SetEnabled(cmd.Context(), args[0], false)
		},
	}
}

func userDelete() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: "delete <username>", Short: "Delete a user permanently", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("refusing to delete without --yes")
			}
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			return app.Users.DeleteUser(cmd.Context(), args[0])
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "required confirmation")
	return cmd
}

// readPassword reads from stdin (or a file for scripts); never logs.
func readPassword(from string) (string, error) {
	if from == "" || from == "-" {
		fmt.Fprint(os.Stderr, "password: ")
		return readLine(os.Stdin)
	}
	f, err := os.Open(from)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readLine(f)
}

func readLine(f io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n == 1 {
			if buf[0] == '\n' || buf[0] == '\r' {
				break
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			break
		}
	}
	return b.String(), nil
}

// --- room commands ---

func roomCmd() *cobra.Command {
	c := &cobra.Command{Use: "room", Short: "Manage rooms"}
	c.AddCommand(roomCreate(), roomList(), roomArchive(), roomMembers(), roomInvite())
	return c
}

func roomCreate() *cobra.Command {
	var typ, description, actingUser string
	cmd := &cobra.Command{
		Use: "create <name>", Short: "Create a room as an acting user", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			creator, err := b.q.UserByUsername(cmd.Context(), actingUser)
			if err != nil {
				return fmt.Errorf("acting user %q: %w", actingUser, err)
			}
			p := service.Principal{UserID: creator.ID, Role: creator.Role, Kind: "api_token"}
			var rt model.RoomType
			switch typ {
			case "public":
				rt = model.RoomPublic
			case "private":
				rt = model.RoomPrivate
			default:
				return fmt.Errorf("--type must be public|private")
			}
			room, err := app.Rooms.Create(cmd.Context(), p, service.CreateInput{
				Type: rt, Name: args[0], Description: description,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), room.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&typ, "type", "public", "public|private")
	cmd.Flags().StringVar(&description, "description", "", "room description")
	cmd.Flags().StringVar(&actingUser, "user", "", "acting username (room creator)")
	return cmd
}

func roomList() *cobra.Command {
	var asJSON bool
	return &cobra.Command{
		Use: "list", Short: "List all rooms",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			rows, err := b.q.ListAllRooms(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
			}
			for _, r := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tarchived=%v\n", r.ID, r.Type, strValue(r.Name), r.ArchivedAt != nil)
			}
			return nil
		},
	}
}

func strValue(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func roomArchive() *cobra.Command {
	return &cobra.Command{
		Use: "archive <room-id>", Short: "Archive a room", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			if err := b.q.ArchiveRoom(cmd.Context(), args[0]); err != nil {
				return err
			}
			_ = b.q.Audit(cmd.Context(), "", "room.archive", "room", args[0], []byte(`{}`), nil)
			return nil
		},
	}
}

func roomMembers() *cobra.Command {
	return &cobra.Command{
		Use: "members <room-id>", Short: "List room members", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			rows, err := b.q.RoomMembers(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, m := range rows {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", m.UserID, m.Role, m.Username)
			}
			return nil
		},
	}
}

func roomInvite() *cobra.Command {
	return &cobra.Command{
		Use: "invite <room-id> <username>", Short: "Add a member directly", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			invitee, err := b.q.UserByUsername(cmd.Context(), args[1])
			if err != nil {
				return fmt.Errorf("invitee %q: %w", args[1], err)
			}
			if err := b.q.AddMember(cmd.Context(), args[0], invitee.ID, "member"); err != nil {
				return err
			}
			_ = b.q.Audit(cmd.Context(), "", "room.add_member", "room", args[0], []byte(`{"user_id":"`+invitee.ID+`"}`), nil)
			return nil
		},
	}
}

// --- token commands ---

func tokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: "Manage API tokens"}
	c.AddCommand(tokenCreate(), tokenRevoke())
	return c
}

func tokenCreate() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use: "create <username>", Short: "Create an API token (raw value printed once)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			app, err := assembleFor(b)
			if err != nil {
				return err
			}
			raw, err := app.Users.CreateAPIToken(cmd.Context(), args[0], name)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), raw) // printed exactly once; never logged
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "cli-token", "token label")
	return cmd
}

func tokenRevoke() *cobra.Command {
	return &cobra.Command{
		Use: "revoke <token-id>", Short: "Revoke an API token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			return b.q.RevokeAPITokenByID(cmd.Context(), args[0])
		},
	}
}
