package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gochathub/gochathub-server/internal/service"
)

// signupInviteCmd: same service as the admin API (ADR-026); --from names the
// acting admin, who is shown to the invitee and recorded in the audit log.
func signupInviteCmd() *cobra.Command {
	var from string
	// run boots the app, resolves the --from admin and hands both to fn.
	run := func(fn func(cmd *cobra.Command, svc *service.App, p service.Principal, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			b, closeFn, err := bootstrap()
			if err != nil {
				return err
			}
			defer closeFn()
			svc, err := assembleFor(b)
			if err != nil {
				return err
			}
			u, err := b.q.UserByUsername(cmd.Context(), from)
			if err != nil {
				return fmt.Errorf("--from %q: %w", from, err)
			}
			return fn(cmd, svc, service.Principal{UserID: u.ID, Username: u.Username, Role: u.Role, Kind: "cli"}, args)
		}
	}
	c := &cobra.Command{Use: "signup-invite", Short: "Manage email signup invites (needs SMTP_HOST)"}
	c.PersistentFlags().StringVar(&from, "from", "", "admin username acting as inviter (required)")
	_ = c.MarkPersistentFlagRequired("from")
	c.AddCommand(
		&cobra.Command{Use: "create <email>", Short: "Email a signup invite", Args: cobra.ExactArgs(1),
			RunE: run(func(cmd *cobra.Command, svc *service.App, p service.Principal, args []string) error {
				inv, err := svc.Signup.Create(cmd.Context(), p, args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tsent\n", inv.ID, inv.Email)
				return nil
			})},
		&cobra.Command{Use: "list", Short: "List signup invites", Args: cobra.NoArgs,
			RunE: run(func(cmd *cobra.Command, svc *service.App, p service.Principal, _ []string) error {
				invs, err := svc.Signup.List(cmd.Context(), p)
				if err != nil {
					return err
				}
				for _, i := range invs {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", i.ID, i.Email, i.Status, i.ExpiresAt.Format(time.RFC3339))
				}
				return nil
			})},
		&cobra.Command{Use: "resend <id>", Short: "Rotate the token and email the invite again", Args: cobra.ExactArgs(1),
			RunE: run(func(cmd *cobra.Command, svc *service.App, p service.Principal, args []string) error {
				_, err := svc.Signup.Resend(cmd.Context(), p, args[0])
				return err
			})},
		&cobra.Command{Use: "revoke <id>", Short: "Revoke an open invite", Args: cobra.ExactArgs(1),
			RunE: run(func(cmd *cobra.Command, svc *service.App, p service.Principal, args []string) error {
				return svc.Signup.Revoke(cmd.Context(), p, args[0])
			})},
	)
	return c
}
