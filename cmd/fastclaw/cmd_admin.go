package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// adminCmd groups the admin-only CLI operations: create users, reset
// passwords, grant roles. These bypass the HTTP API and write to the DB
// directly so an operator who's lost super_admin access can recover.
func adminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative DB operations (create user, reset password, grant role)",
	}
	cmd.AddCommand(adminCreateUserCmd())
	cmd.AddCommand(adminResetPasswordCmd())
	cmd.AddCommand(adminGrantRoleCmd())
	cmd.AddCommand(adminListUsersCmd())
	return cmd
}

func openStoreFromEnv() (store.Store, error) {
	env := config.LoadEnv()
	homeDir, _ := config.HomeDir()
	return store.New(&store.StorageConfig{
		Type:        store.StorageType(env.Storage.Type),
		DSN:         env.Storage.DSN,
		AutoMigrate: true,
	}, homeDir)
}

func adminCreateUserCmd() *cobra.Command {
	var username, email, password, displayName, role string
	cmd := &cobra.Command{
		Use:   "create-user",
		Short: "Create a new user account",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, err := users.NewAccounts(st)
			if err != nil {
				return err
			}
			if role == "" {
				role = users.RoleUser
			}
			acct, err := accts.Create(context.Background(), users.CreateInput{
				Username:    username,
				Email:       email,
				Password:    password,
				DisplayName: displayName,
				Role:        role,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created user %s (%s) role=%s id=%s\n", acct.Username, acct.Email, acct.Role, acct.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "username (required)")
	cmd.Flags().StringVar(&email, "email", "", "email (required)")
	cmd.Flags().StringVar(&password, "password", "", "password (required)")
	cmd.Flags().StringVar(&displayName, "display-name", "", "display name")
	cmd.Flags().StringVar(&role, "role", "user", "'super_admin' or 'user'")
	cmd.MarkFlagRequired("username")
	cmd.MarkFlagRequired("email")
	cmd.MarkFlagRequired("password")
	return cmd
}

func adminResetPasswordCmd() *cobra.Command {
	var login, password string
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Reset a user's password by username or email",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, _ := users.NewAccounts(st)
			rec, err := st.GetUserByLogin(context.Background(), login)
			if err != nil {
				return err
			}
			if err := accts.SetPassword(context.Background(), rec.ID, password); err != nil {
				return err
			}
			fmt.Printf("password reset for %s\n", rec.Username)
			return nil
		},
	}
	cmd.Flags().StringVar(&login, "user", "", "username or email (required)")
	cmd.Flags().StringVar(&password, "password", "", "new password (required)")
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("password")
	return cmd
}

func adminGrantRoleCmd() *cobra.Command {
	var login, role string
	cmd := &cobra.Command{
		Use:   "grant-role",
		Short: "Change a user's role",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, _ := users.NewAccounts(st)
			rec, err := st.GetUserByLogin(context.Background(), login)
			if err != nil {
				return err
			}
			if _, err := accts.Update(context.Background(), rec.ID, "", role, "", nil); err != nil {
				return err
			}
			fmt.Printf("role for %s set to %s\n", rec.Username, role)
			return nil
		},
	}
	cmd.Flags().StringVar(&login, "user", "", "username or email (required)")
	cmd.Flags().StringVar(&role, "role", "", "'super_admin' or 'user' (required)")
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("role")
	return cmd
}

func adminListUsersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-users",
		Short: "List all user accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, _ := users.NewAccounts(st)
			list, err := accts.List(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("%-20s %-30s %-15s %-10s %s\n", "USERNAME", "EMAIL", "ROLE", "STATUS", "ID")
			for _, u := range list {
				fmt.Printf("%-20s %-30s %-15s %-10s %s\n", u.Username, u.Email, u.Role, u.Status, u.ID)
			}
			return nil
		},
	}
}
